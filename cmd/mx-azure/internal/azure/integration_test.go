//go:build integration

package azure

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/vpatelsj/stargate/cmd/mx-azure/internal/cloudinit"
)

// Required environment variables for integration tests
const (
	envSubscriptionID   = "AZURE_SUBSCRIPTION_ID"
	envLocation         = "AZURE_LOCATION"
	envRGPrefix         = "MX_RG_PREFIX"
	envSSHPublicKey     = "MX_SSH_PUBLIC_KEY"
	envTailscaleAuthKey = "MX_TAILSCALE_AUTH_KEY"
)

// Integration test timeouts
const (
	provisionTimeout = 10 * time.Minute
	cleanupTimeout   = 5 * time.Minute
	getTimeout       = 30 * time.Second
)

type integrationConfig struct {
	subscriptionID   string
	location         string
	rgPrefix         string
	sshPublicKey     string
	tailscaleAuthKey string
}

func loadIntegrationConfig(t *testing.T) *integrationConfig {
	t.Helper()

	cfg := &integrationConfig{
		subscriptionID:   os.Getenv(envSubscriptionID),
		location:         os.Getenv(envLocation),
		rgPrefix:         os.Getenv(envRGPrefix),
		sshPublicKey:     os.Getenv(envSSHPublicKey),
		tailscaleAuthKey: os.Getenv(envTailscaleAuthKey),
	}

	// Check required env vars
	missing := []string{}
	if cfg.subscriptionID == "" {
		missing = append(missing, envSubscriptionID)
	}
	if cfg.location == "" {
		missing = append(missing, envLocation)
	}
	if cfg.rgPrefix == "" {
		missing = append(missing, envRGPrefix)
	}
	if cfg.sshPublicKey == "" {
		missing = append(missing, envSSHPublicKey)
	}
	if cfg.tailscaleAuthKey == "" {
		missing = append(missing, envTailscaleAuthKey)
	}

	if len(missing) > 0 {
		t.Skipf("Skipping integration test: missing required env vars: %v", missing)
	}

	return cfg
}

func TestIntegration_ProvisionAndVerify(t *testing.T) {
	cfg := loadIntegrationConfig(t)

	// Generate unique resource names
	uniqueID := fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	rgName := fmt.Sprintf("%s-inttest-%s", cfg.rgPrefix, uniqueID)
	vmName := fmt.Sprintf("vm-inttest-%s", uniqueID)
	nsgName := fmt.Sprintf("nsg-inttest-%s", uniqueID)
	vnetName := fmt.Sprintf("vnet-inttest-%s", uniqueID)
	subnetName := "default"
	publicIPName := fmt.Sprintf("pip-inttest-%s", uniqueID)
	nicName := fmt.Sprintf("nic-inttest-%s", uniqueID)

	t.Logf("Integration test resources: RG=%s, VM=%s", rgName, vmName)

	// Create logger
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	// Create ARM clients (uses DefaultAzureCredential internally)
	clients, err := NewClients(cfg.subscriptionID, logger)
	if err != nil {
		t.Fatalf("Failed to create ARM clients: %v", err)
	}

	// ALWAYS clean up the resource group at the end
	defer func() {
		t.Log("Cleaning up: deleting resource group...")
		cleanupCtx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()

		poller, err := clients.ResourceGroups.BeginDelete(cleanupCtx, rgName, nil)
		if err != nil {
			t.Logf("Warning: failed to start RG deletion: %v", err)
			return
		}
		_, err = poller.PollUntilDone(cleanupCtx, nil)
		if err != nil {
			t.Logf("Warning: failed to complete RG deletion: %v", err)
			return
		}
		t.Log("Cleanup complete: resource group deleted")
	}()

	// Create context with timeout for provisioning
	ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
	defer cancel()

	// Step 1: Create Resource Group
	t.Log("Creating resource group...")
	_, err = clients.EnsureResourceGroup(ctx, rgName, cfg.location)
	if err != nil {
		t.Fatalf("Failed to create resource group: %v", err)
	}

	// Step 2: Create NSG
	t.Log("Creating NSG...")
	nsgID, err := clients.EnsureNSG(ctx, rgName, nsgName, cfg.location)
	if err != nil {
		t.Fatalf("Failed to create NSG: %v", err)
	}

	// Step 3: Create VNet and Subnet
	t.Log("Creating VNet and Subnet...")
	vnetResult, err := clients.EnsureVNetAndSubnet(ctx, rgName, vnetName, cfg.location,
		"10.0.0.0/16", subnetName, "10.0.0.0/24", nsgID)
	if err != nil {
		t.Fatalf("Failed to create VNet/Subnet: %v", err)
	}

	// Step 4: Create Public IP
	t.Log("Creating Public IP...")
	publicIPResult, err := clients.EnsurePublicIP(ctx, rgName, publicIPName, cfg.location)
	if err != nil {
		t.Fatalf("Failed to create Public IP: %v", err)
	}

	// Step 5: Create NIC
	t.Log("Creating NIC...")
	nicResult, err := clients.EnsureNIC(ctx, rgName, nicName, cfg.location, vnetResult.SubnetID, publicIPResult.ID, nsgID)
	if err != nil {
		t.Fatalf("Failed to create NIC: %v", err)
	}

	// Step 6: Generate cloud-init (minimal - no tailscale auth key logged)
	t.Log("Generating cloud-init...")
	customData, err := cloudinit.RenderMXCloudInitBase64("azureuser", cfg.tailscaleAuthKey, "1.29")
	if err != nil {
		t.Fatalf("Failed to generate cloud-init: %v", err)
	}

	// Step 7: Create VM
	t.Log("Creating VM...")
	vmCfg := VMConfig{
		Location:       cfg.location,
		Zone:           "1",
		ResourceGroup:  rgName,
		VMName:         vmName,
		VMSize:         "Standard_D2s_v5",
		AdminUsername:  "azureuser",
		SSHPublicKey:   cfg.sshPublicKey,
		ImagePublisher: "Canonical",
		ImageOffer:     "0001-com-ubuntu-server-jammy",
		ImageSKU:       "22_04-lts-gen2",
		CustomData:     customData,
	}
	_, err = clients.EnsureVM(ctx, vmCfg, nicResult.ID, publicIPResult.IPAddress)
	if err != nil {
		t.Fatalf("Failed to create VM: %v", err)
	}

	t.Log("Provisioning complete. Verifying resources...")

	// Verification phase - use shorter timeout
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), getTimeout)
	defer verifyCancel()

	// Verify Resource Group exists
	t.Log("Verifying resource group...")
	rgResp, err := clients.ResourceGroups.Get(verifyCtx, rgName, nil)
	if err != nil {
		t.Errorf("Failed to verify resource group: %v", err)
	} else if *rgResp.Properties.ProvisioningState != "Succeeded" {
		t.Errorf("Resource group not in Succeeded state: %s", *rgResp.Properties.ProvisioningState)
	}

	// Verify NSG exists
	t.Log("Verifying NSG...")
	nsgResp, err := clients.SecurityGroups.Get(verifyCtx, rgName, nsgName, nil)
	if err != nil {
		t.Errorf("Failed to verify NSG: %v", err)
	} else if *nsgResp.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded {
		t.Errorf("NSG not in Succeeded state: %s", *nsgResp.Properties.ProvisioningState)
	}

	// Verify Public IP exists
	t.Log("Verifying Public IP...")
	pipResp, err := clients.PublicIPs.Get(verifyCtx, rgName, publicIPName, nil)
	if err != nil {
		t.Errorf("Failed to verify Public IP: %v", err)
	} else if *pipResp.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded {
		t.Errorf("Public IP not in Succeeded state: %s", *pipResp.Properties.ProvisioningState)
	}

	// Verify NIC exists
	t.Log("Verifying NIC...")
	nicResp, err := clients.Interfaces.Get(verifyCtx, rgName, nicName, nil)
	if err != nil {
		t.Errorf("Failed to verify NIC: %v", err)
	} else if *nicResp.Properties.ProvisioningState != armnetwork.ProvisioningStateSucceeded {
		t.Errorf("NIC not in Succeeded state: %s", *nicResp.Properties.ProvisioningState)
	}

	// Verify VM exists and is running
	t.Log("Verifying VM...")
	vmResp, err := clients.VirtualMachines.Get(verifyCtx, rgName, vmName, &armcompute.VirtualMachinesClientGetOptions{
		Expand: to.Ptr(armcompute.InstanceViewTypesInstanceView),
	})
	if err != nil {
		t.Errorf("Failed to verify VM: %v", err)
	} else {
		if *vmResp.Properties.ProvisioningState != "Succeeded" {
			t.Errorf("VM not in Succeeded state: %s", *vmResp.Properties.ProvisioningState)
		}
		// Check VM is running
		if vmResp.Properties.InstanceView != nil && vmResp.Properties.InstanceView.Statuses != nil {
			running := false
			for _, status := range vmResp.Properties.InstanceView.Statuses {
				if status.Code != nil && *status.Code == "PowerState/running" {
					running = true
					break
				}
			}
			if !running {
				t.Log("Warning: VM not in running state (may still be starting)")
			}
		}
	}

	t.Log("All verifications complete!")
}

// TestIntegration_ClientCreation verifies clients can be created with valid credentials
func TestIntegration_ClientCreation(t *testing.T) {
	subscriptionID := os.Getenv(envSubscriptionID)
	if subscriptionID == "" {
		t.Skipf("Skipping: %s not set", envSubscriptionID)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clients, err := NewClients(subscriptionID, logger)
	if err != nil {
		t.Fatalf("Failed to create clients: %v", err)
	}

	// Verify clients are not nil
	if clients.ResourceGroups == nil {
		t.Error("ResourceGroups client is nil")
	}
	if clients.VirtualNetworks == nil {
		t.Error("VirtualNetworks client is nil")
	}
	if clients.Subnets == nil {
		t.Error("Subnets client is nil")
	}
	if clients.SecurityGroups == nil {
		t.Error("SecurityGroups client is nil")
	}
	if clients.PublicIPs == nil {
		t.Error("PublicIPs client is nil")
	}
	if clients.Interfaces == nil {
		t.Error("Interfaces client is nil")
	}
	if clients.VirtualMachines == nil {
		t.Error("VirtualMachines client is nil")
	}
}

// TestIntegration_ListResourceGroups verifies we can list resource groups (quick sanity check)
func TestIntegration_ListResourceGroups(t *testing.T) {
	subscriptionID := os.Getenv(envSubscriptionID)
	if subscriptionID == "" {
		t.Skipf("Skipping: %s not set", envSubscriptionID)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clients, err := NewClients(subscriptionID, logger)
	if err != nil {
		t.Fatalf("Failed to create clients: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()

	pager := clients.ResourceGroups.NewListPager(nil)
	count := 0
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			t.Fatalf("Failed to list resource groups: %v", err)
		}
		count += len(page.Value)
		if count > 0 {
			break // Just verify we can list, don't need all
		}
	}

	t.Logf("Successfully listed resource groups (found at least %d)", count)
}

// TestIntegration_DeleteResourceGroup tests the destroy functionality
// This test creates a minimal resource group and then deletes it.
func TestIntegration_DeleteResourceGroup(t *testing.T) {
	subscriptionID := os.Getenv(envSubscriptionID)
	location := os.Getenv(envLocation)

	if subscriptionID == "" {
		t.Skipf("Skipping: %s not set", envSubscriptionID)
	}
	if location == "" {
		location = "canadacentral"
	}

	// Generate unique RG name
	uniqueID := fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	rgName := fmt.Sprintf("mx-delete-test-%s", uniqueID)

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clients, err := NewClients(subscriptionID, logger)
	if err != nil {
		t.Fatalf("Failed to create clients: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Step 1: Create a resource group
	t.Log("Creating resource group for delete test...")
	_, err = clients.EnsureResourceGroup(ctx, rgName, location)
	if err != nil {
		t.Fatalf("Failed to create resource group: %v", err)
	}

	// Verify it exists
	_, err = clients.ResourceGroups.Get(ctx, rgName, nil)
	if err != nil {
		t.Fatalf("Resource group should exist after creation: %v", err)
	}
	t.Logf("Resource group %s created successfully", rgName)

	// Step 2: Delete the resource group using our function
	t.Log("Deleting resource group...")
	err = clients.DeleteResourceGroup(ctx, rgName)
	if err != nil {
		t.Fatalf("Failed to delete resource group: %v", err)
	}

	// Step 3: Verify it's deleted
	t.Log("Verifying resource group is deleted...")
	_, err = clients.ResourceGroups.Get(ctx, rgName, nil)
	if err == nil {
		t.Fatal("Resource group should not exist after deletion")
	}
	if !isNotFound(err) {
		t.Fatalf("Expected not found error, got: %v", err)
	}

	t.Log("Delete test passed: resource group successfully deleted")
}

// TestIntegration_GetStatus tests the status functionality
func TestIntegration_GetStatus(t *testing.T) {
	subscriptionID := os.Getenv(envSubscriptionID)
	if subscriptionID == "" {
		t.Skipf("Skipping: %s not set", envSubscriptionID)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clients, err := NewClients(subscriptionID, logger)
	if err != nil {
		t.Fatalf("Failed to create clients: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()

	// Test with non-existent resource group
	cfg := VMConfig{
		ResourceGroup: "nonexistent-rg-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000),
		VMName:        "test-vm",
		NSGName:       "test-nsg",
		VNetName:      "test-vnet",
		SubnetName:    "test-subnet",
		PublicIPName:  "test-pip",
		NICName:       "test-nic",
	}

	status, err := clients.GetStatus(ctx, cfg)
	if err != nil {
		t.Fatalf("GetStatus should not error for non-existent resources: %v", err)
	}

	// Verify all resources show as not existing
	if status.ResourceGroup.Exists {
		t.Error("Resource group should not exist")
	}
	if status.VM.Exists {
		t.Error("VM should not exist")
	}
	if status.NSG.Exists {
		t.Error("NSG should not exist")
	}

	t.Log("GetStatus correctly reports non-existent resources")
}

// TestIntegration_DeleteNonExistentResourceGroup verifies delete is idempotent
func TestIntegration_DeleteNonExistentResourceGroup(t *testing.T) {
	subscriptionID := os.Getenv(envSubscriptionID)
	if subscriptionID == "" {
		t.Skipf("Skipping: %s not set", envSubscriptionID)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clients, err := NewClients(subscriptionID, logger)
	if err != nil {
		t.Fatalf("Failed to create clients: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), getTimeout)
	defer cancel()

	// Try to delete a non-existent resource group
	rgName := "nonexistent-rg-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000)
	err = clients.DeleteResourceGroup(ctx, rgName)
	if err != nil {
		t.Fatalf("Delete should be idempotent for non-existent RG, got error: %v", err)
	}

	t.Log("Delete is idempotent for non-existent resource groups")
}
