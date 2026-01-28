package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v4"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v4"
	"github.com/vpatelsj/stargate/pkg/infra/providers"
	"github.com/vpatelsj/stargate/pkg/infra/providers/azure"
	"github.com/vpatelsj/stargate/pkg/infra/providers/qemu"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(val string) error {
	for _, part := range strings.Split(val, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			*s = append(*s, trimmed)
		}
	}
	return nil
}

// Package-level Tailscale OAuth credentials for API-based route approval
var (
	tsClientID     string
	tsClientSecret string
)

func main() {
	var vmNames stringSlice
	var providerName string
	var routerName string
	var role string

	// Azure flags
	var subscriptionID, location, zone, resourceGroup string
	var vnetName, vnetCIDR, subnetName, subnetCIDR string
	var vmSize, adminUser, sshPubKeyPath, tailscaleAuthKey string

	// AKS router flags (for provisioning a Tailscale router in AKS VNet)
	var aksRouterName, aksResourceGroup, aksVNetName, aksSubnetName, aksSubnetCIDR, aksVNetCIDR string
	var aksClusterName, aksClusterRG string
	var aksRouteCIDRsFlag stringSlice // CIDRs to route via DC router to reach AKS (when AKS router already exists)

	// QEMU flags
	var qemuWorkDir, qemuImageCacheDir, qemuImageURL string
	var qemuCPUs, qemuMemoryMB, qemuDiskSizeGB int

	// Server CR flags
	var kubeconfig, namespace string
	var skipServerCR bool

	flag.StringVar(&providerName, "provider", "azure", "Provider to use (azure, qemu).")
	flag.StringVar(&role, "role", "dc", "What to provision: 'dc' (router+workers), 'aks-router' (AKS VNet router only).")
	flag.Var(&vmNames, "vm", "VM name (can be repeated or comma-separated).")
	flag.StringVar(&routerName, "router-name", "stargate-router", "Dedicated subnet router VM name (one per datacenter).")

	flag.StringVar(&subscriptionID, "subscription-id", os.Getenv("AZURE_SUBSCRIPTION_ID"), "Azure subscription ID.")
	flag.StringVar(&location, "location", "canadacentral", "Azure region.")
	flag.StringVar(&zone, "zone", "1", "Azure availability zone.")
	flag.StringVar(&resourceGroup, "resource-group", "stargate-vapa-rg", "Azure resource group.")
	flag.StringVar(&vnetName, "vnet-name", "stargate-vnet", "Azure VNet name.")
	flag.StringVar(&vnetCIDR, "vnet-cidr", "10.50.0.0/16", "Azure VNet CIDR.")
	flag.StringVar(&subnetName, "subnet-name", "stargate-subnet", "Azure subnet name.")
	flag.StringVar(&subnetCIDR, "subnet-cidr", "10.50.1.0/24", "Azure subnet CIDR.")
	flag.StringVar(&vmSize, "vm-size", "Standard_D2s_v5", "VM size.")
	flag.StringVar(&adminUser, "admin-username", "ubuntu", "Admin username.")
	flag.StringVar(&sshPubKeyPath, "ssh-public-key", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa.pub"), "SSH public key path.")
	flag.StringVar(&tailscaleAuthKey, "tailscale-auth-key", os.Getenv("TAILSCALE_AUTH_KEY"), "Tailscale auth key (required).")

	// AKS router flags (for provisioning a Tailscale router in existing AKS VNet)
	flag.StringVar(&aksRouterName, "aks-router-name", "", "Name for the AKS VNet router VM (enables AKS router provisioning).")
	flag.StringVar(&aksResourceGroup, "aks-resource-group", "", "Resource group containing the AKS VNet (usually MC_*).")
	flag.StringVar(&aksVNetName, "aks-vnet-name", "", "Existing AKS VNet name.")
	flag.StringVar(&aksSubnetName, "aks-subnet-name", "stargate-aks-router-subnet", "Subnet name for the AKS router.")
	flag.StringVar(&aksSubnetCIDR, "aks-subnet-cidr", "10.224.250.0/24", "CIDR for the AKS router subnet.")
	flag.StringVar(&aksVNetCIDR, "aks-vnet-cidr", "", "AKS VNet CIDR (auto-detected if --aks-cluster-name provided).")
	flag.StringVar(&aksClusterName, "aks-cluster-name", "", "AKS cluster name (for auto-detecting network CIDRs).")
	flag.StringVar(&aksClusterRG, "aks-cluster-rg", "", "AKS cluster resource group (defaults to --aks-cluster-name if not set).")
	flag.Var(&aksRouteCIDRsFlag, "aks-route-cidrs", "CIDRs to route to AKS via DC router (comma-separated, e.g., 10.224.0.0/12,10.244.0.0/16,10.0.0.0/16).")

	// Tailscale API flags for route approval
	flag.StringVar(&tsClientID, "tailscale-client-id", os.Getenv("TAILSCALE_CLIENT_ID"), "Tailscale OAuth client ID (for route approval).")
	flag.StringVar(&tsClientSecret, "tailscale-client-secret", os.Getenv("TAILSCALE_CLIENT_SECRET"), "Tailscale OAuth client secret (for route approval).")

	// QEMU flags
	flag.StringVar(&qemuWorkDir, "qemu-work-dir", "/var/lib/stargate/vms", "QEMU: directory for VM storage.")
	flag.StringVar(&qemuImageCacheDir, "qemu-image-cache", "/var/lib/stargate/images", "QEMU: directory for cached images.")
	flag.StringVar(&qemuImageURL, "qemu-image-url", "", "QEMU: URL for base image (default: Ubuntu cloud image).")
	flag.IntVar(&qemuCPUs, "qemu-cpus", 2, "QEMU: number of CPUs per VM.")
	flag.IntVar(&qemuMemoryMB, "qemu-memory", 4096, "QEMU: memory in MB per VM.")
	flag.IntVar(&qemuDiskSizeGB, "qemu-disk", 20, "QEMU: disk size in GB per VM.")

	// QEMU subnet CIDR (for route advertisement)
	var qemuSubnetCIDR string
	flag.StringVar(&qemuSubnetCIDR, "qemu-subnet-cidr", "192.168.100.0/24", "QEMU: subnet CIDR for route advertisement.")

	// Server CR flags
	// Handle kubeconfig path when running as sudo
	defaultKubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		defaultKubeconfig = filepath.Join("/home", sudoUser, ".kube", "config")
	}
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKubeconfig, "Path to kubeconfig file.")
	flag.StringVar(&namespace, "namespace", "azure-dc", "Namespace for Server CRs.")
	flag.BoolVar(&skipServerCR, "skip-server-cr", false, "Skip creating Server CRs after provisioning.")

	flag.Parse()

	if tailscaleAuthKey == "" {
		die("missing --tailscale-auth-key or TAILSCALE_AUTH_KEY")
	}

	// Validate role
	if role != "dc" && role != "aks-router" {
		die("invalid --role: must be 'dc' or 'aks-router'")
	}

	if len(vmNames) == 0 {
		switch providerName {
		case "azure":
			vmNames = append(vmNames, "stargate-azure-vm")
		case "qemu":
			vmNames = append(vmNames, "stargate-qemu-vm")
		default:
			vmNames = append(vmNames, "stargate-vm")
		}
	}

	switch providerName {
	case "azure":
		if subscriptionID == "" {
			die("missing --subscription-id or AZURE_SUBSCRIPTION_ID")
		}

		ctx := context.Background()

		// Handle based on role
		switch role {
		case "aks-router":
			// Provision only the AKS router in the AKS VNet
			if aksRouterName == "" {
				die("--aks-router-name is required for role 'aks-router'")
			}

			// Auto-detect AKS network CIDRs if cluster name is provided
			var routeCIDRs []string
			var apiServerFQDN string
			if aksClusterName != "" {
				clusterRG := aksClusterRG
				if clusterRG == "" {
					clusterRG = aksClusterName // Default: RG name matches cluster name
				}
				fmt.Printf("[aks] querying cluster %s/%s for network config...\n", clusterRG, aksClusterName)
				aksInfo, err := queryAKSNetworkConfig(ctx, subscriptionID, clusterRG, aksClusterName)
				if err != nil {
					die("query AKS network config: %v", err)
				}
				routeCIDRs = aksInfo.RouteCIDRs
				apiServerFQDN = aksInfo.APIServerFQDN
				if aksVNetCIDR == "" {
					aksVNetCIDR = aksInfo.VNetCIDR
				}
				if aksResourceGroup == "" {
					aksResourceGroup = aksInfo.NodeResourceGroup
				}
				if aksVNetName == "" {
					aksVNetName = aksInfo.VNetName
				}
				fmt.Printf("[aks] detected: VNet=%s RG=%s, CIDRs=%v, API=%s\n", aksVNetName, aksResourceGroup, routeCIDRs, apiServerFQDN)
			} else {
				// Manual mode - require all flags
				if aksResourceGroup == "" {
					die("--aks-resource-group is required (or use --aks-cluster-name for auto-detection)")
				}
				if aksVNetName == "" {
					die("--aks-vnet-name is required (or use --aks-cluster-name for auto-detection)")
				}
				if aksVNetCIDR != "" {
					routeCIDRs = []string{aksVNetCIDR}
				}
			}

			aksRouterCfg := &providers.AKSRouterConfig{
				Name:          aksRouterName,
				ResourceGroup: aksResourceGroup,
				VNetName:      aksVNetName,
				SubnetName:    aksSubnetName,
				SubnetCIDR:    aksSubnetCIDR,
				VNetCIDR:      aksVNetCIDR,
				ClusterName:   aksClusterName,
				ClusterRG:     aksClusterRG,
				RouteCIDRs:    routeCIDRs,
				APIServerFQDN: apiServerFQDN,
			}

			prov, err := azure.NewProvider(ctx, azure.Config{
				SubscriptionID:   subscriptionID,
				Location:         location,
				Zone:             zone,
				ResourceGroup:    aksResourceGroup, // Use AKS resource group
				VMSize:           vmSize,
				AdminUsername:    adminUser,
				SSHPublicKeyPath: sshPubKeyPath,
				TailscaleAuthKey: tailscaleAuthKey,
				AKSRouter:        aksRouterCfg,
			})
			if err != nil {
				die("azure provider init: %v", err)
			}

			// Only create the AKS router
			nodes, err := prov.CreateNodes(ctx, []providers.NodeSpec{
				{Name: aksRouterName, Role: providers.RoleAKSRouter},
			})
			if err != nil {
				die("provision AKS router: %v", err)
			}

			// Run connectivity check for AKS router
			nodes, err = runAKSRouterConnectivityCheck(nodes, adminUser, routeCIDRs)
			if err != nil {
				die("AKS router connectivity check failed: %v", err)
			}

			fmt.Println("AKS router ready and reachable.")
			for _, n := range nodes {
				fmt.Printf("  %s: TailscaleIP=%s PublicIP=%s PrivateIP=%s\n",
					n.Name, n.TailscaleIP, n.PublicIP, n.PrivateIP)
			}

		case "dc":
			// Provision DC router + workers (original behavior)
			var aksRouterCfg *providers.AKSRouterConfig
			var aksNetworkInfo *AKSNetworkInfo // Save for AKS route table creation

			// Auto-detect AKS CIDRs if cluster name is provided (for routing to existing AKS)
			var detectedAKSCIDRs []string
			if aksClusterName != "" && aksRouterName == "" {
				// Query AKS for network CIDRs to route workers through
				clusterRG := aksClusterRG
				if clusterRG == "" {
					clusterRG = aksClusterName
				}
				fmt.Printf("[aks] querying cluster %s/%s for network CIDRs...\n", clusterRG, aksClusterName)
				aksInfo, err := queryAKSNetworkConfig(ctx, subscriptionID, clusterRG, aksClusterName)
				if err != nil {
					die("query AKS network config: %v", err)
				}
				aksNetworkInfo = aksInfo // Save for later use
				detectedAKSCIDRs = aksInfo.RouteCIDRs
				fmt.Printf("[aks] detected CIDRs for worker routing: %v\n", detectedAKSCIDRs)
				fmt.Printf("[aks] AKS subnet: %s/%s\n", aksInfo.VNetName, aksInfo.SubnetName)
			}

			if aksRouterName != "" {
				// Also create AKS router alongside DC resources
				if aksResourceGroup == "" {
					die("--aks-resource-group is required when --aks-router-name is specified")
				}
				if aksVNetName == "" {
					die("--aks-vnet-name is required when --aks-router-name is specified")
				}
				aksRouterCfg = &providers.AKSRouterConfig{
					Name:          aksRouterName,
					ResourceGroup: aksResourceGroup,
					VNetName:      aksVNetName,
					SubnetName:    aksSubnetName,
					SubnetCIDR:    aksSubnetCIDR,
					VNetCIDR:      aksVNetCIDR,
				}
			} else if len(aksRouteCIDRsFlag) > 0 {
				// Manual AKS CIDRs specified via flag
				aksRouterCfg = &providers.AKSRouterConfig{
					RouteCIDRs: aksRouteCIDRsFlag,
				}
			} else if len(detectedAKSCIDRs) > 0 {
				// Auto-detected AKS CIDRs from cluster query
				aksRouterCfg = &providers.AKSRouterConfig{
					RouteCIDRs: detectedAKSCIDRs,
				}
			}

			prov, err := azure.NewProvider(ctx, azure.Config{
				SubscriptionID:   subscriptionID,
				Location:         location,
				Zone:             zone,
				ResourceGroup:    resourceGroup,
				VNetName:         vnetName,
				VNetCIDR:         vnetCIDR,
				SubnetName:       subnetName,
				SubnetCIDR:       subnetCIDR,
				VMSize:           vmSize,
				AdminUsername:    adminUser,
				SSHPublicKeyPath: sshPubKeyPath,
				TailscaleAuthKey: tailscaleAuthKey,
				AKSRouter:        aksRouterCfg,
			})
			if err != nil {
				die("azure provider init: %v", err)
			}

			var specs []providers.NodeSpec
			if routerName != "" {
				specs = append(specs, providers.NodeSpec{Name: routerName, Role: providers.RoleRouter})
			}
			for _, name := range vmNames {
				specs = append(specs, providers.NodeSpec{Name: name, Role: providers.RoleWorker})
			}

			nodes, err := prov.CreateNodes(ctx, specs)
			if err != nil {
				die("provision: %v", err)
			}

			// Run connectivity checks and fetch Tailscale IPs
			nodes, err = runConnectivitySuite(nodes, adminUser, subnetCIDR)
			if err != nil {
				die("connectivity checks failed: %v", err)
			}

			// If we have an AKS router, run additional connectivity check for it
			if aksRouterCfg != nil && len(aksRouterCfg.RouteCIDRs) > 0 {
				nodes, err = runAKSRouterConnectivityCheck(nodes, adminUser, aksRouterCfg.RouteCIDRs)
				if err != nil {
					die("AKS router connectivity check failed: %v", err)
				}
			}

			// Create route table in AKS VNet to route worker traffic through AKS router
			// This enables bidirectional connectivity for kubectl exec/logs
			if aksNetworkInfo != nil && aksNetworkInfo.SubnetID != "" {
				fmt.Println("[aks-rt] setting up AKS VNet route table for bidirectional connectivity...")
				if err := ensureAKSRouteToWorkers(ctx, subscriptionID, aksNetworkInfo, vnetCIDR); err != nil {
					die("create AKS route table: %v", err)
				}
				fmt.Println("[aks-rt] AKS→worker routing configured successfully.")

				// Patch AKS CiliumNodes with podCIDRs (Azure CNI Overlay doesn't set them)
				if kubeconfig != "" {
					if err := patchAKSCiliumNodes(ctx, kubeconfig); err != nil {
						fmt.Printf("warning: could not patch CiliumNodes: %v\n", err)
					}
				}
			}

			fmt.Println("Infrastructure ready and reachable.")

			// Create Server CRs (exclude routers - both regular and AKS routers)
			if !skipServerCR {
				var workerNodes []providers.NodeInfo
				for _, n := range nodes {
					if n.Role != providers.RoleRouter && n.Role != providers.RoleAKSRouter {
						workerNodes = append(workerNodes, n)
					}
				}
				routerProxy := findRouterTarget(nodes)
				if err := createServerCRs(ctx, kubeconfig, namespace, workerNodes, adminUser, providerName, routerProxy); err != nil {
					die("create server CRs: %v", err)
				}
				fmt.Println("Server CRs created successfully.")
			}
		} // end case "dc"

	case "qemu":
		ctx := context.Background()

		// Determine subnet CIDR for QEMU
		qemuSubnet := qemuSubnetCIDR
		if qemuSubnet == "" {
			qemuSubnet = "192.168.100.0/24"
		}

		prov, err := qemu.NewProvider(ctx, qemu.Config{
			WorkDir:          qemuWorkDir,
			ImageCacheDir:    qemuImageCacheDir,
			ImageURL:         qemuImageURL,
			CPUs:             qemuCPUs,
			MemoryMB:         qemuMemoryMB,
			DiskSizeGB:       qemuDiskSizeGB,
			TailscaleAuthKey: tailscaleAuthKey,
			SSHPublicKeyPath: sshPubKeyPath,
			AdminUsername:    adminUser,
			SubnetCIDR:       qemuSubnet,
		})
		if err != nil {
			die("qemu provider init: %v", err)
		}

		var specs []providers.NodeSpec
		if routerName != "" {
			specs = append(specs, providers.NodeSpec{Name: routerName, Role: providers.RoleRouter})
		} else {
			// Default router name for QEMU if not specified
			specs = append(specs, providers.NodeSpec{Name: "stargate-qemu-router", Role: providers.RoleRouter})
		}
		for _, name := range vmNames {
			specs = append(specs, providers.NodeSpec{Name: name, Role: providers.RoleWorker})
		}

		nodes, err := prov.CreateNodes(ctx, specs)
		if err != nil {
			die("provision: %v", err)
		}

		// Run connectivity suite with subnet for route verification
		nodes, err = runConnectivitySuite(nodes, adminUser, qemuSubnet)
		if err != nil {
			die("connectivity checks failed: %v", err)
		}

		fmt.Println("Infrastructure ready and reachable.")

		// Create Server CRs
		if !skipServerCR {
			var workerNodes []providers.NodeInfo
			for _, n := range nodes {
				if n.Role != providers.RoleRouter {
					workerNodes = append(workerNodes, n)
				}
			}
			routerProxy := findRouterTarget(nodes)
			if err := createServerCRs(ctx, kubeconfig, namespace, workerNodes, adminUser, providerName, routerProxy); err != nil {
				die("create server CRs: %v", err)
			}
			fmt.Println("Server CRs created successfully.")
		}

	default:
		die("unsupported provider %q", providerName)
	}
}

func runConnectivitySuite(nodes []providers.NodeInfo, adminUser string, expectedSubnet string) ([]providers.NodeInfo, error) {
	updatedNodes := make([]providers.NodeInfo, len(nodes))
	copy(updatedNodes, nodes)

	routerProxy := findRouterTarget(updatedNodes)

	// First, bring up routers so subnet routes are advertised
	for i, n := range updatedNodes {
		if n.Role != providers.RoleRouter {
			continue
		}

		target := firstNonEmpty(n.TailnetFQDN, n.PublicIP, n.PrivateIP)
		if target == "" {
			return nil, fmt.Errorf("no reachable target for router %s", n.Name)
		}

		fmt.Printf("[connectivity] tailscale ping router %s (%s)...\n", n.Name, target)
		if err := waitTailscalePing(target, 12, 10*time.Second); err != nil {
			return nil, fmt.Errorf("tailscale ping %s: %w", target, err)
		}

		fmt.Printf("[connectivity] ssh %s@%s (router)...\n", adminUser, target)
		if err := waitSSH(adminUser, target, 12, 10*time.Second); err != nil {
			return nil, fmt.Errorf("ssh router %s@%s: %w", adminUser, target, err)
		}

		tailscaleIP, err := fetchTailscaleIP(target, adminUser)
		if err != nil {
			return nil, fmt.Errorf("fetch tailscale ip from router %s: %w", n.Name, err)
		}
		fmt.Printf("[connectivity] router %s tailscale IP: %s\n", n.Name, tailscaleIP)
		updatedNodes[i].TailscaleIP = tailscaleIP

		if expectedSubnet != "" {
			if err := verifyRouterRoute(target, adminUser, expectedSubnet); err != nil {
				return nil, fmt.Errorf("router %s route check: %w", n.Name, err)
			}
		}
	}

	// Then validate workers over reachable addresses (no per-node tailscale expected)
	for i, n := range updatedNodes {
		if n.Role == providers.RoleRouter {
			continue
		}

		target := firstNonEmpty(n.PrivateIP, n.PublicIP, n.TailnetFQDN)
		if target == "" {
			return nil, fmt.Errorf("no reachable target for node %s", n.Name)
		}

		if routerProxy != "" && n.PrivateIP != "" {
			fmt.Printf("[connectivity] ssh %s@%s via router %s...\n", adminUser, target, routerProxy)
			if err := waitSSHViaProxy(adminUser, target, routerProxy, 12, 10*time.Second); err != nil {
				return nil, fmt.Errorf("ssh via router %s -> %s: %w", routerProxy, target, err)
			}
		} else {
			fmt.Printf("[connectivity] ssh %s@%s...\n", adminUser, target)
			if err := waitSSH(adminUser, target, 12, 10*time.Second); err != nil {
				return nil, fmt.Errorf("ssh %s@%s: %w", adminUser, target, err)
			}
		}

		updatedNodes[i].TailscaleIP = n.TailscaleIP // may be empty for workers
	}

	return updatedNodes, nil
}

// runAKSRouterConnectivityCheck validates connectivity to AKS routers and verifies their route advertisements.
func runAKSRouterConnectivityCheck(nodes []providers.NodeInfo, adminUser string, routeCIDRs []string) ([]providers.NodeInfo, error) {
	updatedNodes := make([]providers.NodeInfo, len(nodes))
	copy(updatedNodes, nodes)

	for i, n := range updatedNodes {
		if n.Role != providers.RoleAKSRouter {
			continue
		}

		target := firstNonEmpty(n.TailnetFQDN, n.PublicIP, n.PrivateIP)
		if target == "" {
			return nil, fmt.Errorf("no reachable target for AKS router %s", n.Name)
		}

		fmt.Printf("[connectivity] tailscale ping AKS router %s (%s)...\n", n.Name, target)
		if err := waitTailscalePing(target, 12, 10*time.Second); err != nil {
			return nil, fmt.Errorf("tailscale ping AKS router %s: %w", target, err)
		}

		fmt.Printf("[connectivity] ssh %s@%s (AKS router)...\n", adminUser, target)
		if err := waitSSH(adminUser, target, 12, 10*time.Second); err != nil {
			return nil, fmt.Errorf("ssh AKS router %s@%s: %w", adminUser, target, err)
		}

		tailscaleIP, err := fetchTailscaleIP(target, adminUser)
		if err != nil {
			return nil, fmt.Errorf("fetch tailscale ip from AKS router %s: %w", n.Name, err)
		}
		fmt.Printf("[connectivity] AKS router %s tailscale IP: %s\n", n.Name, tailscaleIP)
		updatedNodes[i].TailscaleIP = tailscaleIP

		// Verify all route CIDRs are advertised
		for _, cidr := range routeCIDRs {
			if err := verifyRouterRoute(target, adminUser, cidr); err != nil {
				return nil, fmt.Errorf("AKS router %s route check for %s: %w", n.Name, cidr, err)
			}
		}
	}

	return updatedNodes, nil
}

func findRouterTarget(nodes []providers.NodeInfo) string {
	for _, n := range nodes {
		if n.Role == providers.RoleRouter {
			// Prefer Tailscale IP over hostname - hostnames may not resolve from controller
			return firstNonEmpty(n.TailscaleIP, n.TailnetFQDN, n.PublicIP)
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func waitTailscalePing(target string, attempts int, delay time.Duration) error {
	for i := 1; i <= attempts; i++ {
		if err := tailscalePing(target); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("tailscale ping to %s did not succeed after %d attempts", target, attempts)
}

func waitSSH(user, host string, attempts int, delay time.Duration) error {
	for i := 1; i <= attempts; i++ {
		if err := sshCheck(user, host); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("ssh to %s@%s did not succeed after %d attempts", user, host, attempts)
}

func waitSSHViaProxy(user, host, proxy string, attempts int, delay time.Duration) error {
	for i := 1; i <= attempts; i++ {
		if err := sshCheckViaProxy(user, host, proxy); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("ssh (proxy %s) to %s@%s did not succeed after %d attempts", proxy, user, host, attempts)
}

func tailscalePing(target string) error {
	cmd := execCommand("tailscale", "ping", "--timeout=5s", "--until-direct=false", target)
	return cmd.Run()
}

// verifyRouterRoute ensures the router advertises and has a primary route for the expected subnet.
func verifyRouterRoute(host, user, subnet string) error {
	if strings.TrimSpace(subnet) == "" {
		return nil
	}

	check := func() (bool, bool, string, error) {
		cmd := execCommand("ssh",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			fmt.Sprintf("%s@%s", user, host),
			"tailscale", "status", "--json",
		)

		out, err := cmd.Output()
		if err != nil {
			return false, false, "", fmt.Errorf("tailscale status --json: %w", err)
		}

		var ts struct {
			Self struct {
				Routes           []string `json:"Routes"`
				AdvertisedRoutes []string `json:"AdvertisedRoutes"`
				PrimaryRoutes    []string `json:"PrimaryRoutes"`
			} `json:"Self"`
			Routes map[string]struct {
				Advertised bool `json:"Advertised"`
				Approved   bool `json:"Approved"`
				Primary    bool `json:"Primary"`
			} `json:"Routes"`
		}

		if err := json.Unmarshal(out, &ts); err != nil {
			return false, false, "", fmt.Errorf("parse tailscale status: %w", err)
		}

		advertised := false
		primary := false

		if route, ok := ts.Routes[subnet]; ok {
			advertised = advertised || route.Advertised
			primary = primary || route.Primary || route.Approved
		}

		for _, r := range ts.Self.AdvertisedRoutes {
			if strings.TrimSpace(r) == subnet {
				advertised = true
			}
		}
		for _, r := range ts.Self.PrimaryRoutes {
			if strings.TrimSpace(r) == subnet {
				primary = true
			}
		}
		for _, r := range ts.Self.Routes {
			if strings.TrimSpace(r) == subnet {
				primary = true
			}
		}

		return advertised, primary, strings.TrimSpace(string(out)), nil
	}

	advertised, primary, snapshot, err := check()
	if err != nil {
		return err
	}

	if !advertised {
		// Attempt to enable advertisement on the router itself
		fmt.Printf("[connectivity] enabling subnet route %s on router %s via tailscale up\n", subnet, host)
		// Ensure forwarding is on before reconfiguring tailscale
		_ = execCommand("ssh",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			fmt.Sprintf("%s@%s", user, host),
			"sudo", "sysctl", "-w", "net.ipv4.ip_forward=1", "net.ipv6.conf.all.forwarding=1",
		).Run()
		cmd := execCommand("ssh",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			fmt.Sprintf("%s@%s", user, host),
			"sudo", "tailscale", "up",
			"--accept-routes",
			fmt.Sprintf("--advertise-routes=%s", subnet),
			fmt.Sprintf("--hostname=%s", host),
			"--snat-subnet-routes=true",
		)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			// Re-check in case the route became advertised despite the non-zero exit
			advertised, primary, snapshot, err = pollCheck(check, 6, 5*time.Second)
			if err != nil {
				return err
			}
			if !advertised {
				return fmt.Errorf("failed to enable advertise-routes on router: %v; output: %s; status: %s", runErr, strings.TrimSpace(string(out)), snapshot)
			}
		}

		advertised, primary, snapshot, err = pollCheck(check, 6, 5*time.Second)
		if err != nil {
			return err
		}
	}

	if !advertised {
		fmt.Printf("[connectivity] retrying tailscale up with --reset for %s on router %s\n", subnet, host)
		cmd := execCommand("ssh",
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			fmt.Sprintf("%s@%s", user, host),
			"sudo", "tailscale", "up", "--reset",
			"--accept-routes",
			fmt.Sprintf("--advertise-routes=%s", subnet),
			fmt.Sprintf("--hostname=%s", host),
			"--snat-subnet-routes=true",
		)
		out, runErr := cmd.CombinedOutput()
		if runErr != nil {
			advertised, primary, snapshot, err = pollCheck(check, 6, 5*time.Second)
			if err != nil {
				return err
			}
			if !advertised {
				return fmt.Errorf("subnet %s not advertised by router after tailscale up --reset; output: %s; status: %s", subnet, strings.TrimSpace(string(out)), snapshot)
			}
		}

		advertised, primary, snapshot, err = pollCheck(check, 6, 5*time.Second)
		if err != nil {
			return err
		}
	}

	if !advertised {
		// Local tailscale status may not show advertised routes until they're approved
		// Try API-based approval which waits for the device to register with the coordination server
		if tsClientID != "" && tsClientSecret != "" {
			fmt.Printf("[connectivity] route %s not visible locally yet - attempting API-based approval for %s\n", subnet, host)
			if err := approveRouteViaAPI(host, subnet); err != nil {
				fmt.Printf("[connectivity] API route approval failed: %v\n", err)
				return fmt.Errorf("subnet %s not advertised; API approval failed: %v; status: %s", subnet, err, snapshot)
			}
			// Poll again to verify the route is now approved
			advertised, primary, snapshot, err = pollCheck(check, 10, 3*time.Second)
			if err != nil {
				return err
			}
			if !primary {
				return fmt.Errorf("subnet %s still not approved/primary after API call; status: %s", subnet, snapshot)
			}
			fmt.Printf("[connectivity] route %s successfully approved via API\n", subnet)
			return nil
		}
		return fmt.Errorf("subnet %s not advertised by router (tailscale status shows no advertised route); set TAILSCALE_CLIENT_ID and TAILSCALE_CLIENT_SECRET for auto-approval; status: %s", subnet, snapshot)
	}
	if !primary {
		// Attempt to approve the route via Tailscale API
		if tsClientID != "" && tsClientSecret != "" {
			fmt.Printf("[connectivity] route %s advertised but not approved - attempting API approval for %s\n", subnet, host)
			if err := approveRouteViaAPI(host, subnet); err != nil {
				fmt.Printf("[connectivity] API route approval failed: %v\n", err)
				return fmt.Errorf("subnet %s advertised but not approved; API approval failed: %v; status: %s", subnet, err, snapshot)
			}
			// Poll again to verify the route is now approved
			advertised, primary, snapshot, err = pollCheck(check, 10, 3*time.Second)
			if err != nil {
				return err
			}
			if !primary {
				return fmt.Errorf("subnet %s still not approved/primary after API call; status: %s", subnet, snapshot)
			}
			fmt.Printf("[connectivity] route %s successfully approved via API\n", subnet)
		} else {
			return fmt.Errorf("subnet %s advertised but not approved/primary yet; set TAILSCALE_CLIENT_ID and TAILSCALE_CLIENT_SECRET for auto-approval, or approve manually in the Tailscale admin console; status: %s", subnet, snapshot)
		}
	}

	return nil
}

// pollCheck repeatedly invokes the provided check function with delay until attempts exhausted.
func pollCheck(check func() (bool, bool, string, error), attempts int, delay time.Duration) (bool, bool, string, error) {
	var advertised, primary bool
	var snapshot string
	var err error
	for i := 0; i < attempts; i++ {
		advertised, primary, snapshot, err = check()
		if err == nil {
			return advertised, primary, snapshot, nil
		}
		time.Sleep(delay)
	}
	return advertised, primary, snapshot, err
}

func waitPing(target string, attempts int, delay time.Duration) error {
	for i := 1; i <= attempts; i++ {
		cmd := execCommand("ping", "-c", "1", "-W", "5", target)
		if err := cmd.Run(); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("ping to %s did not succeed after %d attempts", target, attempts)
}

func sshCheck(user, host string) error {
	cmd := execCommand("ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("%s@%s", user, host),
		"echo", "ok",
	)
	return cmd.Run()
}

func sshCheckViaProxy(user, host, proxy string) error {
	proxyCmd := fmt.Sprintf("ssh -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -W %%h:%%p %s@%s", user, proxy)
	cmd := execCommand("ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		"-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd),
		fmt.Sprintf("%s@%s", user, host),
		"echo", "ok",
	)
	return cmd.Run()
}

// fetchTailscaleIP retrieves the Tailscale IPv4 address from a remote host
func fetchTailscaleIP(host, user string) (string, error) {
	cmd := execCommand("ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@%s", user, host),
		"tailscale", "ip", "-4",
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run tailscale ip: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("empty tailscale ip returned")
	}
	// Take only the first IP if multiple are returned
	lines := strings.Split(ip, "\n")
	return strings.TrimSpace(lines[0]), nil
}

func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// installCRDs installs Stargate CRDs on the target cluster using kubectl
func installCRDs(kubeconfigPath string) error {
	// Find the CRD directory relative to the binary or current working directory
	crdDir := "config/crd/bases"

	// Check if CRD directory exists
	if _, err := os.Stat(crdDir); os.IsNotExist(err) {
		// Try relative to executable
		exePath, err := os.Executable()
		if err == nil {
			crdDir = filepath.Join(filepath.Dir(exePath), "..", "config", "crd", "bases")
		}
	}

	// Check again
	if _, err := os.Stat(crdDir); os.IsNotExist(err) {
		return fmt.Errorf("CRD directory not found at %s - run from project root or install CRDs manually with: kubectl apply -f config/crd/bases/", crdDir)
	}

	fmt.Printf("[crd] installing Stargate CRDs from %s\n", crdDir)

	cmd := exec.Command("kubectl", "--kubeconfig", kubeconfigPath, "apply", "-f", crdDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("install CRDs: %w", err)
	}

	fmt.Printf("[crd] CRDs installed successfully\n")
	return nil
}

// createServerCRs creates Server custom resources for each provisioned node
func createServerCRs(ctx context.Context, kubeconfigPath, namespace string, nodes []providers.NodeInfo, adminUser, providerName string, routerProxy string) error {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	switch providerName {
	case "azure", "qemu":
	default:
		return fmt.Errorf("unsupported provider for Server CRs: %s", providerName)
	}

	// Install CRDs first (idempotent - will update if already exist)
	if err := installCRDs(kubeconfigPath); err != nil {
		return fmt.Errorf("install CRDs: %w", err)
	}

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	// Ensure namespace exists
	nsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	ns := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": namespace,
			},
		},
	}
	_, err = dynClient.Resource(nsGVR).Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		// Namespace doesn't exist, create it
		fmt.Printf("[server-cr] creating namespace %s\n", namespace)
		_, err = dynClient.Resource(nsGVR).Create(ctx, ns, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create namespace %s: %w", namespace, err)
		}
	}

	serverGVR := schema.GroupVersionResource{
		Group:    "stargate.io",
		Version:  "v1alpha1",
		Resource: "servers",
	}

	for _, node := range nodes {
		// Fetch MAC address via SSH
		mac, err := fetchMACAddress(node, adminUser, routerProxy)
		if err != nil {
			return fmt.Errorf("fetch MAC for %s: %w", node.Name, err)
		}

		// Prefer LAN IP (workers sit behind subnet router); fall back to tailscale if present
		ipv4 := node.PrivateIP
		if ipv4 == "" {
			ipv4 = node.TailscaleIP
		}

		// Build the Server CR with router info for workers
		serverSpec := map[string]interface{}{
			"mac":      mac,
			"ipv4":     ipv4,
			"provider": providerName,
		}
		// Add router IP for workers so the controller knows how to SSH via proxy
		if routerProxy != "" && node.Role != providers.RoleRouter {
			serverSpec["routerIP"] = routerProxy
		}

		// Build the Server CR
		server := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "stargate.io/v1alpha1",
				"kind":       "Server",
				"metadata": map[string]interface{}{
					"name":      node.Name,
					"namespace": namespace,
				},
				"spec": serverSpec,
			},
		}

		// Check if it already exists
		existing, err := dynClient.Resource(serverGVR).Namespace(namespace).Get(ctx, node.Name, metav1.GetOptions{})
		if err == nil {
			// Update existing
			server.SetResourceVersion(existing.GetResourceVersion())
			_, err = dynClient.Resource(serverGVR).Namespace(namespace).Update(ctx, server, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("update Server CR %s: %w", node.Name, err)
			}
			fmt.Printf("[server-cr] updated Server %s/%s\n", namespace, node.Name)
		} else {
			// Create new
			_, err = dynClient.Resource(serverGVR).Namespace(namespace).Create(ctx, server, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("create Server CR %s: %w", node.Name, err)
			}
			fmt.Printf("[server-cr] created Server %s/%s\n", namespace, node.Name)
		}
	}

	return nil
}

// fetchMACAddress retrieves the primary MAC address from the node via SSH (optionally via router proxy)
func fetchMACAddress(node providers.NodeInfo, adminUser string, routerProxy string) (string, error) {
	// Prefer private for workers (will proxy via router), then public, then tailscale
	target := firstNonEmpty(node.PrivateIP, node.PublicIP, node.TailscaleIP, node.TailnetFQDN)
	if target == "" {
		return "", fmt.Errorf("no reachable address for node %s", node.Name)
	}

	if routerProxy != "" && node.Role != providers.RoleRouter && node.PrivateIP != "" {
		fmt.Printf("[mac-fetch] SSHing to %s@%s via router %s to get MAC address...\n", adminUser, target, routerProxy)
	} else {
		fmt.Printf("[mac-fetch] SSHing to %s@%s to get MAC address...\n", adminUser, target)
	}

	// Use explicit SSH key to avoid Tailscale SSH
	sshKeyPath := filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa")
	// When running as sudo, HOME might be root's home, check SUDO_USER
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		sshKeyPath = filepath.Join("/home", sudoUser, ".ssh", "id_rsa")
	}

	sshBase := []string{"-i", sshKeyPath, "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null", "-o", "ConnectTimeout=10"}
	makeSSH := func(cmd ...string) *exec.Cmd {
		args := append([]string{}, sshBase...)
		if routerProxy != "" && node.Role != providers.RoleRouter && node.PrivateIP != "" {
			proxyCmd := fmt.Sprintf("ssh -i %s -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -W %%h:%%p %s@%s", sshKeyPath, adminUser, routerProxy)
			args = append(args, "-o", fmt.Sprintf("ProxyCommand=%s", proxyCmd))
		}
		args = append(args, fmt.Sprintf("%s@%s", adminUser, target))
		args = append(args, cmd...)
		return exec.Command("ssh", args...)
	}

	// First, try to find the primary non-loopback interface dynamically
	findIfaceCmd := makeSSH("sh", "-c", "ls /sys/class/net | grep -v lo | head -1")

	ifaceOut, err := findIfaceCmd.Output()
	if err == nil {
		iface := strings.TrimSpace(string(ifaceOut))
		if iface != "" {
			macCmd := makeSSH("cat", fmt.Sprintf("/sys/class/net/%s/address", iface))
			out, err := macCmd.Output()
			if err == nil {
				mac := strings.TrimSpace(string(out))
				if mac != "" {
					fmt.Printf("[mac-fetch] Got MAC %s from interface %s\n", mac, iface)
					return mac, nil
				}
			}
		}
	}

	// Fallback: try known interface names
	interfaces := []string{"eth0", "enp0s2", "enp0s3", "enp1s0", "ens3", "ens4", "ens5", "ens160", "ens192"}
	for _, iface := range interfaces {
		cmd := makeSSH("cat", fmt.Sprintf("/sys/class/net/%s/address", iface))

		out, err := cmd.Output()
		if err == nil {
			mac := strings.TrimSpace(string(out))
			if mac != "" {
				fmt.Printf("[mac-fetch] Got MAC %s from interface %s\n", mac, iface)
				return mac, nil
			}
		}
	}

	return "", fmt.Errorf("could not find MAC address on any interface")
}

// getTailscaleOAuthToken fetches an OAuth access token from Tailscale API
func getTailscaleOAuthToken() (string, error) {
	if tsClientID == "" || tsClientSecret == "" {
		return "", fmt.Errorf("TAILSCALE_CLIENT_ID or TAILSCALE_CLIENT_SECRET not set")
	}

	data := url.Values{}
	data.Set("grant_type", "client_credentials")

	req, err := http.NewRequest("POST", "https://api.tailscale.com/api/v2/oauth/token", strings.NewReader(data.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(tsClientID, tsClientSecret)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("oauth token request failed: %s: %s", resp.Status, string(body))
	}

	var tokenResp struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("parse oauth response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", fmt.Errorf("no access token in response")
	}
	return tokenResp.AccessToken, nil
}

// findTailscaleDeviceID finds a device ID by hostname using the Tailscale API
func findTailscaleDeviceID(token, hostname string) (string, error) {
	req, err := http.NewRequest("GET", "https://api.tailscale.com/api/v2/tailnet/-/devices", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("list devices failed: %s: %s", resp.Status, string(body))
	}

	var devicesResp struct {
		Devices []struct {
			ID       string `json:"id"`
			Hostname string `json:"hostname"`
			Name     string `json:"name"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(body, &devicesResp); err != nil {
		return "", fmt.Errorf("parse devices response: %w", err)
	}

	// Match by hostname (case-insensitive)
	hostnameLower := strings.ToLower(hostname)
	for _, d := range devicesResp.Devices {
		if strings.ToLower(d.Hostname) == hostnameLower {
			return d.ID, nil
		}
		// Also check name (which includes domain)
		if strings.HasPrefix(strings.ToLower(d.Name), hostnameLower+".") {
			return d.ID, nil
		}
	}
	return "", fmt.Errorf("device with hostname %q not found", hostname)
}

// approveDeviceRoutes enables/approves all advertised routes on a device using the Tailscale API
func approveDeviceRoutes(token, deviceID string, routesToApprove []string) error {
	// First, get current routes for the device
	req, err := http.NewRequest("GET", fmt.Sprintf("https://api.tailscale.com/api/v2/device/%s/routes", deviceID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("get device routes failed: %s: %s", resp.Status, string(body))
	}

	var routesResp struct {
		AdvertisedRoutes []string `json:"advertisedRoutes"`
		EnabledRoutes    []string `json:"enabledRoutes"`
	}
	if err := json.Unmarshal(body, &routesResp); err != nil {
		return fmt.Errorf("parse routes response: %w", err)
	}

	// Build list of routes to enable: existing enabled + any advertised routes we want to approve
	enabledSet := make(map[string]bool)
	for _, r := range routesResp.EnabledRoutes {
		enabledSet[r] = true
	}
	for _, r := range routesToApprove {
		enabledSet[r] = true
	}
	// Also enable all currently advertised routes
	for _, r := range routesResp.AdvertisedRoutes {
		enabledSet[r] = true
	}

	var newEnabled []string
	for r := range enabledSet {
		newEnabled = append(newEnabled, r)
	}

	// POST to enable routes
	payload := struct {
		Routes []string `json:"routes"`
	}{Routes: newEnabled}

	payloadBytes, _ := json.Marshal(payload)
	req, err = http.NewRequest("POST", fmt.Sprintf("https://api.tailscale.com/api/v2/device/%s/routes", deviceID), bytes.NewReader(payloadBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("enable routes failed: %s: %s", resp.Status, string(body))
	}

	fmt.Printf("[tailscale-api] routes enabled for device %s: %v\n", deviceID, newEnabled)
	return nil
}

// approveRouteViaAPI attempts to approve a subnet route using the Tailscale API
// It waits for the device to appear in the tailnet and have the route advertised before approving
func approveRouteViaAPI(hostname, subnet string) error {
	token, err := getTailscaleOAuthToken()
	if err != nil {
		return fmt.Errorf("get oauth token: %w", err)
	}

	// Wait for device to appear in tailnet
	var deviceID string
	fmt.Printf("[tailscale-api] waiting for device %s to appear in tailnet...\n", hostname)
	for i := 0; i < 30; i++ {
		deviceID, err = findTailscaleDeviceID(token, hostname)
		if err == nil && deviceID != "" {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if deviceID == "" {
		return fmt.Errorf("device %s did not appear in tailnet after 150s", hostname)
	}
	fmt.Printf("[tailscale-api] found device %s with ID %s\n", hostname, deviceID)

	// Wait for the route to be advertised
	fmt.Printf("[tailscale-api] waiting for route %s to be advertised by device...\n", subnet)
	for i := 0; i < 30; i++ {
		advertised, err := getDeviceAdvertisedRoutes(token, deviceID)
		if err != nil {
			fmt.Printf("[tailscale-api] error checking routes (attempt %d): %v\n", i+1, err)
			time.Sleep(5 * time.Second)
			continue
		}
		for _, r := range advertised {
			if r == subnet {
				fmt.Printf("[tailscale-api] route %s is now advertised, approving...\n", subnet)
				return approveDeviceRoutes(token, deviceID, []string{subnet})
			}
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("route %s was not advertised by device %s after 150s", subnet, hostname)
}

// getDeviceAdvertisedRoutes returns the list of advertised routes for a device
func getDeviceAdvertisedRoutes(token, deviceID string) ([]string, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://api.tailscale.com/api/v2/device/%s/routes", deviceID), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("get device routes failed: %s: %s", resp.Status, string(body))
	}

	var routesResp struct {
		AdvertisedRoutes []string `json:"advertisedRoutes"`
	}
	if err := json.Unmarshal(body, &routesResp); err != nil {
		return nil, fmt.Errorf("parse routes response: %w", err)
	}
	return routesResp.AdvertisedRoutes, nil
}

// AKSNetworkInfo holds auto-detected AKS network configuration.
type AKSNetworkInfo struct {
	VNetCIDR          string
	VNetName          string
	VNetID            string // Full resource ID of the VNet
	SubnetName        string // Name of the AKS subnet
	SubnetID          string // Full resource ID of the AKS subnet
	NodeResourceGroup string
	PodCIDR           string
	ServiceCIDR       string
	APIServerFQDN     string
	RouteCIDRs        []string // All CIDRs to advertise
}

// queryAKSNetworkConfig queries an AKS cluster for its network configuration.
func queryAKSNetworkConfig(ctx context.Context, subscriptionID, resourceGroup, clusterName string) (*AKSNetworkInfo, error) {
	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure CLI credential: %w", err)
	}

	// Get AKS cluster info
	aksClient, err := armcontainerservice.NewManagedClustersClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create AKS client: %w", err)
	}

	cluster, err := aksClient.Get(ctx, resourceGroup, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("get AKS cluster: %w", err)
	}

	info := &AKSNetworkInfo{}

	// Get node resource group
	if cluster.Properties.NodeResourceGroup != nil {
		info.NodeResourceGroup = *cluster.Properties.NodeResourceGroup
	}

	// Get API server FQDN
	if cluster.Properties.Fqdn != nil {
		info.APIServerFQDN = *cluster.Properties.Fqdn
	}

	// Get network profile
	if np := cluster.Properties.NetworkProfile; np != nil {
		if np.PodCidr != nil && *np.PodCidr != "" {
			info.PodCIDR = *np.PodCidr
		} else if len(np.PodCidrs) > 0 {
			info.PodCIDR = *np.PodCidrs[0]
		}
		if np.ServiceCidr != nil && *np.ServiceCidr != "" {
			info.ServiceCIDR = *np.ServiceCidr
		} else if len(np.ServiceCidrs) > 0 {
			info.ServiceCIDR = *np.ServiceCidrs[0]
		}
	}

	// Get VNet info from node resource group
	vnetClient, err := armnetwork.NewVirtualNetworksClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("create vnet client: %w", err)
	}

	pager := vnetClient.NewListPager(info.NodeResourceGroup, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("list vnets: %w", err)
		}
		for _, vnet := range page.Value {
			if vnet.Name != nil {
				info.VNetName = *vnet.Name
			}
			if vnet.ID != nil {
				info.VNetID = *vnet.ID
			}
			if vnet.Properties != nil && vnet.Properties.AddressSpace != nil {
				for _, prefix := range vnet.Properties.AddressSpace.AddressPrefixes {
					if prefix != nil {
						info.VNetCIDR = *prefix
						break
					}
				}
			}
			// Find the main AKS subnet (prefer "aks-subnet" or largest subnet)
			if vnet.Properties != nil && len(vnet.Properties.Subnets) > 0 {
				var bestSubnet *armnetwork.Subnet
				for _, subnet := range vnet.Properties.Subnets {
					if subnet.Name == nil || subnet.ID == nil {
						continue
					}
					// Skip stargate router subnets
					if strings.Contains(*subnet.Name, "stargate") {
						continue
					}
					// Prefer exactly "aks-subnet"
					if *subnet.Name == "aks-subnet" {
						bestSubnet = subnet
						break
					}
					// Otherwise take first non-gateway subnet
					if bestSubnet == nil && !strings.Contains(strings.ToLower(*subnet.Name), "gateway") {
						bestSubnet = subnet
					}
				}
				if bestSubnet != nil {
					info.SubnetName = *bestSubnet.Name
					info.SubnetID = *bestSubnet.ID
				}
			}
			break // Use first VNet
		}
	}

	// Build route CIDRs list
	// Include VNet CIDR for node-to-node connectivity
	if info.VNetCIDR != "" {
		info.RouteCIDRs = append(info.RouteCIDRs, info.VNetCIDR)
	}
	// Include Service CIDR for cluster services
	if info.ServiceCIDR != "" {
		info.RouteCIDRs = append(info.RouteCIDRs, info.ServiceCIDR)
	}

	// IMPORTANT: Do NOT include the cluster-wide PodCIDR (e.g., 10.244.0.0/16)
	// This causes routing conflicts on the DC router - when DC router receives this
	// broad route via Tailscale, it will route DC worker pods (e.g., 10.244.65.0/24)
	// back through Tailscale instead of locally to the workers.
	//
	// Instead, query the Kubernetes API for specific AKS node pod CIDRs and include
	// only those. The route sync controller will handle dynamic updates.
	nodePodCIDRs, err := getAKSNodePodCIDRs(ctx, resourceGroup, clusterName)
	if err != nil {
		// Fall back to NOT advertising any pod CIDRs - route sync will handle it
		fmt.Printf("[warning] could not get AKS node pod CIDRs: %v (route sync will handle)\n", err)
	} else {
		for _, cidr := range nodePodCIDRs {
			info.RouteCIDRs = append(info.RouteCIDRs, cidr)
		}
		fmt.Printf("[aks] discovered AKS node pod CIDRs: %v\n", nodePodCIDRs)
	}

	return info, nil
}

// getAKSNodePodCIDRs queries the Kubernetes API to get specific pod CIDRs for each AKS node.
// This returns per-node /24 CIDRs instead of the cluster-wide /16 to avoid routing conflicts.
func getAKSNodePodCIDRs(ctx context.Context, resourceGroup, clusterName string) ([]string, error) {
	// Use kubectl to get node pod CIDRs (requires kubeconfig to be set up)
	cmd := exec.CommandContext(ctx, "kubectl", "get", "nodes", "-o", "jsonpath={range .items[*]}{.spec.podCIDR}{\"\\n\"}{end}")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("kubectl get nodes: %w", err)
	}

	var podCIDRs []string
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		cidr := strings.TrimSpace(line)
		if cidr != "" {
			podCIDRs = append(podCIDRs, cidr)
		}
	}

	if len(podCIDRs) == 0 {
		return nil, fmt.Errorf("no pod CIDRs found on nodes")
	}

	return podCIDRs, nil
}

// ensureAKSRouteToWorkers creates or updates route tables in the AKS VNet:
// 1. stargate-workers-rt: routes DC worker VNet and pod CIDRs via AKS router (applied to aks-subnet)
// 2. stargate-router-rt: routes AKS node pod CIDRs to their respective nodes (applied to router subnet)
// This enables bidirectional pod-to-pod connectivity between AKS and DC workers.
func ensureAKSRouteToWorkers(ctx context.Context, subscriptionID string, aksInfo *AKSNetworkInfo, workerVNetCIDR string) error {
	if aksInfo.SubnetID == "" {
		return fmt.Errorf("AKS subnet ID not available - cannot create route table")
	}

	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		return fmt.Errorf("create Azure CLI credential: %w", err)
	}

	// Find the stargate router VM in the AKS VNet (by looking for stargate-* NICs)
	nicClient, err := armnetwork.NewInterfacesClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create NIC client: %w", err)
	}

	var aksRouterIP string
	var aksRouterSubnetID string
	nicPager := nicClient.NewListPager(aksInfo.NodeResourceGroup, nil)
	for nicPager.More() {
		page, err := nicPager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list NICs: %w", err)
		}
		for _, nic := range page.Value {
			// Look for stargate or aks-router NICs (the Tailscale router in AKS VNet)
			nicName := ""
			if nic.Name != nil {
				nicName = *nic.Name
			}
			isRouterNIC := strings.HasPrefix(nicName, "stargate-") || strings.HasPrefix(nicName, "aks-router")
			if isRouterNIC {
				if nic.Properties != nil && len(nic.Properties.IPConfigurations) > 0 {
					for _, ipCfg := range nic.Properties.IPConfigurations {
						if ipCfg.Properties != nil && ipCfg.Properties.PrivateIPAddress != nil {
							aksRouterIP = *ipCfg.Properties.PrivateIPAddress
							if ipCfg.Properties.Subnet != nil && ipCfg.Properties.Subnet.ID != nil {
								aksRouterSubnetID = *ipCfg.Properties.Subnet.ID
							}
							fmt.Printf("[aks-rt] found AKS router NIC %s with IP %s\n", nicName, aksRouterIP)
							break
						}
					}
				}
				break
			}
		}
		if aksRouterIP != "" {
			break
		}
	}

	if aksRouterIP == "" {
		return fmt.Errorf("could not find stargate router VM in AKS VNet - ensure aks-router was provisioned first")
	}

	// Collect AKS node IPs and their pod CIDRs for routing
	// AKS nodes are in VMSS, each gets a /24 from the pod CIDR
	type aksNodeInfo struct {
		Name    string
		IP      string
		PodCIDR string
	}
	var aksNodes []aksNodeInfo

	// List NICs to find AKS node IPs (VMSS NICs have pattern aks-*-vmss*)
	nicPager = nicClient.NewListPager(aksInfo.NodeResourceGroup, nil)
	nodeIndex := 0
	for nicPager.More() {
		page, err := nicPager.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("list NICs for AKS nodes: %w", err)
		}
		for _, nic := range page.Value {
			nicName := ""
			if nic.Name != nil {
				nicName = *nic.Name
			}
			// Skip stargate routers, only look for VMSS NICs
			if strings.HasPrefix(nicName, "stargate-") || strings.HasPrefix(nicName, "aks-router") {
				continue
			}
			// VMSS NICs typically have format like "aks-nodepool1-12345678-vmss_0"
			if !strings.Contains(nicName, "vmss") {
				continue
			}
			if nic.Properties != nil && len(nic.Properties.IPConfigurations) > 0 {
				for _, ipCfg := range nic.Properties.IPConfigurations {
					if ipCfg.Properties != nil && ipCfg.Properties.PrivateIPAddress != nil {
						nodeIP := *ipCfg.Properties.PrivateIPAddress
						// Azure CNI Overlay assigns /24 per node starting from 10.244.0.0/24
						podCIDR := fmt.Sprintf("10.244.%d.0/24", nodeIndex)
						aksNodes = append(aksNodes, aksNodeInfo{
							Name:    nicName,
							IP:      nodeIP,
							PodCIDR: podCIDR,
						})
						fmt.Printf("[aks-rt] found AKS node %s with IP %s, assigning pod CIDR %s\n", nicName, nodeIP, podCIDR)
						nodeIndex++
						break
					}
				}
			}
		}
	}

	// Create/update route table
	rtClient, err := armnetwork.NewRouteTablesClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create route table client: %w", err)
	}

	// Get subnet client
	subnetClient, err := armnetwork.NewSubnetsClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create subnet client: %w", err)
	}

	// Get VNet to determine location
	vnetClient, err := armnetwork.NewVirtualNetworksClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create vnet client: %w", err)
	}

	vnet, err := vnetClient.Get(ctx, aksInfo.NodeResourceGroup, aksInfo.VNetName, nil)
	if err != nil {
		return fmt.Errorf("get AKS VNet: %w", err)
	}

	location := ""
	if vnet.Location != nil {
		location = *vnet.Location
	}

	// ========================================
	// Part 1: Create stargate-workers-rt for AKS subnet
	// Routes DC worker VNet and pod CIDRs via AKS router
	// ========================================
	rtName := "stargate-workers-rt"
	fmt.Printf("[aks-rt] ensuring route table %s in %s...\n", rtName, aksInfo.NodeResourceGroup)

	pollerRT, err := rtClient.BeginCreateOrUpdate(ctx, aksInfo.NodeResourceGroup, rtName, armnetwork.RouteTable{
		Location: to.Ptr(location),
		Properties: &armnetwork.RouteTablePropertiesFormat{
			DisableBgpRoutePropagation: to.Ptr(false),
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("create route table: %w", err)
	}

	rt, err := pollerRT.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("wait for route table: %w", err)
	}
	fmt.Printf("[aks-rt] route table created: %s\n", *rt.ID)

	routeClient, err := armnetwork.NewRoutesClient(subscriptionID, cred, nil)
	if err != nil {
		return fmt.Errorf("create routes client: %w", err)
	}

	// Add route for worker VNet
	routeName := "to-workers"
	fmt.Printf("[aks-rt] adding route %s -> %s via %s...\n", routeName, workerVNetCIDR, aksRouterIP)

	pollerRoute, err := routeClient.BeginCreateOrUpdate(ctx, aksInfo.NodeResourceGroup, rtName, routeName, armnetwork.Route{
		Properties: &armnetwork.RoutePropertiesFormat{
			AddressPrefix:    to.Ptr(workerVNetCIDR),
			NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
			NextHopIPAddress: to.Ptr(aksRouterIP),
		},
	}, nil)
	if err != nil {
		return fmt.Errorf("create route: %w", err)
	}

	if _, err := pollerRoute.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for route: %w", err)
	}

	// Add routes for DC worker pod CIDRs (10.244.50-250.0/24 range based on derivePodCIDR logic)
	// These use the same formula as derivePodCIDR: (third_octet * 10 + fourth_octet) % 200 + 50
	// For typical DC workers in 10.X.1.5, 10.X.1.6, etc., we need to add their pod CIDRs
	// We derive based on workerVNetCIDR - extract the third octet
	workerSubnetParts := strings.Split(workerVNetCIDR, ".")
	if len(workerSubnetParts) >= 3 {
		thirdOctet := 1 // Default for /24 worker subnet
		if len(workerSubnetParts) >= 3 {
			fmt.Sscanf(workerSubnetParts[2], "%d", &thirdOctet)
		}
		// Add routes for workers .5 through .10 (typical worker range)
		for fourthOctet := 5; fourthOctet <= 10; fourthOctet++ {
			uniqueOctet := (thirdOctet*10+fourthOctet)%200 + 50
			podCIDR := fmt.Sprintf("10.244.%d.0/24", uniqueOctet)
			routeName := fmt.Sprintf("pod-cidr-worker-%d", fourthOctet-4)
			fmt.Printf("[aks-rt] adding DC worker pod route %s -> %s via %s...\n", routeName, podCIDR, aksRouterIP)

			pollerRoute, err := routeClient.BeginCreateOrUpdate(ctx, aksInfo.NodeResourceGroup, rtName, routeName, armnetwork.Route{
				Properties: &armnetwork.RoutePropertiesFormat{
					AddressPrefix:    to.Ptr(podCIDR),
					NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
					NextHopIPAddress: to.Ptr(aksRouterIP),
				},
			}, nil)
			if err != nil {
				fmt.Printf("[aks-rt] warning: failed to add route %s: %v\n", routeName, err)
				continue
			}
			if _, err := pollerRoute.PollUntilDone(ctx, nil); err != nil {
				fmt.Printf("[aks-rt] warning: failed to wait for route %s: %v\n", routeName, err)
			}
		}
	}
	fmt.Printf("[aks-rt] DC worker pod routes added\n")

	// Associate route table with AKS subnet
	subnet, err := subnetClient.Get(ctx, aksInfo.NodeResourceGroup, aksInfo.VNetName, aksInfo.SubnetName, nil)
	if err != nil {
		return fmt.Errorf("get AKS subnet: %w", err)
	}

	fmt.Printf("[aks-rt] associating route table with subnet %s...\n", aksInfo.SubnetName)

	subnet.Properties.RouteTable = &armnetwork.RouteTable{
		ID: rt.ID,
	}

	pollerSubnet, err := subnetClient.BeginCreateOrUpdate(ctx, aksInfo.NodeResourceGroup, aksInfo.VNetName, aksInfo.SubnetName, subnet.Subnet, nil)
	if err != nil {
		return fmt.Errorf("update subnet: %w", err)
	}

	if _, err := pollerSubnet.PollUntilDone(ctx, nil); err != nil {
		return fmt.Errorf("wait for subnet update: %w", err)
	}
	fmt.Printf("[aks-rt] route table associated with AKS subnet\n")

	// ========================================
	// Part 2: Create stargate-router-rt for AKS router subnet
	// Routes AKS node pod CIDRs to their respective nodes (for return traffic)
	// ========================================
	if aksRouterSubnetID != "" && len(aksNodes) > 0 {
		routerRTName := "stargate-router-rt"
		fmt.Printf("[aks-rt] ensuring route table %s for router subnet...\n", routerRTName)

		pollerRouterRT, err := rtClient.BeginCreateOrUpdate(ctx, aksInfo.NodeResourceGroup, routerRTName, armnetwork.RouteTable{
			Location: to.Ptr(location),
			Properties: &armnetwork.RouteTablePropertiesFormat{
				DisableBgpRoutePropagation: to.Ptr(false),
			},
		}, nil)
		if err != nil {
			return fmt.Errorf("create router route table: %w", err)
		}

		routerRT, err := pollerRouterRT.PollUntilDone(ctx, nil)
		if err != nil {
			return fmt.Errorf("wait for router route table: %w", err)
		}
		fmt.Printf("[aks-rt] router route table created: %s\n", *routerRT.ID)

		// Add routes for each AKS node's pod CIDR
		for i, node := range aksNodes {
			routeName := fmt.Sprintf("pod-cidr-node-%d", i)
			fmt.Printf("[aks-rt] adding AKS node route %s -> %s via %s...\n", routeName, node.PodCIDR, node.IP)

			pollerRoute, err := routeClient.BeginCreateOrUpdate(ctx, aksInfo.NodeResourceGroup, routerRTName, routeName, armnetwork.Route{
				Properties: &armnetwork.RoutePropertiesFormat{
					AddressPrefix:    to.Ptr(node.PodCIDR),
					NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
					NextHopIPAddress: to.Ptr(node.IP),
				},
			}, nil)
			if err != nil {
				fmt.Printf("[aks-rt] warning: failed to add route %s: %v\n", routeName, err)
				continue
			}
			if _, err := pollerRoute.PollUntilDone(ctx, nil); err != nil {
				fmt.Printf("[aks-rt] warning: failed to wait for route %s: %v\n", routeName, err)
			}
		}

		// Extract router subnet name from ID
		// Format: /subscriptions/.../subnets/<subnet-name>
		parts := strings.Split(aksRouterSubnetID, "/")
		routerSubnetName := parts[len(parts)-1]

		fmt.Printf("[aks-rt] associating router route table with subnet %s...\n", routerSubnetName)

		routerSubnet, err := subnetClient.Get(ctx, aksInfo.NodeResourceGroup, aksInfo.VNetName, routerSubnetName, nil)
		if err != nil {
			fmt.Printf("[aks-rt] warning: could not get router subnet: %v\n", err)
		} else {
			routerSubnet.Properties.RouteTable = &armnetwork.RouteTable{
				ID: routerRT.ID,
			}

			pollerRouterSubnet, err := subnetClient.BeginCreateOrUpdate(ctx, aksInfo.NodeResourceGroup, aksInfo.VNetName, routerSubnetName, routerSubnet.Subnet, nil)
			if err != nil {
				fmt.Printf("[aks-rt] warning: could not update router subnet: %v\n", err)
			} else if _, err := pollerRouterSubnet.PollUntilDone(ctx, nil); err != nil {
				fmt.Printf("[aks-rt] warning: failed to wait for router subnet update: %v\n", err)
			} else {
				fmt.Printf("[aks-rt] router route table associated with router subnet\n")
			}
		}
	}

	return nil
}

// patchAKSCiliumNodes patches CiliumNode resources for AKS nodes to set their podCIDRs.
// Azure CNI Overlay doesn't set podCIDRs on Node resources, but Cilium needs them for routing.
// This function assigns unique /24 pod CIDRs from 10.244.0.0/16 to each AKS node.
func patchAKSCiliumNodes(ctx context.Context, kubeconfig string) error {
	fmt.Println("[cilium] patching AKS CiliumNodes with podCIDRs...")

	// Build kubeconfig
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return fmt.Errorf("build kubeconfig: %w", err)
	}

	// Create dynamic client
	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	// CiliumNode GVR
	ciliumNodeGVR := schema.GroupVersionResource{
		Group:    "cilium.io",
		Version:  "v2",
		Resource: "ciliumnodes",
	}

	// List CiliumNodes
	ciliumNodes, err := dynClient.Resource(ciliumNodeGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("list ciliumnodes: %w", err)
	}

	if len(ciliumNodes.Items) == 0 {
		fmt.Println("[cilium] no CiliumNodes found, skipping")
		return nil
	}

	fmt.Printf("[cilium] found %d CiliumNodes\n", len(ciliumNodes.Items))

	// Assign pod CIDRs to each node (10.244.0.0/24, 10.244.1.0/24, etc.)
	for i, cn := range ciliumNodes.Items {
		nodeName := cn.GetName()

		// Check if already has a podCIDR in spec.ipam.podCIDRs
		spec, ok := cn.Object["spec"].(map[string]interface{})
		if ok {
			ipam, ok := spec["ipam"].(map[string]interface{})
			if ok {
				podCIDRs, ok := ipam["podCIDRs"].([]interface{})
				if ok && len(podCIDRs) > 0 {
					fmt.Printf("[cilium] node %s already has podCIDR: %v, skipping\n", nodeName, podCIDRs)
					continue
				}
			}
		}

		// Assign a unique /24 from 10.244.X.0/24
		podCIDR := fmt.Sprintf("10.244.%d.0/24", i)

		// Create patch
		patch := map[string]interface{}{
			"spec": map[string]interface{}{
				"ipam": map[string]interface{}{
					"podCIDRs": []string{podCIDR},
				},
			},
		}

		patchBytes, err := json.Marshal(patch)
		if err != nil {
			fmt.Printf("[cilium] warning: could not marshal patch for %s: %v\n", nodeName, err)
			continue
		}

		// Apply patch
		_, err = dynClient.Resource(ciliumNodeGVR).Patch(ctx, nodeName, "application/merge-patch+json", patchBytes, metav1.PatchOptions{})
		if err != nil {
			fmt.Printf("[cilium] warning: could not patch CiliumNode %s: %v\n", nodeName, err)
			continue
		}

		fmt.Printf("[cilium] patched CiliumNode %s with podCIDR %s\n", nodeName, podCIDR)
	}

	fmt.Println("[cilium] CiliumNode patching complete")
	return nil
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
