package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/vpatelsj/stargate/pkg/infra/providers"
	"github.com/vpatelsj/stargate/pkg/tailscale"
)

// Config holds Azure-specific settings for provisioning base VMs (no Kubernetes bootstrap).
type Config struct {
	SubscriptionID   string
	Location         string
	Zone             string
	ResourceGroup    string
	VNetName         string
	VNetCIDR         string
	SubnetName       string
	SubnetCIDR       string
	VMSize           string
	AdminUsername    string
	SSHPublicKeyPath string
	TailscaleAuthKey string

	// AKS router config (optional) - for provisioning a router in existing AKS VNet
	AKSRouter *providers.AKSRouterConfig
}

// Provider provisions Azure VMs with base cloud-init (tailscale only) and returns node addresses.
type Provider struct {
	cfg              Config
	logger           *slog.Logger
	rgClient         *armresources.ResourceGroupsClient
	vnetClient       *armnetwork.VirtualNetworksClient
	subnetClient     *armnetwork.SubnetsClient
	pipClient        *armnetwork.PublicIPAddressesClient
	nicClient        *armnetwork.InterfacesClient
	vmClient         *armcompute.VirtualMachinesClient
	routeTableClient *armnetwork.RouteTablesClient
	routesClient     *armnetwork.RoutesClient
	aksClient        *armcontainerservice.ManagedClustersClient
	vmssClient       *armcompute.VirtualMachineScaleSetsClient
	vmssVMsClient    *armcompute.VirtualMachineScaleSetVMsClient
	tsClient         *tailscale.Client
}

// route holds a CIDR and the next hop IP for route table construction.
type route struct {
	Prefix  string
	NextHop string
}

// NewProvider initializes Azure clients.
func NewProvider(ctx context.Context, cfg Config) (*Provider, error) {
	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure CLI credential: %w", err)
	}

	rgClient, err := armresources.NewResourceGroupsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("resource groups client: %w", err)
	}

	vnetClient, err := armnetwork.NewVirtualNetworksClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("vnet client: %w", err)
	}

	subnetClient, err := armnetwork.NewSubnetsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("subnet client: %w", err)
	}

	pipClient, err := armnetwork.NewPublicIPAddressesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("pip client: %w", err)
	}

	nicClient, err := armnetwork.NewInterfacesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("nic client: %w", err)
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("vm client: %w", err)
	}

	routeTableClient, err := armnetwork.NewRouteTablesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("route table client: %w", err)
	}

	routesClient, err := armnetwork.NewRoutesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("routes client: %w", err)
	}

	aksClient, err := armcontainerservice.NewManagedClustersClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("aks client: %w", err)
	}

	vmssClient, err := armcompute.NewVirtualMachineScaleSetsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("vmss client: %w", err)
	}

	vmssVMsClient, err := armcompute.NewVirtualMachineScaleSetVMsClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("vmss vms client: %w", err)
	}

	// Create Tailscale client (optional - will be nil if no API key configured)
	logger := slog.Default()
	var tsClient *tailscale.Client
	if os.Getenv("TAILSCALE_API_KEY") != "" {
		tsClient, err = tailscale.NewClient("", "", logger)
		if err != nil {
			logger.Warn("failed to create Tailscale client, route approval will be manual", "error", err)
		}
	}

	return &Provider{
		cfg:              cfg,
		logger:           logger,
		rgClient:         rgClient,
		vnetClient:       vnetClient,
		subnetClient:     subnetClient,
		pipClient:        pipClient,
		nicClient:        nicClient,
		vmClient:         vmClient,
		routeTableClient: routeTableClient,
		routesClient:     routesClient,
		aksClient:        aksClient,
		vmssClient:       vmssClient,
		vmssVMsClient:    vmssVMsClient,
		tsClient:         tsClient,
	}, nil
}

// CreateNodes provisions the requested VMs and returns their addresses.
// Creates router first to get its private IP for workers.
func (p *Provider) CreateNodes(ctx context.Context, specs []providers.NodeSpec) ([]providers.NodeInfo, error) {
	// Check if we're only creating an AKS router (no DC infrastructure needed)
	onlyAKSRouter := len(specs) == 1 && specs[0].Role == providers.RoleAKSRouter

	var subnetID string
	if !onlyAKSRouter {
		// Only set up DC infrastructure if we're not just creating an AKS router
		if err := p.ensureResourceGroup(ctx); err != nil {
			return nil, err
		}
		if err := p.ensureVNet(ctx); err != nil {
			return nil, err
		}
		var err error
		subnetID, err = p.ensureSubnet(ctx)
		if err != nil {
			return nil, err
		}
	}

	sshKey, err := os.ReadFile(p.cfg.SSHPublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read SSH public key: %w", err)
	}

	// Separate router, workers, and AKS router specs
	var routerSpecs, workerSpecs, aksRouterSpecs []providers.NodeSpec
	for _, spec := range specs {
		role := spec.Role
		if role == "" {
			role = providers.RoleWorker
		}
		switch role {
		case providers.RoleRouter:
			routerSpecs = append(routerSpecs, spec)
		case providers.RoleAKSRouter:
			aksRouterSpecs = append(aksRouterSpecs, spec)
		default:
			workerSpecs = append(workerSpecs, spec)
		}
	}

	var nodes []providers.NodeInfo
	var routerIP string

	// Create AKS router first if specified (uses different VNet)
	for range aksRouterSpecs {
		if p.cfg.AKSRouter == nil {
			return nil, fmt.Errorf("AKS router spec provided but no AKS router config in provider")
		}
		aksNode, err := p.createAKSRouter(ctx, string(sshKey))
		if err != nil {
			return nil, fmt.Errorf("create AKS router: %w", err)
		}
		nodes = append(nodes, aksNode)
	}

	// Create DC routers
	for _, spec := range routerSpecs {
		nicName := fmt.Sprintf("%s-nic", spec.Name)
		pipName := fmt.Sprintf("%s-pip", spec.Name)

		pipID, err := p.ensurePublicIP(ctx, pipName)
		if err != nil {
			return nil, fmt.Errorf("public IP %s: %w", pipName, err)
		}

		nicID, err := p.ensureNICWithIPForwarding(ctx, nicName, subnetID, pipID)
		if err != nil {
			return nil, fmt.Errorf("NIC %s: %w", nicName, err)
		}

		cloudInit, err := buildRouterCloudInit(spec.Name, p.cfg.AdminUsername, string(sshKey), p.cfg.TailscaleAuthKey, p.cfg.SubnetCIDR)
		if err != nil {
			return nil, err
		}

		if err := p.ensureVM(ctx, spec.Name, nicID, cloudInit, string(sshKey)); err != nil {
			return nil, fmt.Errorf("VM %s: %w", spec.Name, err)
		}

		pubIP, err := p.getPublicIPAddress(ctx, pipName)
		if err != nil {
			return nil, fmt.Errorf("get public IP %s: %w", pipName, err)
		}

		privIP, err := p.getPrivateIP(ctx, nicName)
		if err != nil {
			return nil, fmt.Errorf("get private IP %s: %w", nicName, err)
		}

		routerIP = privIP // Save for workers

		// Enable routes in Tailscale for this router (via API if configured)
		if err := p.ensureTailscaleRoutes(ctx, spec.Name); err != nil {
			p.logger.Warn("failed to auto-approve Tailscale routes, manual approval may be required",
				"router", spec.Name, "error", err)
		}

		nodes = append(nodes, providers.NodeInfo{
			Name:        spec.Name,
			Role:        providers.RoleRouter,
			PublicIP:    pubIP,
			PrivateIP:   privIP,
			TailnetFQDN: spec.Name,
		})
	}

	// Create route table for workers to reach Tailscale network and AKS CIDRs via router
	// If AKS router is configured, also add routes for AKS CIDRs
	var aksRouteCIDRs []string
	if p.cfg.AKSRouter != nil && len(p.cfg.AKSRouter.RouteCIDRs) > 0 {
		aksRouteCIDRs = p.cfg.AKSRouter.RouteCIDRs
	}

	// Build per-worker pod CIDR routes once we have private IPs
	workerRoutes := make([]route, 0, len(workerSpecs))

	// Create workers with router IP for Tailscale routing
	for _, spec := range workerSpecs {
		nicName := fmt.Sprintf("%s-nic", spec.Name)

		nicID, err := p.ensureNIC(ctx, nicName, subnetID, "")
		if err != nil {
			return nil, fmt.Errorf("NIC %s: %w", nicName, err)
		}

		cloudInit, err := buildWorkerCloudInit(spec.Name, p.cfg.AdminUsername, string(sshKey), routerIP, aksRouteCIDRs)
		if err != nil {
			return nil, err
		}

		if err := p.ensureVM(ctx, spec.Name, nicID, cloudInit, string(sshKey)); err != nil {
			return nil, fmt.Errorf("VM %s: %w", spec.Name, err)
		}

		privIP, err := p.getPrivateIP(ctx, nicName)
		if err != nil {
			return nil, fmt.Errorf("get private IP %s: %w", nicName, err)
		}

		podCIDR := derivePodCIDR(privIP)

		nodes = append(nodes, providers.NodeInfo{
			Name:      spec.Name,
			Role:      providers.RoleWorker,
			PublicIP:  "",
			PrivateIP: privIP,
			RouterIP:  routerIP,
			PodCIDR:   podCIDR,
		})

		if podCIDR != "" {
			workerRoutes = append(workerRoutes, route{Prefix: podCIDR, NextHop: privIP})
		}
	}

	if routerIP != "" && len(workerSpecs) > 0 {
		if err := p.ensureRouteTable(ctx, subnetID, routerIP, aksRouteCIDRs, workerRoutes); err != nil {
			return nil, fmt.Errorf("route table: %w", err)
		}
	}

	return nodes, nil
}

// createAKSRouter provisions a Tailscale subnet router in an existing AKS VNet.
// This enables worker nodes to reach the AKS control plane through the Tailscale mesh.
func (p *Provider) createAKSRouter(ctx context.Context, sshKey string) (providers.NodeInfo, error) {
	cfg := p.cfg.AKSRouter
	if cfg == nil {
		return providers.NodeInfo{}, fmt.Errorf("AKS router config is nil")
	}

	// Ensure the subnet exists in the AKS VNet (in the AKS resource group)
	subnetID, err := p.ensureSubnetInVNet(ctx, cfg.ResourceGroup, cfg.VNetName, cfg.SubnetName, cfg.SubnetCIDR)
	if err != nil {
		return providers.NodeInfo{}, fmt.Errorf("ensure AKS router subnet: %w", err)
	}

	nicName := fmt.Sprintf("%s-nic", cfg.Name)
	pipName := fmt.Sprintf("%s-pip", cfg.Name)

	// Create public IP in the AKS resource group
	pipID, err := p.ensurePublicIPInRG(ctx, cfg.ResourceGroup, pipName)
	if err != nil {
		return providers.NodeInfo{}, fmt.Errorf("public IP %s: %w", pipName, err)
	}

	// Create NIC with IP forwarding in the AKS resource group
	nicID, err := p.ensureNICWithIPForwardingInRG(ctx, cfg.ResourceGroup, nicName, subnetID, pipID)
	if err != nil {
		return providers.NodeInfo{}, fmt.Errorf("NIC %s: %w", nicName, err)
	}

	// Build cloud-init that advertises all AKS CIDRs to Tailscale and sets up API proxy
	var cloudInit string
	if len(cfg.RouteCIDRs) > 0 {
		cloudInit, err = buildAKSRouterCloudInit(cfg.Name, p.cfg.AdminUsername, sshKey, p.cfg.TailscaleAuthKey, cfg.RouteCIDRs, cfg.APIServerFQDN)
	} else {
		// Fallback to single VNet CIDR
		cloudInit, err = buildRouterCloudInit(cfg.Name, p.cfg.AdminUsername, sshKey, p.cfg.TailscaleAuthKey, cfg.VNetCIDR)
	}
	if err != nil {
		return providers.NodeInfo{}, err
	}

	// Create VM in the AKS resource group
	if err := p.ensureVMInRG(ctx, cfg.ResourceGroup, cfg.Name, nicID, cloudInit, sshKey); err != nil {
		return providers.NodeInfo{}, fmt.Errorf("VM %s: %w", cfg.Name, err)
	}

	pubIP, err := p.getPublicIPAddressInRG(ctx, cfg.ResourceGroup, pipName)
	if err != nil {
		return providers.NodeInfo{}, fmt.Errorf("get public IP %s: %w", pipName, err)
	}

	privIP, err := p.getPrivateIPInRG(ctx, cfg.ResourceGroup, nicName)
	if err != nil {
		return providers.NodeInfo{}, fmt.Errorf("get private IP %s: %w", nicName, err)
	}

	// Set up route table in the AKS VNet for DC-bound traffic
	// Routes DC pod CIDRs (10.244.50-70.0/24) through this router
	if err := p.ensureAKSRouteTable(ctx, cfg, privIP); err != nil {
		return providers.NodeInfo{}, fmt.Errorf("AKS route table: %w", err)
	}

	// Enable routes in Tailscale for this router (via API if configured)
	// This auto-approves the advertised routes instead of requiring manual approval
	if err := p.ensureTailscaleRoutes(ctx, cfg.Name); err != nil {
		// Log but don't fail - routes can be approved manually
		p.logger.Warn("failed to auto-approve Tailscale routes, manual approval may be required",
			"router", cfg.Name, "error", err)
	}

	return providers.NodeInfo{
		Name:        cfg.Name,
		Role:        providers.RoleAKSRouter,
		PublicIP:    pubIP,
		PrivateIP:   privIP,
		TailnetFQDN: cfg.Name,
	}, nil
}

func baseCloudInitHeader(vmName, adminUser, sshPublicKey string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
users:
  - name: %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

package_update: true
package_upgrade: false

packages:
  - apt-transport-https
  - ca-certificates
  - curl
  - gnupg
  - lsb-release
`, vmName, adminUser, sshPublicKey)
}

// buildRouterCloudInit creates cloud-init for a DC router that:
// 1. Advertises the DC subnet and expected DC worker pod CIDRs to Tailscale
// 2. Sets up kernel routes for AKS node subnet and pod CIDRs via tailscale0
func buildRouterCloudInit(vmName, adminUser, sshPublicKey, tailscaleAuthKey, subnetCIDR string) (string, error) {
	if tailscaleAuthKey == "" {
		return "", fmt.Errorf("missing tailscale auth key for router %s", vmName)
	}

	// Advertise: DC subnet + expected DC worker pod CIDRs (10.244.50-70.0/24 range)
	// This enables AKS to reach DC worker pods
	advertiseRoutes := subnetCIDR
	for i := 50; i <= 70; i++ {
		advertiseRoutes += fmt.Sprintf(",10.244.%d.0/24", i)
	}

	cloudInit := baseCloudInitHeader(vmName, adminUser, sshPublicKey)
	cloudInit += fmt.Sprintf(`
write_files:
  - path: /tmp/configure-router.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -ex
      sysctl -w net.ipv4.ip_forward=1
      sed -i 's/^#*net.ipv4.ip_forward.*/net.ipv4.ip_forward=1/' /etc/sysctl.conf
      curl -fsSL https://tailscale.com/install.sh | sh
      tailscale up --authkey %s --hostname %s --advertise-routes=%s --accept-routes
      # MASQUERADE for LAN traffic going to Tailscale network
      iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE
      mkdir -p /etc/iptables
      iptables-save > /etc/iptables/rules.v4 || true
      
      # Add kernel routes for AKS node subnet and pod CIDRs via tailscale0
      # AKS node subnet is typically 10.224.0.0/16
      ip route add 10.224.0.0/16 dev tailscale0 2>/dev/null || true
      # AKS pod CIDRs are in 10.244.0-10.0/24 range (first 10 nodes)
      for i in $(seq 0 10); do
        ip route add 10.244.$i.0/24 dev tailscale0 2>/dev/null || true
      done
      
      # Persist routes for reboot - write script using echo to avoid YAML heredoc issues
      mkdir -p /etc/network/if-up.d
      echo '#!/bin/bash' > /etc/network/if-up.d/stargate-routes
      echo 'if [ "$IFACE" = "tailscale0" ]; then' >> /etc/network/if-up.d/stargate-routes
      echo '  ip route add 10.224.0.0/16 dev tailscale0 2>/dev/null || true' >> /etc/network/if-up.d/stargate-routes
      echo '  for i in $(seq 0 10); do' >> /etc/network/if-up.d/stargate-routes
      echo '    ip route add 10.244.$i.0/24 dev tailscale0 2>/dev/null || true' >> /etc/network/if-up.d/stargate-routes
      echo '  done' >> /etc/network/if-up.d/stargate-routes
      echo 'fi' >> /etc/network/if-up.d/stargate-routes
      chmod +x /etc/network/if-up.d/stargate-routes

runcmd:
  - /tmp/configure-router.sh
`, tailscaleAuthKey, vmName, advertiseRoutes)

	cloudInit = strings.ReplaceAll(cloudInit, "\t", "    ")
	return cloudInit, nil
}

// buildAKSRouterCloudInit creates cloud-init for an AKS router that:
// 1. Advertises multiple CIDRs (VNet, Pod, Service) to Tailscale
// 2. Sets up a proxy to the AKS API server
// 3. Sets up kernel routes for DC worker pod CIDRs via tailscale0
func buildAKSRouterCloudInit(vmName, adminUser, sshPublicKey, tailscaleAuthKey string, routeCIDRs []string, apiServerFQDN string) (string, error) {
	if tailscaleAuthKey == "" {
		return "", fmt.Errorf("missing tailscale auth key for router %s", vmName)
	}
	if len(routeCIDRs) == 0 {
		return "", fmt.Errorf("no route CIDRs provided for AKS router %s", vmName)
	}

	routes := strings.Join(routeCIDRs, ",")

	cloudInit := baseCloudInitHeader(vmName, adminUser, sshPublicKey)
	cloudInit += fmt.Sprintf(`
write_files:
  - path: /tmp/configure-router.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -ex
      sysctl -w net.ipv4.ip_forward=1
      sed -i 's/^#*net.ipv4.ip_forward.*/net.ipv4.ip_forward=1/' /etc/sysctl.conf
      curl -fsSL https://tailscale.com/install.sh | sh
      tailscale up --authkey %s --hostname %s --advertise-routes=%s --accept-routes
      # MASQUERADE for LAN traffic going to Tailscale network
      iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE
      mkdir -p /etc/iptables
      iptables-save > /etc/iptables/rules.v4 || true
      
      # Add kernel routes for DC worker pod CIDRs via tailscale0
      # These are in the 10.244.50-250 range based on derivePodCIDR formula
      # We add routes for the common range used by DC workers
      for i in $(seq 50 70); do
        ip route add 10.244.$i.0/24 dev tailscale0 2>/dev/null || true
      done
      
      # Persist routes for reboot - write script using echo to avoid YAML heredoc issues
      mkdir -p /etc/network/if-up.d
      echo '#!/bin/bash' > /etc/network/if-up.d/stargate-routes
      echo 'if [ "$IFACE" = "tailscale0" ]; then' >> /etc/network/if-up.d/stargate-routes
      echo '  for i in $(seq 50 70); do' >> /etc/network/if-up.d/stargate-routes
      echo '    ip route add 10.244.$i.0/24 dev tailscale0 2>/dev/null || true' >> /etc/network/if-up.d/stargate-routes
      echo '  done' >> /etc/network/if-up.d/stargate-routes
      echo 'fi' >> /etc/network/if-up.d/stargate-routes
      chmod +x /etc/network/if-up.d/stargate-routes
`, tailscaleAuthKey, vmName, routes)

	// Add AKS API proxy if FQDN is provided
	if apiServerFQDN != "" {
		cloudInit += fmt.Sprintf(`
  - path: /etc/systemd/system/aks-proxy.service
    content: |
      [Unit]
      Description=AKS API Server Proxy
      After=network.target
      [Service]
      Type=simple
      ExecStart=/usr/bin/socat TCP-LISTEN:6443,fork,reuseaddr TCP:%s:443
      Restart=always
      RestartSec=5
      [Install]
      WantedBy=multi-user.target

runcmd:
  - apt-get update && apt-get install -y socat
  - /tmp/configure-router.sh
  - systemctl daemon-reload
  - systemctl enable --now aks-proxy
`, apiServerFQDN)
	} else {
		cloudInit += `
runcmd:
  - /tmp/configure-router.sh
`
	}

	cloudInit = strings.ReplaceAll(cloudInit, "\t", "    ")
	return cloudInit, nil
}

func buildWorkerCloudInit(vmName, adminUser, sshPublicKey, routerIP string, extraRoutes []string) (string, error) {
	cloudInit := baseCloudInitHeader(vmName, adminUser, sshPublicKey)

	// If router IP is provided, set up routes for Tailscale CGNAT and any extra CIDRs (e.g., AKS)
	if routerIP != "" {
		// Build route commands - Tailscale CGNAT + any extra routes (AKS CIDRs, etc.)
		allRoutes := []string{"100.64.0.0/10"}
		allRoutes = append(allRoutes, extraRoutes...)

		routeCmds := ""
		for _, cidr := range allRoutes {
			routeCmds += fmt.Sprintf("  - ip route add %s via %s || true\n", cidr, routerIP)
			routeCmds += fmt.Sprintf("  - echo \"%s via %s\" >> /etc/network/routes.conf || true\n", cidr, routerIP)
		}

		cloudInit += fmt.Sprintf(`
runcmd:
%s  - echo "worker initialized with routes via router %s"
`, routeCmds, routerIP)
	} else {
		cloudInit += `
runcmd:
  - echo "worker initialized without tailscale"
`
	}

	cloudInit = strings.ReplaceAll(cloudInit, "\t", "    ")
	return cloudInit, nil
}

func (p *Provider) ensureResourceGroup(ctx context.Context) error {
	_, err := p.rgClient.Get(ctx, p.cfg.ResourceGroup, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}

	_, err = p.rgClient.CreateOrUpdate(ctx, p.cfg.ResourceGroup, armresources.ResourceGroup{Location: to.Ptr(p.cfg.Location)}, nil)
	return err
}

func (p *Provider) ensureVNet(ctx context.Context) error {
	_, err := p.vnetClient.Get(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}

	poller, err := p.vnetClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, armnetwork.VirtualNetwork{
		Location: to.Ptr(p.cfg.Location),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{AddressPrefixes: []*string{to.Ptr(p.cfg.VNetCIDR)}},
		},
	}, nil)
	if err != nil {
		return err
	}

	_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	return err
}

func (p *Provider) ensureSubnet(ctx context.Context) (string, error) {
	subnet, err := p.subnetClient.Get(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, p.cfg.SubnetName, nil)
	if err == nil {
		if subnet.ID == nil {
			return "", errors.New("subnet has no ID")
		}
		return *subnet.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := p.subnetClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, p.cfg.SubnetName, armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: to.Ptr(p.cfg.SubnetCIDR)},
	}, nil)
	if err != nil {
		return "", err
	}

	resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errors.New("subnet has no ID")
	}
	return *resp.ID, nil
}

func (p *Provider) ensurePublicIP(ctx context.Context, name string) (string, error) {
	existing, err := p.pipClient.Get(ctx, p.cfg.ResourceGroup, name, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("public IP has no ID")
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := p.pipClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, armnetwork.PublicIPAddress{
		Location: to.Ptr(p.cfg.Location),
		Zones:    []*string{to.Ptr(p.cfg.Zone)},
		SKU:      &armnetwork.PublicIPAddressSKU{Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
		},
	}, nil)
	if err != nil {
		return "", err
	}

	resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errors.New("public IP has no ID")
	}
	return *resp.ID, nil
}

func (p *Provider) getPublicIPAddress(ctx context.Context, name string) (string, error) {
	pip, err := p.pipClient.Get(ctx, p.cfg.ResourceGroup, name, nil)
	if err != nil {
		return "", err
	}
	if pip.Properties == nil || pip.Properties.IPAddress == nil {
		return "", fmt.Errorf("public IP %s has no address yet", name)
	}
	return *pip.Properties.IPAddress, nil
}

func (p *Provider) ensureNIC(ctx context.Context, name, subnetID, publicIPID string) (string, error) {
	existing, err := p.nicClient.Get(ctx, p.cfg.ResourceGroup, name, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("NIC has no ID")
		}
		// Ensure IP forwarding is enabled for worker NICs so they can route pod traffic
		if existing.Properties != nil && (existing.Properties.EnableIPForwarding == nil || !*existing.Properties.EnableIPForwarding) {
			existing.Properties.EnableIPForwarding = to.Ptr(true)
			poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, existing.Interface, nil)
			if err != nil {
				return "", fmt.Errorf("enable IP forwarding on worker NIC: %w", err)
			}
			_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
			if err != nil {
				return "", err
			}
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	ipProps := &armnetwork.InterfaceIPConfigurationPropertiesFormat{
		Subnet:                    &armnetwork.Subnet{ID: to.Ptr(subnetID)},
		PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
	}
	if publicIPID != "" {
		ipProps.PublicIPAddress = &armnetwork.PublicIPAddress{ID: to.Ptr(publicIPID)}
	}

	poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, armnetwork.Interface{
		Location: to.Ptr(p.cfg.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			EnableIPForwarding: to.Ptr(true),
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name:       to.Ptr("ipconfig1"),
				Properties: ipProps,
			}},
		},
	}, nil)
	if err != nil {
		return "", err
	}

	resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errors.New("NIC has no ID")
	}
	return *resp.ID, nil
}

func (p *Provider) getPrivateIP(ctx context.Context, name string) (string, error) {
	nic, err := p.nicClient.Get(ctx, p.cfg.ResourceGroup, name, nil)
	if err != nil {
		return "", err
	}
	if nic.Properties == nil || len(nic.Properties.IPConfigurations) == 0 || nic.Properties.IPConfigurations[0].Properties == nil || nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress == nil {
		return "", fmt.Errorf("NIC %s has no private IP yet", name)
	}
	return *nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress, nil
}

func (p *Provider) ensureVM(ctx context.Context, vmName, nicID, cloudInit, sshPublicKey string) error {
	_, err := p.vmClient.Get(ctx, p.cfg.ResourceGroup, vmName, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}

	customData := base64.StdEncoding.EncodeToString([]byte(cloudInit))
	sshKeyPath := filepath.Join("/home", p.cfg.AdminUsername, ".ssh", "authorized_keys")

	poller, err := p.vmClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, vmName, armcompute.VirtualMachine{
		Location: to.Ptr(p.cfg.Location),
		Zones:    []*string{to.Ptr(p.cfg.Zone)},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(p.cfg.VMSize))},
			StorageProfile: &armcompute.StorageProfile{ImageReference: &armcompute.ImageReference{
				Publisher: to.Ptr("Canonical"),
				Offer:     to.Ptr("0001-com-ubuntu-server-jammy"),
				SKU:       to.Ptr("22_04-lts-gen2"),
				Version:   to.Ptr("latest"),
			}},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(vmName),
				AdminUsername: to.Ptr(p.cfg.AdminUsername),
				CustomData:    to.Ptr(customData),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH:                           &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{Path: to.Ptr(sshKeyPath), KeyData: to.Ptr(sshPublicKey)}}},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
				ID:         to.Ptr(nicID),
				Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: to.Ptr(true)},
			}}},
		},
	}, nil)
	if err != nil {
		return err
	}

	_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 30 * time.Second})
	return err
}

func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}

// ensureNICWithIPForwarding creates a NIC with IP forwarding enabled (for routers)
func (p *Provider) ensureNICWithIPForwarding(ctx context.Context, name, subnetID, publicIPID string) (string, error) {
	existing, err := p.nicClient.Get(ctx, p.cfg.ResourceGroup, name, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("NIC has no ID")
		}
		// Ensure IP forwarding is enabled
		if existing.Properties != nil && (existing.Properties.EnableIPForwarding == nil || !*existing.Properties.EnableIPForwarding) {
			existing.Properties.EnableIPForwarding = to.Ptr(true)
			poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, existing.Interface, nil)
			if err != nil {
				return "", fmt.Errorf("enable IP forwarding on existing NIC: %w", err)
			}
			_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
			if err != nil {
				return "", err
			}
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	ipProps := &armnetwork.InterfaceIPConfigurationPropertiesFormat{
		Subnet:                    &armnetwork.Subnet{ID: to.Ptr(subnetID)},
		PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
	}
	if publicIPID != "" {
		ipProps.PublicIPAddress = &armnetwork.PublicIPAddress{ID: to.Ptr(publicIPID)}
	}

	poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, armnetwork.Interface{
		Location: to.Ptr(p.cfg.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			EnableIPForwarding: to.Ptr(true),
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name:       to.Ptr("ipconfig1"),
				Properties: ipProps,
			}},
		},
	}, nil)
	if err != nil {
		return "", err
	}

	resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errors.New("NIC has no ID")
	}
	return *resp.ID, nil
}

// ensureRouteTable creates a route table with Tailscale CGNAT route, optional extra routes (e.g., AKS CIDRs),
// and per-worker pod CIDR routes, then associates it with the subnet
func (p *Provider) ensureRouteTable(ctx context.Context, subnetID, routerIP string, extraRouteCIDRs []string, workerRoutes []route) error {
	routeTableName := fmt.Sprintf("%s-route-table", p.cfg.ResourceGroup)

	// Build routes: Tailscale CGNAT + any extra CIDRs
	routes := []*armnetwork.Route{{
		Name: to.Ptr("tailscale-route"),
		Properties: &armnetwork.RoutePropertiesFormat{
			AddressPrefix:    to.Ptr("100.64.0.0/10"),
			NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
			NextHopIPAddress: to.Ptr(routerIP),
		},
	}}

	// Add extra routes (e.g., AKS VNet, Pod, Service CIDRs)
	for i, cidr := range extraRouteCIDRs {
		routes = append(routes, &armnetwork.Route{
			Name: to.Ptr(fmt.Sprintf("extra-route-%d", i)),
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    to.Ptr(cidr),
				NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
				NextHopIPAddress: to.Ptr(routerIP),
			},
		})
	}

	// Add per-worker pod CIDR routes to keep pod egress symmetric and avoid blackholes
	for i, wr := range workerRoutes {
		if wr.Prefix == "" || wr.NextHop == "" {
			continue
		}
		routes = append(routes, &armnetwork.Route{
			Name: to.Ptr(fmt.Sprintf("worker-pod-route-%d", i)),
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    to.Ptr(wr.Prefix),
				NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
				NextHopIPAddress: to.Ptr(wr.NextHop),
			},
		})
	}

	// Check if route table exists
	existing, err := p.routeTableClient.Get(ctx, p.cfg.ResourceGroup, routeTableName, nil)
	if err != nil && !isNotFound(err) {
		return err
	}

	var routeTableID string
	if err == nil && existing.ID != nil {
		// Route table exists - update it with new routes
		existing.Properties.Routes = routes
		poller, err := p.routeTableClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, routeTableName, existing.RouteTable, nil)
		if err != nil {
			return fmt.Errorf("update route table: %w", err)
		}
		resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
		if err != nil {
			return fmt.Errorf("poll route table update: %w", err)
		}
		routeTableID = *resp.ID
	} else {
		// Create route table with routes
		poller, err := p.routeTableClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, routeTableName, armnetwork.RouteTable{
			Location: to.Ptr(p.cfg.Location),
			Properties: &armnetwork.RouteTablePropertiesFormat{
				Routes: routes,
			},
		}, nil)
		if err != nil {
			return fmt.Errorf("create route table: %w", err)
		}

		resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
		if err != nil {
			return fmt.Errorf("poll route table creation: %w", err)
		}
		if resp.ID == nil {
			return errors.New("route table has no ID")
		}
		routeTableID = *resp.ID
	}

	// Associate route table with subnet
	subnet, err := p.subnetClient.Get(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, p.cfg.SubnetName, nil)
	if err != nil {
		return fmt.Errorf("get subnet for route association: %w", err)
	}

	// Check if already associated
	if subnet.Properties != nil && subnet.Properties.RouteTable != nil && subnet.Properties.RouteTable.ID != nil {
		if *subnet.Properties.RouteTable.ID == routeTableID {
			return nil // Already associated
		}
	}

	// Update subnet with route table
	if subnet.Properties == nil {
		subnet.Properties = &armnetwork.SubnetPropertiesFormat{}
	}
	subnet.Properties.RouteTable = &armnetwork.RouteTable{ID: to.Ptr(routeTableID)}

	poller, err := p.subnetClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, p.cfg.SubnetName, subnet.Subnet, nil)
	if err != nil {
		return fmt.Errorf("associate route table with subnet: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return fmt.Errorf("poll subnet route table association: %w", err)
	}

	return nil
}

// ensureTailscaleRoutes enables the advertised routes for a Tailscale router.
// This uses the Tailscale API to auto-approve routes, avoiding manual approval in the admin console.
// If TAILSCALE_API_KEY is not set or the device is not yet registered, it logs a warning and returns nil.
func (p *Provider) ensureTailscaleRoutes(ctx context.Context, hostname string) error {
	if p.tsClient == nil {
		p.logger.Info("Tailscale API not configured, routes must be approved manually")
		return nil
	}

	log := p.logger.With("operation", "ensureTailscaleRoutes", "hostname", hostname)

	// Wait for the device to register with Tailscale (cloud-init takes some time)
	var device *tailscale.Device
	var err error
	for i := 0; i < 30; i++ { // Wait up to 5 minutes
		device, err = p.tsClient.FindDeviceByHostname(ctx, hostname)
		if err == nil {
			break
		}
		log.Info("waiting for device to register with Tailscale", "attempt", i+1)
		time.Sleep(10 * time.Second)
	}
	if err != nil {
		return fmt.Errorf("device not found after waiting: %w", err)
	}

	// Enable all advertised routes
	log.Info("enabling routes for device", "deviceID", device.ID, "advertisedRoutes", device.AdvertisedRoutes)
	if err := p.tsClient.EnableAllRoutes(ctx, device.ID); err != nil {
		return fmt.Errorf("enable routes: %w", err)
	}

	// Get updated device to confirm routes are enabled
	device, err = p.tsClient.GetDevice(ctx, device.ID)
	if err != nil {
		return fmt.Errorf("get updated device: %w", err)
	}

	log.Info("routes enabled successfully", "enabledRoutes", device.EnabledRoutes)
	return nil
}

// ensureAKSRouteTable creates a route table in the AKS VNet that routes DC-bound traffic
// through the AKS router. This includes:
// - DC subnet (e.g., 10.50.0.0/16) → router
// - DC worker pod CIDRs (10.244.50-70.0/24 range) → router
// - Tailscale CGNAT (100.64.0.0/10) → router
//
// The route table is associated with both the AKS router subnet and the AKS node subnet
// so that AKS nodes can reach DC workers through the router.
func (p *Provider) ensureAKSRouteTable(ctx context.Context, cfg *providers.AKSRouterConfig, routerIP string) error {
	routeTableName := "stargate-workers-rt"

	// Build routes for DC-bound traffic
	routes := []*armnetwork.Route{
		{
			Name: to.Ptr("tailscale-route"),
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    to.Ptr("100.64.0.0/10"),
				NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
				NextHopIPAddress: to.Ptr(routerIP),
			},
		},
		{
			Name: to.Ptr("to-dc-workers"),
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    to.Ptr("10.50.0.0/16"), // DC worker subnet
				NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
				NextHopIPAddress: to.Ptr(routerIP),
			},
		},
	}

	// Add routes for DC worker pod CIDRs (10.244.50-70.0/24 range)
	// These are derived from DC worker private IPs using derivePodCIDR formula
	for i := 50; i <= 70; i++ {
		routes = append(routes, &armnetwork.Route{
			Name: to.Ptr(fmt.Sprintf("dc-pod-cidr-%d", i)),
			Properties: &armnetwork.RoutePropertiesFormat{
				AddressPrefix:    to.Ptr(fmt.Sprintf("10.244.%d.0/24", i)),
				NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
				NextHopIPAddress: to.Ptr(routerIP),
			},
		})
	}

	// Query AKS nodes and add routes for their pod CIDRs (for DC → AKS traffic)
	if cfg.ClusterRG != "" && cfg.ClusterName != "" {
		aksNodeRoutes, err := p.getAKSNodeRoutes(ctx, cfg.ClusterRG, cfg.ClusterName)
		if err != nil {
			// Log but don't fail - AKS node routes can be added later
			fmt.Printf("Warning: could not get AKS node routes: %v\n", err)
		} else {
			routes = append(routes, aksNodeRoutes...)
		}
	}

	// Check if route table exists
	existing, err := p.routeTableClient.Get(ctx, cfg.ResourceGroup, routeTableName, nil)
	if err != nil && !isNotFound(err) {
		return err
	}

	var routeTableID string
	if err == nil && existing.ID != nil {
		// Route table exists - update it with new routes
		existing.Properties.Routes = routes
		poller, err := p.routeTableClient.BeginCreateOrUpdate(ctx, cfg.ResourceGroup, routeTableName, existing.RouteTable, nil)
		if err != nil {
			return fmt.Errorf("update AKS route table: %w", err)
		}
		resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
		if err != nil {
			return fmt.Errorf("poll AKS route table update: %w", err)
		}
		routeTableID = *resp.ID
	} else {
		// Create route table with routes
		poller, err := p.routeTableClient.BeginCreateOrUpdate(ctx, cfg.ResourceGroup, routeTableName, armnetwork.RouteTable{
			Location: to.Ptr(p.cfg.Location),
			Properties: &armnetwork.RouteTablePropertiesFormat{
				Routes: routes,
			},
		}, nil)
		if err != nil {
			return fmt.Errorf("create AKS route table: %w", err)
		}

		resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
		if err != nil {
			return fmt.Errorf("poll AKS route table creation: %w", err)
		}
		if resp.ID == nil {
			return errors.New("AKS route table has no ID")
		}
		routeTableID = *resp.ID
	}

	// Associate route table with AKS router subnet
	if err := p.associateRouteTableWithSubnet(ctx, cfg.ResourceGroup, cfg.VNetName, cfg.SubnetName, routeTableID); err != nil {
		return fmt.Errorf("associate route table with router subnet: %w", err)
	}

	// Also associate with AKS node subnet (aks-subnet) so AKS nodes can reach DC
	if err := p.associateRouteTableWithSubnet(ctx, cfg.ResourceGroup, cfg.VNetName, "aks-subnet", routeTableID); err != nil {
		// Don't fail if aks-subnet doesn't exist or has different name
		// The route table is still set up for the router subnet
		_ = err // Ignore error - aks-subnet may have different name
	}

	return nil
}

// getAKSNodeRoutes queries AKS VMSS instances and returns routes for their pod CIDRs.
func (p *Provider) getAKSNodeRoutes(ctx context.Context, clusterRG, clusterName string) ([]*armnetwork.Route, error) {
	// Get the AKS cluster to find the node resource group
	cluster, err := p.aksClient.Get(ctx, clusterRG, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("get AKS cluster: %w", err)
	}

	if cluster.Properties == nil || cluster.Properties.NodeResourceGroup == nil {
		return nil, fmt.Errorf("AKS cluster has no node resource group")
	}
	nodeRG := *cluster.Properties.NodeResourceGroup

	// List all VMSS in the node resource group and get node IPs
	var routes []*armnetwork.Route
	nodeIndex := 0

	pager := p.vmssClient.NewListPager(nodeRG, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list VMSS: %w", err)
		}

		for _, vmss := range page.Value {
			if vmss.Name == nil {
				continue
			}
			vmssName := *vmss.Name

			// List VMs in this VMSS
			vmPager := p.vmssVMsClient.NewListPager(nodeRG, vmssName, nil)
			for vmPager.More() {
				vmPage, err := vmPager.NextPage(ctx)
				if err != nil {
					continue // Skip this VMSS if we can't list VMs
				}

				for _, vm := range vmPage.Value {
					if vm.Properties == nil || vm.Properties.NetworkProfile == nil {
						continue
					}

					// Get private IP from network interface
					var privateIP string
					for _, nicRef := range vm.Properties.NetworkProfile.NetworkInterfaces {
						if nicRef.ID == nil {
							continue
						}
						privateIP, err = p.getPrivateIPFromNICID(ctx, *nicRef.ID)
						if err != nil {
							continue
						}
						break
					}

					if privateIP == "" {
						continue
					}

					// Pod CIDR is 10.244.X.0/24 where X is node index
					podCIDR := fmt.Sprintf("10.244.%d.0/24", nodeIndex)
					routeName := fmt.Sprintf("aks-pod-cidr-%d", nodeIndex)

					routes = append(routes, &armnetwork.Route{
						Name: to.Ptr(routeName),
						Properties: &armnetwork.RoutePropertiesFormat{
							AddressPrefix:    to.Ptr(podCIDR),
							NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
							NextHopIPAddress: to.Ptr(privateIP),
						},
					})

					nodeIndex++
				}
			}
		}
	}

	return routes, nil
}

// getPrivateIPFromNICID extracts the private IP from a NIC by its Azure resource ID.
func (p *Provider) getPrivateIPFromNICID(ctx context.Context, nicID string) (string, error) {
	// Parse NIC ID: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/networkInterfaces/{name}
	parts := strings.Split(nicID, "/")
	var rg, name string
	for i, part := range parts {
		if part == "resourceGroups" && i+1 < len(parts) {
			rg = parts[i+1]
		}
		if part == "networkInterfaces" && i+1 < len(parts) {
			name = parts[i+1]
		}
	}
	if rg == "" || name == "" {
		return "", fmt.Errorf("invalid NIC ID: %s", nicID)
	}

	nic, err := p.nicClient.Get(ctx, rg, name, nil)
	if err != nil {
		return "", err
	}

	if nic.Properties != nil && len(nic.Properties.IPConfigurations) > 0 {
		for _, ipConfig := range nic.Properties.IPConfigurations {
			if ipConfig.Properties != nil && ipConfig.Properties.PrivateIPAddress != nil {
				return *ipConfig.Properties.PrivateIPAddress, nil
			}
		}
	}
	return "", fmt.Errorf("no private IP found")
}

// associateRouteTableWithSubnet associates a route table with a subnet.
func (p *Provider) associateRouteTableWithSubnet(ctx context.Context, resourceGroup, vnetName, subnetName, routeTableID string) error {
	subnet, err := p.subnetClient.Get(ctx, resourceGroup, vnetName, subnetName, nil)
	if err != nil {
		return fmt.Errorf("get subnet %s: %w", subnetName, err)
	}

	// Check if already associated
	if subnet.Properties != nil && subnet.Properties.RouteTable != nil && subnet.Properties.RouteTable.ID != nil {
		if *subnet.Properties.RouteTable.ID == routeTableID {
			return nil // Already associated
		}
	}

	// Update subnet with route table
	if subnet.Properties == nil {
		subnet.Properties = &armnetwork.SubnetPropertiesFormat{}
	}
	subnet.Properties.RouteTable = &armnetwork.RouteTable{ID: to.Ptr(routeTableID)}

	poller, err := p.subnetClient.BeginCreateOrUpdate(ctx, resourceGroup, vnetName, subnetName, subnet.Subnet, nil)
	if err != nil {
		return fmt.Errorf("associate route table: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return fmt.Errorf("poll subnet route table association: %w", err)
	}

	return nil
}

// derivePodCIDR returns a /24 pod CIDR in 10.244.x.0/24 based on the node's private IP.
// Uses the formula from the bootstrap script: (third_octet*10 + fourth_octet) % 200 + 50
func derivePodCIDR(privateIP string) string {
	ip := net.ParseIP(privateIP).To4()
	if ip == nil {
		return ""
	}
	// Match the bootstrap script's podCIDR derivation formula
	uniqueOctet := (int(ip[2])*10+int(ip[3]))%200 + 50
	return fmt.Sprintf("10.244.%d.0/24", uniqueOctet)
}

// ensureSubnetInVNet creates or gets a subnet in a specific VNet and resource group.
func (p *Provider) ensureSubnetInVNet(ctx context.Context, resourceGroup, vnetName, subnetName, subnetCIDR string) (string, error) {
	subnet, err := p.subnetClient.Get(ctx, resourceGroup, vnetName, subnetName, nil)
	if err == nil {
		if subnet.ID == nil {
			return "", errors.New("subnet has no ID")
		}
		return *subnet.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := p.subnetClient.BeginCreateOrUpdate(ctx, resourceGroup, vnetName, subnetName, armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: to.Ptr(subnetCIDR)},
	}, nil)
	if err != nil {
		return "", err
	}

	resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errors.New("subnet has no ID")
	}
	return *resp.ID, nil
}

// ensurePublicIPInRG creates a public IP in a specific resource group.
func (p *Provider) ensurePublicIPInRG(ctx context.Context, resourceGroup, name string) (string, error) {
	existing, err := p.pipClient.Get(ctx, resourceGroup, name, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("public IP has no ID")
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := p.pipClient.BeginCreateOrUpdate(ctx, resourceGroup, name, armnetwork.PublicIPAddress{
		Location: to.Ptr(p.cfg.Location),
		Zones:    []*string{to.Ptr(p.cfg.Zone)},
		SKU:      &armnetwork.PublicIPAddressSKU{Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard)},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
			PublicIPAddressVersion:   to.Ptr(armnetwork.IPVersionIPv4),
		},
	}, nil)
	if err != nil {
		return "", err
	}

	resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errors.New("public IP has no ID")
	}
	return *resp.ID, nil
}

// ensureNICWithIPForwardingInRG creates a NIC with IP forwarding in a specific resource group.
func (p *Provider) ensureNICWithIPForwardingInRG(ctx context.Context, resourceGroup, name, subnetID, publicIPID string) (string, error) {
	existing, err := p.nicClient.Get(ctx, resourceGroup, name, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("NIC has no ID")
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	ipProps := &armnetwork.InterfaceIPConfigurationPropertiesFormat{
		Subnet:                    &armnetwork.Subnet{ID: to.Ptr(subnetID)},
		PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
	}
	if publicIPID != "" {
		ipProps.PublicIPAddress = &armnetwork.PublicIPAddress{ID: to.Ptr(publicIPID)}
	}

	poller, err := p.nicClient.BeginCreateOrUpdate(ctx, resourceGroup, name, armnetwork.Interface{
		Location: to.Ptr(p.cfg.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			EnableIPForwarding: to.Ptr(true),
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name:       to.Ptr("ipconfig1"),
				Properties: ipProps,
			}},
		},
	}, nil)
	if err != nil {
		return "", err
	}

	resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return "", err
	}
	if resp.ID == nil {
		return "", errors.New("NIC has no ID")
	}
	return *resp.ID, nil
}

// ensureVMInRG creates a VM in a specific resource group.
func (p *Provider) ensureVMInRG(ctx context.Context, resourceGroup, vmName, nicID, cloudInit, sshPublicKey string) error {
	_, err := p.vmClient.Get(ctx, resourceGroup, vmName, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}

	customData := base64.StdEncoding.EncodeToString([]byte(cloudInit))
	sshKeyPath := filepath.Join("/home", p.cfg.AdminUsername, ".ssh", "authorized_keys")

	poller, err := p.vmClient.BeginCreateOrUpdate(ctx, resourceGroup, vmName, armcompute.VirtualMachine{
		Location: to.Ptr(p.cfg.Location),
		Zones:    []*string{to.Ptr(p.cfg.Zone)},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(p.cfg.VMSize))},
			StorageProfile: &armcompute.StorageProfile{ImageReference: &armcompute.ImageReference{
				Publisher: to.Ptr("Canonical"),
				Offer:     to.Ptr("0001-com-ubuntu-server-jammy"),
				SKU:       to.Ptr("22_04-lts-gen2"),
				Version:   to.Ptr("latest"),
			}},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(vmName),
				AdminUsername: to.Ptr(p.cfg.AdminUsername),
				CustomData:    to.Ptr(customData),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH:                           &armcompute.SSHConfiguration{PublicKeys: []*armcompute.SSHPublicKey{{Path: to.Ptr(sshKeyPath), KeyData: to.Ptr(sshPublicKey)}}},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{NetworkInterfaces: []*armcompute.NetworkInterfaceReference{{
				ID:         to.Ptr(nicID),
				Properties: &armcompute.NetworkInterfaceReferenceProperties{Primary: to.Ptr(true)},
			}}},
		},
	}, nil)
	if err != nil {
		return err
	}

	_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 30 * time.Second})
	return err
}

// getPublicIPAddressInRG gets the IP address from a public IP in a specific resource group.
func (p *Provider) getPublicIPAddressInRG(ctx context.Context, resourceGroup, name string) (string, error) {
	pip, err := p.pipClient.Get(ctx, resourceGroup, name, nil)
	if err != nil {
		return "", err
	}
	if pip.Properties == nil || pip.Properties.IPAddress == nil {
		return "", fmt.Errorf("public IP %s has no address yet", name)
	}
	return *pip.Properties.IPAddress, nil
}

// getPrivateIPInRG gets the private IP from a NIC in a specific resource group.
func (p *Provider) getPrivateIPInRG(ctx context.Context, resourceGroup, name string) (string, error) {
	nic, err := p.nicClient.Get(ctx, resourceGroup, name, nil)
	if err != nil {
		return "", err
	}
	if nic.Properties == nil || len(nic.Properties.IPConfigurations) == 0 || nic.Properties.IPConfigurations[0].Properties == nil || nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress == nil {
		return "", fmt.Errorf("NIC %s has no private IP yet", name)
	}
	return *nic.Properties.IPConfigurations[0].Properties.PrivateIPAddress, nil
}
