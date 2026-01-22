package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vpatelsj/stargate/cmd/mx-azure/internal/azure"
	"github.com/vpatelsj/stargate/cmd/mx-azure/internal/cloudinit"
)

func main() {
	// Define CLI flags
	subscriptionID := flag.String("subscription-id", os.Getenv("AZURE_SUBSCRIPTION_ID"), "Azure Subscription ID (env: AZURE_SUBSCRIPTION_ID)")
	location := flag.String("location", "canadacentral", "Azure region for resources")
	zone := flag.String("zone", "1", "Azure availability zone (1, 2, or 3)")
	resourceGroup := flag.String("resource-group", "mx-azure-rg", "Name of the resource group")
	vnetName := flag.String("vnet-name", "mx-vnet", "Name of the virtual network")
	vnetAddressSpace := flag.String("vnet-address-space", "10.0.0.0/16", "Address space for the VNet")
	subnetName := flag.String("subnet-name", "mx-subnet", "Name of the subnet")
	subnetPrefix := flag.String("subnet-prefix", "10.0.1.0/24", "Address prefix for the subnet")
	nsgName := flag.String("nsg-name", "mx-nsg", "Name of the network security group")
	publicIPName := flag.String("public-ip-name", "mx-pip", "Name of the public IP address")
	nicName := flag.String("nic-name", "mx-nic", "Name of the network interface")
	vmName := flag.String("vm-name", "mx-vm", "Name of the virtual machine")
	adminUsername := flag.String("admin-username", "azureuser", "Admin username for the VM")
	sshPublicKeyPath := flag.String("ssh-public-key-path", "", "Path to SSH public key file (required)")
	vmSize := flag.String("vm-size", "Standard_D2s_v5", "Azure VM size (try Standard_B2s if quota issues)")
	imagePublisher := flag.String("image-publisher", "Canonical", "VM image publisher")
	imageOffer := flag.String("image-offer", "0001-com-ubuntu-server-jammy", "VM image offer")
	imageSKU := flag.String("image-sku", "22_04-lts-gen2", "VM image SKU")
	tailscaleAuthKey := flag.String("tailscale-auth-key", os.Getenv("TAILSCALE_AUTH_KEY"), "Tailscale auth key for automatic enrollment (env: TAILSCALE_AUTH_KEY)")
	kubernetesVersion := flag.String("kubernetes-version", "1.29", "Kubernetes version to install (e.g., 1.29)")
	logJSON := flag.Bool("log-json", false, "Output logs in JSON format")

	flag.Parse()

	// Setup structured logger
	var logger *slog.Logger
	if *logJSON {
		logger = slog.New(slog.NewJSONHandler(os.Stderr, nil))
	} else {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	slog.SetDefault(logger)

	// SECURITY: Never log the tailscale auth key - it's a secret
	// The key is passed to cloud-init but never appears in our logs

	// Validate required flags
	if *subscriptionID == "" {
		logger.Error("subscription-id is required (set via flag or AZURE_SUBSCRIPTION_ID env var)")
		os.Exit(1)
	}

	if *sshPublicKeyPath == "" {
		logger.Error("ssh-public-key-path is required")
		os.Exit(1)
	}

	// Read SSH public key
	sshKeyData, err := os.ReadFile(*sshPublicKeyPath)
	if err != nil {
		logger.Error("failed to read SSH public key", "path", *sshPublicKeyPath, "error", err)
		os.Exit(1)
	}

	// Create context with cancellation
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle SIGINT/SIGTERM gracefully
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Initialize Azure clients
	logger.Info("initializing Azure clients", "subscriptionID", *subscriptionID)
	clients, err := azure.NewClients(*subscriptionID, logger)
	if err != nil {
		logger.Error("failed to create Azure clients", "error", err)
		os.Exit(1)
	}

	// Render cloud-init
	// SECURITY: tailscaleAuthKey is embedded in cloud-init but never logged
	// Cloud-init custom data is encrypted at rest by Azure
	logger.Info("rendering cloud-init configuration")
	cloudInitData, err := cloudinit.RenderMXCloudInitBase64(*adminUsername, *tailscaleAuthKey, *kubernetesVersion)
	if err != nil {
		logger.Error("failed to render cloud-init", "error", err)
		os.Exit(1)
	}

	// Build VM configuration
	vmConfig := azure.VMConfig{
		Location:         *location,
		Zone:             *zone,
		ResourceGroup:    *resourceGroup,
		VNetName:         *vnetName,
		VNetAddressSpace: *vnetAddressSpace,
		SubnetName:       *subnetName,
		SubnetPrefix:     *subnetPrefix,
		NSGName:          *nsgName,
		PublicIPName:     *publicIPName,
		NICName:          *nicName,
		VMName:           *vmName,
		VMSize:           *vmSize,
		AdminUsername:    *adminUsername,
		SSHPublicKey:     string(sshKeyData),
		ImagePublisher:   *imagePublisher,
		ImageOffer:       *imageOffer,
		ImageSKU:         *imageSKU,
		CustomData:       cloudInitData,
	}

	// Provision all resources
	logger.Info("starting Azure resource provisioning")
	result, err := clients.ProvisionAll(ctx, vmConfig)
	if err != nil {
		logger.Error("provisioning failed", "error", err)
		os.Exit(1)
	}

	logger.Info("provisioning completed successfully",
		"vmID", result.VMID,
		"publicIP", result.PublicIPAddress,
	)

	// Print post-provisioning instructions
	printPostProvisioningInstructions(*vmName, *resourceGroup, result.PublicIPAddress)
}

func printPostProvisioningInstructions(vmName, resourceGroup, publicIP string) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    MX AZURE PROVISIONING COMPLETE                            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  VM Name:        %s\n", vmName)
	fmt.Printf("  Resource Group: %s\n", resourceGroup)
	fmt.Printf("  Public IP:      %s (for diagnostics only - use Tailscale for access)\n", publicIP)
	fmt.Println()
	fmt.Println("┌──────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│ NEXT STEPS                                                                   │")
	fmt.Println("└──────────────────────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  1. Wait for the VM to appear in Tailscale Admin Console:")
	fmt.Println("     https://login.tailscale.com/admin/machines")
	fmt.Println()
	fmt.Println("  2. Verify Kubernetes is ready (from your laptop):")
	fmt.Printf("     tailscale ssh %s -- sudo kubectl get nodes\n", vmName)
	fmt.Println()
	fmt.Println("  3. Copy kubeconfig to your local machine (optional):")
	fmt.Printf("     tailscale ssh %s -- sudo cat /etc/kubernetes/admin.conf > ~/.kube/%s.conf\n", vmName, vmName)
	fmt.Printf("     export KUBECONFIG=~/.kube/%s.conf\n", vmName)
	fmt.Println()
	fmt.Println("  4. Check bootstrap logs if needed:")
	fmt.Printf("     tailscale ssh %s -- sudo tail -f /var/log/mx-bootstrap.log\n", vmName)
	fmt.Println()
	fmt.Println("┌──────────────────────────────────────────────────────────────────────────────┐")
	fmt.Println("│ ⚠️  IMPORTANT: TAILSCALE ACL CONFIGURATION                                   │")
	fmt.Println("└──────────────────────────────────────────────────────────────────────────────┘")
	fmt.Println()
	fmt.Println("  Ensure your Tailscale ACL policy allows:")
	fmt.Println("    • SSH access to this node (port 22 or Tailscale SSH)")
	fmt.Println("    • Kubernetes API access (port 6443) from your devices")
	fmt.Println("    • Pod-to-pod communication if using Tailscale for CNI")
	fmt.Println()
	fmt.Println("  Example ACL snippet:")
	fmt.Println("    {")
	fmt.Println("      \"acls\": [")
	fmt.Println("        {\"action\": \"accept\", \"src\": [\"tag:admin\"], \"dst\": [\"tag:k8s:*\"]}")
	fmt.Println("      ],")
	fmt.Println("      \"ssh\": [")
	fmt.Println("        {\"action\": \"accept\", \"src\": [\"tag:admin\"], \"dst\": [\"tag:k8s\"], \"users\": [\"autogroup:nonroot\", \"root\"]}")
	fmt.Println("      ]")
	fmt.Println("    }")
	fmt.Println()
}
