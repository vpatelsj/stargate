package controller

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/vpatelsj/stargate/pkg/tailscale"
)

// RouteSyncReconciler reconciles Azure route tables and Tailscale routes
// when Kubernetes nodes join or leave the cluster.
type RouteSyncReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Logger *slog.Logger

	// Azure configuration
	SubscriptionID       string
	AKSResourceGroup     string // Resource group containing the AKS managed infrastructure (MC_*)
	ClusterResourceGroup string // Resource group containing the AKS cluster
	ClusterName          string // AKS cluster name
	RouteTableName       string // Route table name for worker routes (stargate-workers-rt)
	RouterRouteTableName string // Route table name for router subnet return traffic (stargate-router-rt)
	RouterSubnetName     string // Subnet where the Tailscale router lives
	AKSSubnetName        string // AKS node subnet name
	VNetName             string // AKS VNet name (auto-discovered if empty)

	// DC Configuration
	DCSubnetCIDR     string // DC subnet CIDR to route (e.g., 10.50.0.0/16)
	DCPodCIDR        string // DC pod CIDR range (e.g., 10.244.50.0/20)
	DCResourceGroup  string // DC resource group name
	DCRouteTableName string // DC route table name

	// Router IPs
	AKSRouterIP       string // AKS router private IP (e.g., 10.237.0.4)
	AKSRouterTSIP     string // AKS router Tailscale IP
	DCRouterTSIP      string // DC router Tailscale IP
	SSHPrivateKeyPath string // Path to SSH private key for router access

	// Tailscale configuration
	TailscaleAPIKey       string
	TailscaleClientID     string
	TailscaleClientSecret string
	TailnetName           string

	// Azure clients (initialized lazily)
	routeTableClient *armnetwork.RouteTablesClient
	routesClient     *armnetwork.RoutesClient
	subnetsClient    *armnetwork.SubnetsClient
	vmssClient       *armcompute.VirtualMachineScaleSetsClient
	vmssVMsClient    *armcompute.VirtualMachineScaleSetVMsClient
	nicClient        *armnetwork.InterfacesClient
	tsClient         *tailscale.Client

	// Runtime state
	initialized     bool
	lastSync        time.Time
	aksRouterIP     string // Cached AKS router IP
	discoveredVNet  string // Auto-discovered VNet name
	sshClientConfig *ssh.ClientConfig
}

// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get

// Reconcile handles Node events and syncs routes accordingly.
// When a new node joins, it ensures the Azure route table has a route for its pod CIDR.
func (r *RouteSyncReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("node", req.Name)

	// Skip if not configured
	if r.SubscriptionID == "" || r.AKSResourceGroup == "" || r.RouteTableName == "" {
		return ctrl.Result{}, nil
	}

	// Initialize Azure clients on first run
	if err := r.ensureInitialized(ctx); err != nil {
		logger.Error(err, "Failed to initialize Azure clients")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Fetch the Node
	var node corev1.Node
	if err := r.Get(ctx, req.NamespacedName, &node); err != nil {
		// Node was deleted - we could clean up routes here
		// For now, just log and ignore (routes to deleted nodes are harmless)
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Node deleted, route cleanup not implemented yet")
		return ctrl.Result{}, nil
	}

	// Get the node's pod CIDR
	podCIDR := node.Spec.PodCIDR
	if podCIDR == "" && isAKSNode(&node) {
		// AKS nodes with Azure CNI Overlay don't set spec.podCIDR
		// The pod CIDR is in CiliumNode.spec.ipam.podCIDRs instead
		logger.V(1).Info("AKS node has no spec.podCIDR (Azure CNI Overlay mode), fetching from CiliumNode")

		// Fetch podCIDR from CiliumNode
		ciliumPodCIDR, err := r.getCiliumNodePodCIDR(ctx, node.Name)
		if err != nil {
			logger.V(1).Info("Could not get CiliumNode podCIDR, will retry", "error", err.Error())
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		if ciliumPodCIDR == "" {
			logger.V(1).Info("CiliumNode has no podCIDR yet, will retry")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
		podCIDR = ciliumPodCIDR
		logger.Info("Got podCIDR from CiliumNode", "podCIDR", podCIDR)
	}
	if podCIDR == "" {
		logger.Info("Node has no pod CIDR yet, will retry")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Get the node's internal IP (next hop for routes)
	nodeIP := getNodeInternalIP(&node)
	if nodeIP == "" {
		logger.Info("Node has no internal IP yet, will retry")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// Check if this is a stargate DC worker node
	if isStargateWorker(&node) {
		// Stargate DC worker - add routes for traffic TO this node
		logger.Info("Ensuring routes for DC worker node", "podCIDR", podCIDR, "nodeIP", nodeIP)
		if err := r.ensureRouteForNode(ctx, &node, podCIDR, nodeIP); err != nil {
			logger.Error(err, "Failed to ensure route for DC worker node")
			return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
		}
	} else if isAKSNode(&node) {
		// AKS node - add route to router route table for return traffic FROM DC workers
		logger.Info("Ensuring router route for AKS node", "podCIDR", podCIDR, "nodeIP", nodeIP)
		if r.RouterSubnetName != "" && r.VNetName != "" {
			if err := r.ensureRouterRouteForAKSNode(ctx, node.Name, podCIDR, nodeIP); err != nil {
				logger.Error(err, "Failed to ensure router route for AKS node")
				// Don't requeue - this is not critical for DC workers
			}
		}

		// Update AKS router Tailscale routes to include this node's pod CIDR
		if r.tsClient != nil && r.AKSRouterTSIP != "" {
			if err := r.updateAKSRouterTailscaleRoutes(ctx, podCIDR); err != nil {
				logger.Error(err, "Failed to update AKS router Tailscale routes")
			} else {
				logger.Info("AKS router Tailscale routes updated", "podCIDR", podCIDR)
			}
		}
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager
func (r *RouteSyncReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		Complete(r)
}

// ensureInitialized lazily initializes Azure and Tailscale clients
func (r *RouteSyncReconciler) ensureInitialized(ctx context.Context) error {
	if r.initialized {
		return nil
	}

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return fmt.Errorf("create Azure credential: %w", err)
	}

	r.routeTableClient, err = armnetwork.NewRouteTablesClient(r.SubscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create route table client: %w", err)
	}

	r.routesClient, err = armnetwork.NewRoutesClient(r.SubscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create routes client: %w", err)
	}

	r.subnetsClient, err = armnetwork.NewSubnetsClient(r.SubscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create subnets client: %w", err)
	}

	r.vmssClient, err = armcompute.NewVirtualMachineScaleSetsClient(r.SubscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create vmss client: %w", err)
	}

	r.vmssVMsClient, err = armcompute.NewVirtualMachineScaleSetVMsClient(r.SubscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create vmss vms client: %w", err)
	}

	r.nicClient, err = armnetwork.NewInterfacesClient(r.SubscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create nic client: %w", err)
	}

	// Initialize Tailscale client - prefer OAuth, fall back to API key
	if r.TailscaleClientID != "" && r.TailscaleClientSecret != "" {
		r.tsClient, err = tailscale.NewClientWithOAuth(r.TailscaleClientID, r.TailscaleClientSecret, r.TailnetName, r.Logger)
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warn("Failed to create Tailscale OAuth client", "error", err)
			}
		} else if r.Logger != nil {
			r.Logger.Info("Tailscale OAuth client initialized")
		}
	} else if r.TailscaleAPIKey != "" {
		r.tsClient, err = tailscale.NewClient(r.TailscaleAPIKey, r.TailnetName, r.Logger)
		if err != nil {
			if r.Logger != nil {
				r.Logger.Warn("Failed to create Tailscale client", "error", err)
			}
		}
	}

	// Initialize SSH client config for router access
	if err := r.initSSHClientConfig(); err != nil {
		if r.Logger != nil {
			r.Logger.Warn("Failed to initialize SSH client config", "error", err)
		}
	}

	// Discover VNet name if not provided
	if r.VNetName == "" {
		if vnet, err := r.discoverVNetName(ctx); err == nil {
			r.VNetName = vnet
			r.discoveredVNet = vnet
			if r.Logger != nil {
				r.Logger.Info("Discovered VNet name", "vnet", vnet)
			}
		} else if r.Logger != nil {
			r.Logger.Warn("Failed to discover VNet name", "error", err)
		}
	}

	// Discover AKS router IP if not provided
	if r.AKSRouterIP == "" && r.RouterSubnetName != "" {
		if ip, err := r.discoverAKSRouterIP(ctx); err == nil {
			r.aksRouterIP = ip
			if r.Logger != nil {
				r.Logger.Info("Discovered AKS router IP", "ip", ip)
			}
		} else if r.Logger != nil {
			r.Logger.Warn("Failed to discover AKS router IP", "error", err)
		}
	} else {
		r.aksRouterIP = r.AKSRouterIP
	}

	// Ensure the router route table exists and is associated
	if r.RouterSubnetName != "" && r.VNetName != "" {
		if err := r.ensureRouterRouteTable(ctx); err != nil {
			if r.Logger != nil {
				r.Logger.Warn("Failed to ensure router route table", "error", err)
			}
		}
	}

	r.initialized = true
	return nil
}

// ensureRouteForNode creates or updates routes for a DC worker node's pod CIDR.
// This includes:
// 1. Azure route table entry (stargate-workers-rt) pointing to AKS router
// 2. Kernel route on AKS router via tailscale0
// 3. Kernel route on DC router to the worker's IP
// 4. Tailscale route advertisement updates
func (r *RouteSyncReconciler) ensureRouteForNode(ctx context.Context, node *corev1.Node, podCIDR, nodeIP string) error {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// 1. Add Azure route table entry for DC worker pod CIDR -> AKS router
	if err := r.ensureAzureRoute(ctx, node.Name, podCIDR); err != nil {
		return fmt.Errorf("ensure Azure route: %w", err)
	}
	logger.Info("Azure route created/updated", "node", node.Name, "podCIDR", podCIDR, "nextHop", r.aksRouterIP)

	// 2. Add kernel route on AKS router for pod CIDR via tailscale0
	if r.AKSRouterTSIP != "" && r.sshClientConfig != nil {
		if err := r.ensureAKSRouterKernelRoute(ctx, podCIDR); err != nil {
			logger.Warn("Failed to add AKS router kernel route", "error", err, "podCIDR", podCIDR)
		} else {
			logger.Info("AKS router kernel route added", "podCIDR", podCIDR)
		}
	}

	// 3. Add kernel route on DC router for pod CIDR -> worker IP
	if r.DCRouterTSIP != "" && r.sshClientConfig != nil {
		if err := r.ensureDCRouterKernelRoute(ctx, podCIDR, nodeIP); err != nil {
			logger.Warn("Failed to add DC router kernel route", "error", err, "podCIDR", podCIDR, "nodeIP", nodeIP)
		} else {
			logger.Info("DC router kernel route added", "podCIDR", podCIDR, "nextHop", nodeIP)
		}
	}

	// 4. Update Tailscale route advertisements on DC router
	if r.tsClient != nil && r.DCRouterTSIP != "" {
		if err := r.updateDCRouterTailscaleRoutes(ctx, podCIDR); err != nil {
			logger.Warn("Failed to update DC router Tailscale routes", "error", err)
		} else {
			logger.Info("DC router Tailscale routes updated")
		}
	}

	return nil
}

// ensureAzureRoute creates/updates Azure route table entry
func (r *RouteSyncReconciler) ensureAzureRoute(ctx context.Context, nodeName, podCIDR string) error {
	// Route name: safe version of node name
	routeName := fmt.Sprintf("stargate-%s", sanitizeRouteName(nodeName))

	// Determine next hop - use AKS router if available, otherwise direct to node
	nextHop := r.aksRouterIP
	if nextHop == "" {
		return fmt.Errorf("AKS router IP not configured or discovered")
	}

	// Create or update the route
	poller, err := r.routesClient.BeginCreateOrUpdate(ctx, r.AKSResourceGroup, r.RouteTableName, routeName,
		armnetwork.Route{
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    to.Ptr(podCIDR),
				NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
				NextHopIPAddress: to.Ptr(nextHop),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("begin create route: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("create route: %w", err)
	}

	return nil
}

// ensureRouterRouteTable ensures the stargate-router-rt exists and is associated with the router subnet.
// This route table handles return traffic from DC workers back to AKS pods.
func (r *RouteSyncReconciler) ensureRouterRouteTable(ctx context.Context) error {
	if r.RouterSubnetName == "" || r.VNetName == "" {
		return fmt.Errorf("router subnet or VNet name not configured")
	}

	routeTableName := r.RouterRouteTableName
	if routeTableName == "" {
		routeTableName = "stargate-router-rt"
	}

	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Check if route table exists
	_, err := r.routeTableClient.Get(ctx, r.AKSResourceGroup, routeTableName, nil)
	if err != nil {
		// Create route table if it doesn't exist
		logger.Info("Creating router route table", "name", routeTableName)
		poller, err := r.routeTableClient.BeginCreateOrUpdate(ctx, r.AKSResourceGroup, routeTableName,
			armnetwork.RouteTable{
				Location: to.Ptr("canadacentral"), // TODO: Make configurable
				Properties: &armnetwork.RouteTablePropertiesFormat{
					DisableBgpRoutePropagation: to.Ptr(false),
				},
			}, nil)
		if err != nil {
			return fmt.Errorf("begin create route table: %w", err)
		}
		if _, err = poller.PollUntilDone(ctx, nil); err != nil {
			return fmt.Errorf("create route table: %w", err)
		}
		logger.Info("Router route table created", "name", routeTableName)
	}

	// Associate route table with router subnet if not already
	subnet, err := r.subnetsClient.Get(ctx, r.AKSResourceGroup, r.VNetName, r.RouterSubnetName, nil)
	if err != nil {
		return fmt.Errorf("get router subnet: %w", err)
	}

	routeTableID := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Network/routeTables/%s",
		r.SubscriptionID, r.AKSResourceGroup, routeTableName)

	if subnet.Properties.RouteTable == nil || subnet.Properties.RouteTable.ID == nil || *subnet.Properties.RouteTable.ID != routeTableID {
		logger.Info("Associating router route table with subnet", "subnet", r.RouterSubnetName, "routeTable", routeTableName)
		subnet.Subnet.Properties.RouteTable = &armnetwork.RouteTable{
			ID: to.Ptr(routeTableID),
		}
		poller, err := r.subnetsClient.BeginCreateOrUpdate(ctx, r.AKSResourceGroup, r.VNetName, r.RouterSubnetName, subnet.Subnet, nil)
		if err != nil {
			return fmt.Errorf("begin associate route table: %w", err)
		}
		if _, err = poller.PollUntilDone(ctx, nil); err != nil {
			return fmt.Errorf("associate route table: %w", err)
		}
		logger.Info("Router route table associated with subnet")
	}

	return nil
}

// ensureRouterRouteForAKSNode adds a route in the router route table for an AKS node's pod CIDR.
// This enables return traffic from DC workers back to AKS pods.
func (r *RouteSyncReconciler) ensureRouterRouteForAKSNode(ctx context.Context, nodeName, podCIDR, nodeIP string) error {
	routeTableName := r.RouterRouteTableName
	if routeTableName == "" {
		routeTableName = "stargate-router-rt"
	}

	routeName := fmt.Sprintf("aks-node-%s", sanitizeRouteName(nodeName))

	// First check if a route for this podCIDR already exists (might have different name)
	routeTable, err := r.routeTableClient.Get(ctx, r.AKSResourceGroup, routeTableName, nil)
	if err == nil && routeTable.Properties != nil && routeTable.Properties.Routes != nil {
		for _, existingRoute := range routeTable.Properties.Routes {
			if existingRoute.Properties != nil && existingRoute.Properties.AddressPrefix != nil &&
				*existingRoute.Properties.AddressPrefix == podCIDR {
				// Route exists - check if it has the right next hop
				if existingRoute.Properties.NextHopIPAddress != nil &&
					*existingRoute.Properties.NextHopIPAddress == nodeIP {
					// Route already exists with correct config
					return nil
				}
				// Route exists but with wrong next hop or different name - delete it first
				if existingRoute.Name != nil && *existingRoute.Name != routeName {
					delPoller, err := r.routesClient.BeginDelete(ctx, r.AKSResourceGroup, routeTableName, *existingRoute.Name, nil)
					if err == nil {
						_, _ = delPoller.PollUntilDone(ctx, nil)
					}
				}
				break
			}
		}
	}

	poller, err := r.routesClient.BeginCreateOrUpdate(ctx, r.AKSResourceGroup, routeTableName, routeName,
		armnetwork.Route{
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    to.Ptr(podCIDR),
				NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
				NextHopIPAddress: to.Ptr(nodeIP),
			},
		}, nil)
	if err != nil {
		return fmt.Errorf("begin create router route: %w", err)
	}

	if _, err = poller.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("create router route: %w", err)
	}

	if r.Logger != nil {
		r.Logger.Info("Router route created for AKS node", "node", nodeName, "podCIDR", podCIDR, "nextHop", nodeIP)
	}

	return nil
}

// SyncAllAKSNodeRoutes synchronizes routes for all AKS nodes in the VMSS.
// This is called periodically to ensure routes are up-to-date.
func (r *RouteSyncReconciler) SyncAllAKSNodeRoutes(ctx context.Context) error {
	logger := r.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := r.ensureInitialized(ctx); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}

	// Find all VMSS in the AKS resource group
	pager := r.vmssClient.NewListPager(r.AKSResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list vmss: %w", err)
		}

		for _, vmss := range page.Value {
			if vmss.Name == nil {
				continue
			}

			// Skip non-AKS VMSS (AKS node pools start with "aks-")
			if !strings.HasPrefix(*vmss.Name, "aks-") {
				continue
			}

			logger.Info("Syncing routes for VMSS", "vmss", *vmss.Name)

			// Get all VMs in this VMSS
			vmPager := r.vmssVMsClient.NewListPager(r.AKSResourceGroup, *vmss.Name, nil)
			for vmPager.More() {
				vmPage, err := vmPager.NextPage(ctx)
				if err != nil {
					return fmt.Errorf("list vmss vms: %w", err)
				}

				for _, vm := range vmPage.Value {
					if vm.Name == nil || vm.InstanceID == nil {
						continue
					}

					// Get the VM's IP address from its NIC
					ip, err := r.getVMSSVMPrivateIP(ctx, r.AKSResourceGroup, *vmss.Name, *vm.InstanceID)
					if err != nil {
						logger.Warn("Failed to get VM IP", "vm", *vm.Name, "error", err)
						continue
					}

					// Derive pod CIDR from the VMSS instance index
					// AKS Azure CNI Overlay assigns sequential /24 CIDRs
					instanceID := *vm.InstanceID
					podCIDR := fmt.Sprintf("10.244.%s.0/24", instanceID)

					routeName := fmt.Sprintf("aks-node-%s-%s", *vmss.Name, *vm.InstanceID)

					logger.Info("Ensuring route", "route", routeName, "podCIDR", podCIDR, "nextHop", ip)

					poller, err := r.routesClient.BeginCreateOrUpdate(ctx, r.AKSResourceGroup, r.RouteTableName, routeName,
						armnetwork.Route{
							Properties: &armnetwork.RoutePropertiesFormat{
								AddressPrefix:    to.Ptr(podCIDR),
								NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
								NextHopIPAddress: to.Ptr(ip),
							},
						}, nil)
					if err != nil {
						logger.Warn("Failed to begin create route", "route", routeName, "error", err)
						continue
					}

					if _, err := poller.PollUntilDone(ctx, nil); err != nil {
						logger.Warn("Failed to create route", "route", routeName, "error", err)
						continue
					}
				}
			}
		}
	}

	r.lastSync = time.Now()
	return nil
}

// getVMSSVMPrivateIP gets the private IP of a VMSS VM instance
func (r *RouteSyncReconciler) getVMSSVMPrivateIP(ctx context.Context, resourceGroup, vmssName, instanceID string) (string, error) {
	// Get the network interface for this VM instance
	nicName := fmt.Sprintf("%s_%s", vmssName, instanceID)

	pager := r.nicClient.NewListVirtualMachineScaleSetVMNetworkInterfacesPager(resourceGroup, vmssName, instanceID, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("list nics for %s: %w", nicName, err)
		}

		for _, nic := range page.Value {
			if nic.Properties == nil || nic.Properties.IPConfigurations == nil {
				continue
			}
			for _, ipConfig := range nic.Properties.IPConfigurations {
				if ipConfig.Properties == nil || ipConfig.Properties.PrivateIPAddress == nil {
					continue
				}
				return *ipConfig.Properties.PrivateIPAddress, nil
			}
		}
	}

	return "", fmt.Errorf("no private IP found for VMSS VM %s/%s", vmssName, instanceID)
}

// EnableTailscaleRoutes enables routes for a Tailscale router device
func (r *RouteSyncReconciler) EnableTailscaleRoutes(ctx context.Context, hostname string) error {
	if r.tsClient == nil {
		return fmt.Errorf("Tailscale client not configured")
	}

	device, err := r.tsClient.EnsureRouterSetup(ctx, hostname)
	if err != nil {
		return fmt.Errorf("ensure router setup: %w", err)
	}

	if r.Logger != nil {
		r.Logger.Info("Tailscale routes enabled",
			"hostname", hostname,
			"advertisedRoutes", device.AdvertisedRoutes,
			"enabledRoutes", device.EnabledRoutes)
	}

	return nil
}

// isStargateWorker checks if a node is a stargate DC worker
func isStargateWorker(node *corev1.Node) bool {
	// Check for stargate-specific labels
	if _, ok := node.Labels["stargate.io/role"]; ok {
		return true
	}
	// Check for DC worker naming convention
	if strings.Contains(node.Name, "dc-") || strings.Contains(node.Name, "worker") {
		return true
	}
	return false
}

// isAKSNode checks if a node is an AKS node (from VMSS)
func isAKSNode(node *corev1.Node) bool {
	// Check for AKS node naming convention (aks-nodepool...)
	if strings.HasPrefix(node.Name, "aks-") {
		return true
	}
	// Check for VMSS naming pattern
	if strings.Contains(node.Name, "vmss") {
		return true
	}
	return false
}

// getNodeInternalIP returns the internal IP of a node
func getNodeInternalIP(node *corev1.Node) string {
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			return addr.Address
		}
	}
	return ""
}

// sanitizeRouteName converts a node name to a valid Azure route name
func sanitizeRouteName(name string) string {
	// Azure route names: alphanumeric, hyphens, underscores, periods
	// Max 80 characters
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, ":", "-")
	if len(name) > 70 {
		name = name[:70]
	}
	return name
}

// initSSHClientConfig initializes the SSH client configuration
func (r *RouteSyncReconciler) initSSHClientConfig() error {
	keyPath := r.SSHPrivateKeyPath
	if keyPath == "" {
		keyPath = filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa")
	}

	key, err := os.ReadFile(keyPath)
	if err != nil {
		return fmt.Errorf("read SSH key: %w", err)
	}

	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return fmt.Errorf("parse SSH key: %w", err)
	}

	r.sshClientConfig = &ssh.ClientConfig{
		User: "ubuntu",
		Auth: []ssh.AuthMethod{
			ssh.PublicKeys(signer),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	return nil
}

// discoverAKSRouterIP discovers the AKS router IP from the router subnet
func (r *RouteSyncReconciler) discoverAKSRouterIP(ctx context.Context) (string, error) {
	// List all NICs and find one in the router subnet
	nicPager := r.nicClient.NewListPager(r.AKSResourceGroup, nil)
	for nicPager.More() {
		page, err := nicPager.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("list NICs: %w", err)
		}

		for _, nic := range page.Value {
			if nic.Name == nil || nic.Properties == nil {
				continue
			}
			// Look for router NIC by naming convention
			if strings.Contains(*nic.Name, "router") || strings.Contains(*nic.Name, "stargate") {
				for _, ipConfig := range nic.Properties.IPConfigurations {
					if ipConfig.Properties != nil && ipConfig.Properties.PrivateIPAddress != nil {
						return *ipConfig.Properties.PrivateIPAddress, nil
					}
				}
			}
		}
	}
	return "", fmt.Errorf("could not find AKS router NIC")
}

// discoverVNetName discovers the VNet name in the AKS resource group
func (r *RouteSyncReconciler) discoverVNetName(ctx context.Context) (string, error) {
	// Create VNet client
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return "", fmt.Errorf("create credential: %w", err)
	}

	vnetClient, err := armnetwork.NewVirtualNetworksClient(r.SubscriptionID, cred, nil)
	if err != nil {
		return "", fmt.Errorf("create vnet client: %w", err)
	}

	// List VNets in resource group
	pager := vnetClient.NewListPager(r.AKSResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return "", fmt.Errorf("list vnets: %w", err)
		}

		for _, vnet := range page.Value {
			if vnet.Name != nil && strings.HasPrefix(*vnet.Name, "aks-vnet") {
				return *vnet.Name, nil
			}
		}

		// If no aks-vnet found, use first VNet
		if len(page.Value) > 0 && page.Value[0].Name != nil {
			return *page.Value[0].Name, nil
		}
	}

	return "", fmt.Errorf("no VNet found in resource group %s", r.AKSResourceGroup)
}

// runSSHCommand executes a command on a remote host via SSH
func (r *RouteSyncReconciler) runSSHCommand(host string, command string) (string, error) {
	if r.sshClientConfig == nil {
		return "", fmt.Errorf("SSH client not configured")
	}

	client, err := ssh.Dial("tcp", host+":22", r.sshClientConfig)
	if err != nil {
		return "", fmt.Errorf("SSH dial: %w", err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	if err := session.Run(command); err != nil {
		// Check if this is an "already exists" type error which is OK
		if strings.Contains(stderr.String(), "File exists") || strings.Contains(stderr.String(), "RTNETLINK answers: File exists") {
			return stdout.String(), nil
		}
		return "", fmt.Errorf("SSH run: %w (stderr: %s)", err, stderr.String())
	}

	return stdout.String(), nil
}

// ensureAKSRouterKernelRoute adds a kernel route on the AKS router for a pod CIDR via tailscale0
func (r *RouteSyncReconciler) ensureAKSRouterKernelRoute(ctx context.Context, podCIDR string) error {
	cmd := fmt.Sprintf("sudo ip route replace %s dev tailscale0", podCIDR)
	_, err := r.runSSHCommand(r.AKSRouterTSIP, cmd)
	return err
}

// ensureDCRouterKernelRoute adds a kernel route on the DC router for a pod CIDR to a worker IP
func (r *RouteSyncReconciler) ensureDCRouterKernelRoute(ctx context.Context, podCIDR, workerIP string) error {
	cmd := fmt.Sprintf("sudo ip route replace %s via %s", podCIDR, workerIP)
	_, err := r.runSSHCommand(r.DCRouterTSIP, cmd)
	return err
}

// updateDCRouterTailscaleRoutes updates Tailscale route advertisements on the DC router
func (r *RouteSyncReconciler) updateDCRouterTailscaleRoutes(ctx context.Context, newPodCIDR string) error {
	if r.tsClient == nil {
		return fmt.Errorf("Tailscale client not configured")
	}

	// Get DC router device
	devices, err := r.tsClient.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}

	var dcRouterDevice *tailscale.Device
	for _, d := range devices {
		if d.TailscaleIP == r.DCRouterTSIP || strings.Contains(d.Hostname, "dc-router") {
			dcRouterDevice = d
			break
		}
	}

	if dcRouterDevice == nil {
		return fmt.Errorf("DC router not found in Tailscale devices")
	}

	// Fetch current routes from the dedicated routes endpoint (more accurate than device list)
	currentRoutes, err := r.tsClient.GetDeviceRoutes(ctx, dcRouterDevice.ID)
	if err != nil {
		return fmt.Errorf("get device routes: %w", err)
	}

	// Build route list including the DC subnet and all known pod CIDRs
	routes := []string{r.DCSubnetCIDR}

	// Add existing enabled routes (using the accurate routes endpoint data)
	for _, route := range currentRoutes.EnabledRoutes {
		if route != newPodCIDR && route != r.DCSubnetCIDR {
			routes = append(routes, route)
		}
	}

	// Add the new pod CIDR
	routes = append(routes, newPodCIDR)

	// SSH to DC router to advertise routes
	routeList := strings.Join(routes, ",")
	cmd := fmt.Sprintf("sudo tailscale set --advertise-routes=%s", routeList)
	if _, err := r.runSSHCommand(r.DCRouterTSIP, cmd); err != nil {
		return fmt.Errorf("advertise routes: %w", err)
	}

	// Enable routes via Tailscale API
	if err := r.tsClient.EnableRoutes(ctx, dcRouterDevice.ID, routes); err != nil {
		return fmt.Errorf("enable routes: %w", err)
	}

	return nil
}

// updateAKSRouterTailscaleRoutes updates Tailscale route advertisements on the AKS router.
// The AKS router must advertise:
// - AKS node subnet (e.g., 10.224.0.0/16) so DC workers can reach AKS nodes
// - AKS pod CIDRs (e.g., 10.244.0.0/24, 10.244.1.0/24) for return traffic
func (r *RouteSyncReconciler) updateAKSRouterTailscaleRoutes(ctx context.Context, newPodCIDR string) error {
	if r.tsClient == nil {
		return fmt.Errorf("Tailscale client not configured")
	}

	// Get AKS router device
	devices, err := r.tsClient.ListDevices(ctx)
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}

	var aksRouterDevice *tailscale.Device
	for _, d := range devices {
		if d.TailscaleIP == r.AKSRouterTSIP || strings.Contains(d.Hostname, "router") && !strings.Contains(d.Hostname, "dc") {
			aksRouterDevice = d
			break
		}
	}

	if aksRouterDevice == nil {
		return fmt.Errorf("AKS router not found in Tailscale devices")
	}

	// Fetch current routes from the dedicated routes endpoint (more accurate than device list)
	currentRoutes, err := r.tsClient.GetDeviceRoutes(ctx, aksRouterDevice.ID)
	if err != nil {
		return fmt.Errorf("get device routes: %w", err)
	}

	// Build route list: AKS node subnet + all known AKS pod CIDRs
	// The AKS node subnet (10.224.0.0/16) allows DC router to reach AKS node IPs
	routes := []string{"10.224.0.0/16"} // TODO: Make configurable

	// Add existing enabled routes (using the accurate routes endpoint data)
	// IMPORTANT: Filter out broad pod CIDRs like 10.244.0.0/16 - these cause routing
	// conflicts on the DC router where it routes DC worker pods back through Tailscale
	// instead of locally. Only specific /24 pod CIDRs should be advertised.
	logger := log.FromContext(ctx)
	for _, route := range currentRoutes.EnabledRoutes {
		if route == newPodCIDR || route == "10.224.0.0/16" {
			continue // Will be added separately
		}
		if isBroadPodCIDR(route) {
			logger.Info("Filtering out broad pod CIDR from AKS router routes", "cidr", route)
			continue
		}
		routes = append(routes, route)
	}

	// Add the new pod CIDR
	routes = append(routes, newPodCIDR)

	// SSH to AKS router to advertise routes
	routeList := strings.Join(routes, ",")
	cmd := fmt.Sprintf("sudo tailscale set --advertise-routes=%s", routeList)
	if _, err := r.runSSHCommand(r.AKSRouterTSIP, cmd); err != nil {
		return fmt.Errorf("advertise routes: %w", err)
	}

	// Enable routes via Tailscale API
	if err := r.tsClient.EnableRoutes(ctx, aksRouterDevice.ID, routes); err != nil {
		return fmt.Errorf("enable routes: %w", err)
	}

	return nil
}

// getCiliumNodePodCIDR fetches the podCIDR from a CiliumNode resource.
// CiliumNode stores the pod CIDR in spec.ipam.podCIDRs for Azure CNI Overlay mode.
func (r *RouteSyncReconciler) getCiliumNodePodCIDR(ctx context.Context, nodeName string) (string, error) {
	// Define CiliumNode GVK
	ciliumNodeGVK := schema.GroupVersionKind{
		Group:   "cilium.io",
		Version: "v2",
		Kind:    "CiliumNode",
	}

	// Create unstructured object
	ciliumNode := &unstructured.Unstructured{}
	ciliumNode.SetGroupVersionKind(ciliumNodeGVK)

	// Fetch CiliumNode
	key := client.ObjectKey{Name: nodeName}
	if err := r.Get(ctx, key, ciliumNode); err != nil {
		return "", fmt.Errorf("get CiliumNode %s: %w", nodeName, err)
	}

	// Extract spec.ipam.podCIDRs
	spec, found, err := unstructured.NestedMap(ciliumNode.Object, "spec")
	if err != nil || !found {
		return "", fmt.Errorf("CiliumNode %s has no spec", nodeName)
	}

	ipam, found, err := unstructured.NestedMap(spec, "ipam")
	if err != nil || !found {
		return "", fmt.Errorf("CiliumNode %s has no spec.ipam", nodeName)
	}

	podCIDRs, found, err := unstructured.NestedStringSlice(ipam, "podCIDRs")
	if err != nil || !found || len(podCIDRs) == 0 {
		return "", fmt.Errorf("CiliumNode %s has no spec.ipam.podCIDRs", nodeName)
	}

	return podCIDRs[0], nil
}

// isBroadPodCIDR returns true if the given CIDR is a broad pod CIDR (e.g., /16 or larger)
// that should NOT be advertised via Tailscale. Only specific node pod CIDRs (/24) should
// be advertised to avoid routing conflicts where DC routers route DC worker pods back
// through Tailscale instead of locally.
func isBroadPodCIDR(cidr string) bool {
	// Check if this is a pod CIDR in the typical 10.244.x.x range
	if !strings.HasPrefix(cidr, "10.244.") {
		return false
	}
	// A broad CIDR ends with /16, /12, /8, etc. (prefix length < 24)
	// We only want to allow /24 or smaller for pod CIDRs
	if strings.HasSuffix(cidr, "/16") || strings.HasSuffix(cidr, "/12") || strings.HasSuffix(cidr, "/8") {
		return true
	}
	return false
}
