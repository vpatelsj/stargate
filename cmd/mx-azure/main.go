package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/vpatelsj/stargate/cmd/mx-azure/internal/azure"
	"github.com/vpatelsj/stargate/cmd/mx-azure/internal/cloudinit"
)

const usage = `mx-azure - Azure VM provisioning tool for MX clusters

Usage:
  mx-azure <command> [flags]

Commands:
  provision    Create Azure resources (RG, VNet, NSG, VM, etc.)
  status       Show status of existing resources
  destroy      Delete the resource group and all resources

Examples:
  mx-azure provision --subscription-id=<id> --ssh-public-key-path=~/.ssh/id_rsa.pub
  mx-azure status --subscription-id=<id> --resource-group=mx-azure-rg
  mx-azure destroy --subscription-id=<id> --resource-group=mx-azure-rg --yes

Run 'mx-azure <command> --help' for command-specific flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}

	command := os.Args[1]

	// Handle help specially
	if command == "-h" || command == "--help" || command == "help" {
		fmt.Print(usage)
		os.Exit(0)
	}

	switch command {
	case "provision":
		os.Exit(runProvision(os.Args[2:]))
	case "status":
		os.Exit(runStatus(os.Args[2:]))
	case "destroy":
		os.Exit(runDestroy(os.Args[2:]))
	default:
		fmt.Fprintf(os.Stderr, "Unknown command: %s\n\n", command)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(1)
	}
}

// CommonFlags contains flags shared across all commands
type CommonFlags struct {
	SubscriptionID string
	Location       string
	ResourceGroup  string
	LogJSON        bool
}

// VMFlags contains flags for VM configuration
type VMFlags struct {
	Zone              string
	VNetName          string
	VNetAddressSpace  string
	SubnetName        string
	SubnetPrefix      string
	NSGName           string
	PublicIPName      string
	NICName           string
	VMName            string
	AdminUsername     string
	SSHPublicKeyPath  string
	VMSize            string
	ImagePublisher    string
	ImageOffer        string
	ImageSKU          string
	TailscaleAuthKey  string
	KubernetesVersion string
}

func addCommonFlags(fs *flag.FlagSet, cf *CommonFlags) {
	fs.StringVar(&cf.SubscriptionID, "subscription-id", os.Getenv("AZURE_SUBSCRIPTION_ID"), "Azure Subscription ID (env: AZURE_SUBSCRIPTION_ID)")
	fs.StringVar(&cf.Location, "location", "canadacentral", "Azure region for resources")
	fs.StringVar(&cf.ResourceGroup, "resource-group", "mx-azure-rg", "Name of the resource group")
	fs.BoolVar(&cf.LogJSON, "log-json", false, "Output logs in JSON format")
}

func addVMFlags(fs *flag.FlagSet, vf *VMFlags) {
	fs.StringVar(&vf.Zone, "zone", "1", "Azure availability zone (1, 2, or 3)")
	fs.StringVar(&vf.VNetName, "vnet-name", "mx-vnet", "Name of the virtual network")
	fs.StringVar(&vf.VNetAddressSpace, "vnet-address-space", "10.0.0.0/16", "Address space for the VNet")
	fs.StringVar(&vf.SubnetName, "subnet-name", "mx-subnet", "Name of the subnet")
	fs.StringVar(&vf.SubnetPrefix, "subnet-prefix", "10.0.1.0/24", "Address prefix for the subnet")
	fs.StringVar(&vf.NSGName, "nsg-name", "mx-nsg", "Name of the network security group")
	fs.StringVar(&vf.PublicIPName, "public-ip-name", "mx-pip", "Name of the public IP address")
	fs.StringVar(&vf.NICName, "nic-name", "mx-nic", "Name of the network interface")
	fs.StringVar(&vf.VMName, "vm-name", "mx-vm", "Name of the virtual machine")
	fs.StringVar(&vf.AdminUsername, "admin-username", "azureuser", "Admin username for the VM")
	fs.StringVar(&vf.SSHPublicKeyPath, "ssh-public-key-path", "", "Path to SSH public key file (required)")
	fs.StringVar(&vf.VMSize, "vm-size", "Standard_D2s_v5", "Azure VM size")
	fs.StringVar(&vf.ImagePublisher, "image-publisher", "Canonical", "VM image publisher")
	fs.StringVar(&vf.ImageOffer, "image-offer", "0001-com-ubuntu-server-jammy", "VM image offer")
	fs.StringVar(&vf.ImageSKU, "image-sku", "22_04-lts-gen2", "VM image SKU")
	fs.StringVar(&vf.TailscaleAuthKey, "tailscale-auth-key", os.Getenv("TAILSCALE_AUTH_KEY"), "Tailscale auth key (env: TAILSCALE_AUTH_KEY)")
	fs.StringVar(&vf.KubernetesVersion, "kubernetes-version", "1.29", "Kubernetes version to install")
}

func setupLogger(jsonFormat bool) *slog.Logger {
	if jsonFormat {
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

func createContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	return ctx, cancel
}

func runProvision(args []string) int {
	fs := flag.NewFlagSet("provision", flag.ExitOnError)
	var cf CommonFlags
	var vf VMFlags
	addCommonFlags(fs, &cf)
	addVMFlags(fs, &vf)

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mx-azure provision [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 1
	}

	logger := setupLogger(cf.LogJSON)
	slog.SetDefault(logger)

	// Validate required flags
	if cf.SubscriptionID == "" {
		logger.Error("subscription-id is required (set via flag or AZURE_SUBSCRIPTION_ID env var)")
		return 1
	}
	if vf.SSHPublicKeyPath == "" {
		logger.Error("ssh-public-key-path is required")
		return 1
	}

	// Read SSH public key
	sshKeyData, err := os.ReadFile(vf.SSHPublicKeyPath)
	if err != nil {
		logger.Error("failed to read SSH public key", "path", vf.SSHPublicKeyPath, "error", err)
		return 1
	}

	ctx, cancel := createContext()
	defer cancel()

	// Initialize Azure clients
	logger.Info("initializing Azure clients", "subscriptionID", cf.SubscriptionID)
	clients, err := azure.NewClients(cf.SubscriptionID, logger)
	if err != nil {
		logger.Error("failed to create Azure clients", "error", err)
		return 1
	}

	// Render cloud-init
	logger.Info("rendering cloud-init configuration")
	cloudInitData, err := cloudinit.RenderMXCloudInitBase64(vf.AdminUsername, vf.TailscaleAuthKey, vf.KubernetesVersion)
	if err != nil {
		logger.Error("failed to render cloud-init", "error", err)
		return 1
	}

	// Build VM configuration
	vmConfig := azure.VMConfig{
		Location:         cf.Location,
		Zone:             vf.Zone,
		ResourceGroup:    cf.ResourceGroup,
		VNetName:         vf.VNetName,
		VNetAddressSpace: vf.VNetAddressSpace,
		SubnetName:       vf.SubnetName,
		SubnetPrefix:     vf.SubnetPrefix,
		NSGName:          vf.NSGName,
		PublicIPName:     vf.PublicIPName,
		NICName:          vf.NICName,
		VMName:           vf.VMName,
		VMSize:           vf.VMSize,
		AdminUsername:    vf.AdminUsername,
		SSHPublicKey:     string(sshKeyData),
		ImagePublisher:   vf.ImagePublisher,
		ImageOffer:       vf.ImageOffer,
		ImageSKU:         vf.ImageSKU,
		CustomData:       cloudInitData,
	}

	// Provision all resources
	logger.Info("starting Azure resource provisioning")
	result, err := clients.ProvisionAll(ctx, vmConfig)
	if err != nil {
		logger.Error("provisioning failed", "error", err)
		return 1
	}

	logger.Info("provisioning completed successfully",
		"vmID", result.VMID,
		"publicIP", result.PublicIPAddress,
	)

	// Add SSH config entry
	if err := addSSHConfigEntry(vf.VMName, vf.AdminUsername, logger); err != nil {
		logger.Warn("failed to add SSH config entry", "error", err)
	}

	// Print post-provisioning instructions
	printPostProvisioningInstructions(vf.VMName, cf.ResourceGroup, vf.AdminUsername, result.PublicIPAddress)

	return 0
}

func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	var cf CommonFlags
	var vf VMFlags
	addCommonFlags(fs, &cf)
	addVMFlags(fs, &vf)

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mx-azure status [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 1
	}

	logger := setupLogger(cf.LogJSON)
	slog.SetDefault(logger)

	if cf.SubscriptionID == "" {
		logger.Error("subscription-id is required")
		return 1
	}

	ctx, cancel := createContext()
	defer cancel()

	clients, err := azure.NewClients(cf.SubscriptionID, logger)
	if err != nil {
		logger.Error("failed to create Azure clients", "error", err)
		return 1
	}

	vmConfig := azure.VMConfig{
		ResourceGroup: cf.ResourceGroup,
		NSGName:       vf.NSGName,
		VNetName:      vf.VNetName,
		SubnetName:    vf.SubnetName,
		PublicIPName:  vf.PublicIPName,
		NICName:       vf.NICName,
		VMName:        vf.VMName,
	}

	status, err := clients.GetStatus(ctx, vmConfig)
	if err != nil {
		logger.Error("failed to get status", "error", err)
		return 1
	}

	printStatus(status)
	return 0
}

func runDestroy(args []string) int {
	fs := flag.NewFlagSet("destroy", flag.ExitOnError)
	var cf CommonFlags
	addCommonFlags(fs, &cf)
	yes := fs.Bool("yes", false, "Skip confirmation prompt")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: mx-azure destroy [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return 1
	}

	logger := setupLogger(cf.LogJSON)
	slog.SetDefault(logger)

	if cf.SubscriptionID == "" {
		logger.Error("subscription-id is required")
		return 1
	}

	if !*yes {
		fmt.Printf("⚠️  WARNING: This will permanently delete resource group '%s' and ALL resources inside it.\n", cf.ResourceGroup)
		fmt.Printf("Type the resource group name to confirm: ")
		var confirmation string
		fmt.Scanln(&confirmation)
		if confirmation != cf.ResourceGroup {
			fmt.Println("Confirmation failed. Aborting.")
			return 1
		}
	}

	ctx, cancel := createContext()
	defer cancel()

	clients, err := azure.NewClients(cf.SubscriptionID, logger)
	if err != nil {
		logger.Error("failed to create Azure clients", "error", err)
		return 1
	}

	if err := clients.DeleteResourceGroup(ctx, cf.ResourceGroup); err != nil {
		logger.Error("failed to delete resource group", "error", err)
		return 1
	}

	fmt.Printf("✓ Resource group '%s' deleted successfully.\n", cf.ResourceGroup)
	return 0
}

func printStatus(status *azure.ClusterStatus) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                           MX AZURE RESOURCE STATUS                           ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	printResourceStatus("Resource Group", status.ResourceGroup)
	printResourceStatus("Network Security Group", status.NSG)
	printResourceStatus("Virtual Network", status.VNet)
	printResourceStatus("Subnet", status.Subnet)
	printResourceStatus("Public IP", status.PublicIP)
	printResourceStatus("Network Interface", status.NIC)
	printResourceStatus("Virtual Machine", status.VM)

	fmt.Println()
}

func printResourceStatus(label string, rs azure.ResourceStatus) {
	if rs.Exists {
		fmt.Printf("  ✓ %-24s %s\n", label+":", rs.Name)
		if rs.ID != "" {
			fmt.Printf("    %-24s %s\n", "ID:", truncateID(rs.ID))
		}
		for k, v := range rs.Extra {
			fmt.Printf("    %-24s %s\n", k+":", v)
		}
	} else {
		fmt.Printf("  ✗ %-24s %s (not found)\n", label+":", rs.Name)
	}
}

func truncateID(id string) string {
	// Show last part of the ID (resource name section)
	parts := strings.Split(id, "/")
	if len(parts) > 2 {
		return ".../" + strings.Join(parts[len(parts)-2:], "/")
	}
	return id
}

func printPostProvisioningInstructions(vmName, resourceGroup, adminUser, publicIP string) {
	fmt.Println()
	fmt.Println("╔══════════════════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                    MX AZURE PROVISIONING COMPLETE                            ║")
	fmt.Println("╚══════════════════════════════════════════════════════════════════════════════╝")
	fmt.Println()
	fmt.Printf("  VM Name:        %s\n", vmName)
	fmt.Printf("  Resource Group: %s\n", resourceGroup)
	fmt.Printf("  Public IP:      %s (for diagnostics only - use Tailscale for access)\n", publicIP)
	fmt.Printf("  SSH Config:     ~/.ssh/config entry added (use: tailscale ssh %s)\n", vmName)
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
	fmt.Println("  5. To destroy resources when done:")
	fmt.Printf("     mx-azure destroy --resource-group=%s --yes\n", resourceGroup)
	fmt.Println()
}

func addSSHConfigEntry(vmName, adminUser string, logger *slog.Logger) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get home directory: %w", err)
	}

	sshConfigPath := filepath.Join(home, ".ssh", "config")

	sshDir := filepath.Dir(sshConfigPath)
	if err := os.MkdirAll(sshDir, 0700); err != nil {
		return fmt.Errorf("failed to create .ssh directory: %w", err)
	}

	existingConfig, err := os.ReadFile(sshConfigPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read SSH config: %w", err)
	}

	hostMarker := fmt.Sprintf("Host %s", vmName)
	if strings.Contains(string(existingConfig), hostMarker) {
		logger.Info("SSH config entry already exists", "host", vmName)
		return nil
	}

	entry := fmt.Sprintf(`
# Added by mx-azure for Tailscale SSH
Host %s
  User %s
  HostName %s
`, vmName, adminUser, vmName)

	f, err := os.OpenFile(sshConfigPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open SSH config: %w", err)
	}
	defer f.Close()

	if _, err := f.WriteString(entry); err != nil {
		return fmt.Errorf("failed to write SSH config entry: %w", err)
	}

	logger.Info("added SSH config entry", "host", vmName, "path", sshConfigPath)
	return nil
}
