package azure

import (
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
)

func TestTags(t *testing.T) {
	tags := Tags()

	if tags == nil {
		t.Fatal("Tags() should not return nil")
	}

	managedBy, ok := tags["managedBy"]
	if !ok {
		t.Error("Tags should contain 'managedBy' key")
	}
	if managedBy == nil || *managedBy != "mx-azure" {
		t.Errorf("managedBy tag should be 'mx-azure', got %v", managedBy)
	}
}

func TestBuildResourceGroup(t *testing.T) {
	rg := BuildResourceGroup("eastus")

	if rg.Location == nil || *rg.Location != "eastus" {
		t.Errorf("Location should be 'eastus', got %v", rg.Location)
	}

	if rg.Tags == nil {
		t.Error("Tags should not be nil")
	}
	if rg.Tags["managedBy"] == nil || *rg.Tags["managedBy"] != "mx-azure" {
		t.Error("Tags should include managedBy=mx-azure")
	}
}

func TestBuildNSG(t *testing.T) {
	t.Run("no_inbound_allow_rules", func(t *testing.T) {
		nsg := BuildNSG("test-nsg", "canadacentral")

		if nsg.Name == nil || *nsg.Name != "test-nsg" {
			t.Errorf("Name should be 'test-nsg', got %v", nsg.Name)
		}
		if nsg.Location == nil || *nsg.Location != "canadacentral" {
			t.Errorf("Location should be 'canadacentral', got %v", nsg.Location)
		}
		if nsg.Properties == nil {
			t.Fatal("Properties should not be nil")
		}

		// CRITICAL: NSG must have no inbound allow rules
		if nsg.Properties.SecurityRules == nil {
			t.Error("SecurityRules should be an empty slice, not nil")
		}
		if len(nsg.Properties.SecurityRules) != 0 {
			t.Errorf("NSG should have zero security rules (deny-all by default), got %d rules", len(nsg.Properties.SecurityRules))
		}

		// Verify no inbound allow rules exist
		for _, rule := range nsg.Properties.SecurityRules {
			if rule.Properties != nil &&
				rule.Properties.Direction != nil &&
				*rule.Properties.Direction == armnetwork.SecurityRuleDirectionInbound &&
				rule.Properties.Access != nil &&
				*rule.Properties.Access == armnetwork.SecurityRuleAccessAllow {
				t.Errorf("NSG should NOT have inbound allow rules, found: %v", rule.Name)
			}
		}
	})

	t.Run("has_tags", func(t *testing.T) {
		nsg := BuildNSG("test-nsg", "eastus")

		if nsg.Tags == nil {
			t.Fatal("Tags should not be nil")
		}
		if nsg.Tags["managedBy"] == nil || *nsg.Tags["managedBy"] != "mx-azure" {
			t.Error("Tags should include managedBy=mx-azure")
		}
	})
}

func TestBuildVNet(t *testing.T) {
	vnet := BuildVNet("my-vnet", "westus", "10.0.0.0/16")

	if vnet.Name == nil || *vnet.Name != "my-vnet" {
		t.Errorf("Name should be 'my-vnet', got %v", vnet.Name)
	}
	if vnet.Location == nil || *vnet.Location != "westus" {
		t.Errorf("Location should be 'westus', got %v", vnet.Location)
	}
	if vnet.Properties == nil {
		t.Fatal("Properties should not be nil")
	}
	if vnet.Properties.AddressSpace == nil {
		t.Fatal("AddressSpace should not be nil")
	}
	if len(vnet.Properties.AddressSpace.AddressPrefixes) != 1 {
		t.Errorf("AddressPrefixes should have 1 entry, got %d", len(vnet.Properties.AddressSpace.AddressPrefixes))
	}
	if *vnet.Properties.AddressSpace.AddressPrefixes[0] != "10.0.0.0/16" {
		t.Errorf("Address prefix should be '10.0.0.0/16', got %v", *vnet.Properties.AddressSpace.AddressPrefixes[0])
	}
}

func TestBuildSubnet(t *testing.T) {
	t.Run("with_nsg", func(t *testing.T) {
		nsgID := "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/nsg"
		subnet := BuildSubnet("my-subnet", "10.0.1.0/24", nsgID)

		if subnet.Name == nil || *subnet.Name != "my-subnet" {
			t.Errorf("Name should be 'my-subnet', got %v", subnet.Name)
		}
		if subnet.Properties == nil {
			t.Fatal("Properties should not be nil")
		}
		if subnet.Properties.AddressPrefix == nil || *subnet.Properties.AddressPrefix != "10.0.1.0/24" {
			t.Errorf("AddressPrefix should be '10.0.1.0/24', got %v", subnet.Properties.AddressPrefix)
		}
		if subnet.Properties.NetworkSecurityGroup == nil {
			t.Fatal("NetworkSecurityGroup should not be nil when nsgID is provided")
		}
		if subnet.Properties.NetworkSecurityGroup.ID == nil || *subnet.Properties.NetworkSecurityGroup.ID != nsgID {
			t.Errorf("NSG ID should be '%s', got %v", nsgID, subnet.Properties.NetworkSecurityGroup.ID)
		}
	})

	t.Run("without_nsg", func(t *testing.T) {
		subnet := BuildSubnet("my-subnet", "10.0.2.0/24", "")

		if subnet.Properties.NetworkSecurityGroup != nil {
			t.Error("NetworkSecurityGroup should be nil when nsgID is empty")
		}
	})
}

func TestBuildPublicIP(t *testing.T) {
	pip := BuildPublicIP("my-pip", "northeurope")

	if pip.Name == nil || *pip.Name != "my-pip" {
		t.Errorf("Name should be 'my-pip', got %v", pip.Name)
	}
	if pip.Location == nil || *pip.Location != "northeurope" {
		t.Errorf("Location should be 'northeurope', got %v", pip.Location)
	}
	if pip.SKU == nil {
		t.Fatal("SKU should not be nil")
	}
	if pip.SKU.Name == nil || *pip.SKU.Name != armnetwork.PublicIPAddressSKUNameStandard {
		t.Errorf("SKU Name should be Standard, got %v", pip.SKU.Name)
	}
	if pip.Properties == nil {
		t.Fatal("Properties should not be nil")
	}
	if pip.Properties.PublicIPAllocationMethod == nil || *pip.Properties.PublicIPAllocationMethod != armnetwork.IPAllocationMethodStatic {
		t.Errorf("Allocation method should be Static, got %v", pip.Properties.PublicIPAllocationMethod)
	}
	if pip.Tags == nil || pip.Tags["managedBy"] == nil || *pip.Tags["managedBy"] != "mx-azure" {
		t.Error("Tags should include managedBy=mx-azure")
	}
}

func TestBuildNIC(t *testing.T) {
	subnetID := "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/subnet"
	publicIPID := "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.Network/publicIPAddresses/pip"
	nsgID := "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.Network/networkSecurityGroups/nsg"

	t.Run("with_all_references", func(t *testing.T) {
		params := NICParams{
			Name:       "my-nic",
			Location:   "eastus2",
			SubnetID:   subnetID,
			PublicIPID: publicIPID,
			NSGID:      nsgID,
		}
		nic := BuildNIC(params)

		if nic.Name == nil || *nic.Name != "my-nic" {
			t.Errorf("Name should be 'my-nic', got %v", nic.Name)
		}
		if nic.Location == nil || *nic.Location != "eastus2" {
			t.Errorf("Location should be 'eastus2', got %v", nic.Location)
		}
		if nic.Properties == nil {
			t.Fatal("Properties should not be nil")
		}

		// Check subnet reference
		if len(nic.Properties.IPConfigurations) != 1 {
			t.Fatalf("Should have 1 IP configuration, got %d", len(nic.Properties.IPConfigurations))
		}
		ipConfig := nic.Properties.IPConfigurations[0]
		if ipConfig.Properties.Subnet == nil || ipConfig.Properties.Subnet.ID == nil {
			t.Fatal("Subnet reference should be set")
		}
		if *ipConfig.Properties.Subnet.ID != subnetID {
			t.Errorf("Subnet ID should be '%s', got '%s'", subnetID, *ipConfig.Properties.Subnet.ID)
		}

		// Check public IP reference
		if ipConfig.Properties.PublicIPAddress == nil || ipConfig.Properties.PublicIPAddress.ID == nil {
			t.Fatal("Public IP reference should be set")
		}
		if *ipConfig.Properties.PublicIPAddress.ID != publicIPID {
			t.Errorf("Public IP ID should be '%s', got '%s'", publicIPID, *ipConfig.Properties.PublicIPAddress.ID)
		}

		// Check NSG reference
		if nic.Properties.NetworkSecurityGroup == nil || nic.Properties.NetworkSecurityGroup.ID == nil {
			t.Fatal("NSG reference should be set")
		}
		if *nic.Properties.NetworkSecurityGroup.ID != nsgID {
			t.Errorf("NSG ID should be '%s', got '%s'", nsgID, *nic.Properties.NetworkSecurityGroup.ID)
		}

		// Check tags
		if nic.Tags == nil || nic.Tags["managedBy"] == nil || *nic.Tags["managedBy"] != "mx-azure" {
			t.Error("Tags should include managedBy=mx-azure")
		}
	})

	t.Run("without_optional_fields", func(t *testing.T) {
		params := NICParams{
			Name:     "minimal-nic",
			Location: "westus",
			SubnetID: subnetID,
			// PublicIPID and NSGID are empty
		}
		nic := BuildNIC(params)

		ipConfig := nic.Properties.IPConfigurations[0]
		if ipConfig.Properties.PublicIPAddress != nil {
			t.Error("PublicIPAddress should be nil when not provided")
		}
		if nic.Properties.NetworkSecurityGroup != nil {
			t.Error("NetworkSecurityGroup should be nil when not provided")
		}
	})
}

func TestBuildVM(t *testing.T) {
	nicID := "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.Network/networkInterfaces/nic"

	cfg := VMConfig{
		Location:       "canadacentral",
		Zone:           "1",
		ResourceGroup:  "my-rg",
		VMName:         "my-vm",
		VMSize:         "Standard_D2s_v5",
		AdminUsername:  "azureuser",
		SSHPublicKey:   "ssh-rsa AAAAB3... user@host",
		ImagePublisher: "Canonical",
		ImageOffer:     "0001-com-ubuntu-server-jammy",
		ImageSKU:       "22_04-lts-gen2",
		CustomData:     "I2Nsb3VkLWNvbmZpZw==", // base64 "#cloud-config"
	}

	t.Run("required_fields", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		if vm.Name == nil || *vm.Name != "my-vm" {
			t.Errorf("Name should be 'my-vm', got %v", vm.Name)
		}
		if vm.Location == nil || *vm.Location != "canadacentral" {
			t.Errorf("Location should be 'canadacentral', got %v", vm.Location)
		}
		if vm.Zones == nil || len(vm.Zones) != 1 || *vm.Zones[0] != "1" {
			t.Errorf("Zones should be ['1'], got %v", vm.Zones)
		}
	})

	t.Run("custom_data_set", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		if vm.Properties == nil || vm.Properties.OSProfile == nil {
			t.Fatal("OSProfile should not be nil")
		}
		if vm.Properties.OSProfile.CustomData == nil {
			t.Fatal("CustomData should be set when provided in config")
		}
		if *vm.Properties.OSProfile.CustomData != cfg.CustomData {
			t.Errorf("CustomData should be '%s', got '%s'", cfg.CustomData, *vm.Properties.OSProfile.CustomData)
		}
	})

	t.Run("custom_data_empty", func(t *testing.T) {
		cfgNoCustomData := cfg
		cfgNoCustomData.CustomData = ""
		vm := BuildVM(cfgNoCustomData, nicID)

		if vm.Properties.OSProfile.CustomData != nil {
			t.Error("CustomData should be nil when not provided")
		}
	})

	t.Run("nic_reference", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		if vm.Properties.NetworkProfile == nil {
			t.Fatal("NetworkProfile should not be nil")
		}
		if len(vm.Properties.NetworkProfile.NetworkInterfaces) != 1 {
			t.Fatalf("Should have 1 NIC, got %d", len(vm.Properties.NetworkProfile.NetworkInterfaces))
		}
		nicRef := vm.Properties.NetworkProfile.NetworkInterfaces[0]
		if nicRef.ID == nil || *nicRef.ID != nicID {
			t.Errorf("NIC ID should be '%s', got '%v'", nicID, nicRef.ID)
		}
		if nicRef.Properties == nil || nicRef.Properties.Primary == nil || !*nicRef.Properties.Primary {
			t.Error("NIC should be marked as primary")
		}
	})

	t.Run("ssh_key_path", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		if vm.Properties.OSProfile.LinuxConfiguration == nil {
			t.Fatal("LinuxConfiguration should not be nil")
		}
		if vm.Properties.OSProfile.LinuxConfiguration.SSH == nil {
			t.Fatal("SSH config should not be nil")
		}
		if len(vm.Properties.OSProfile.LinuxConfiguration.SSH.PublicKeys) != 1 {
			t.Fatal("Should have 1 SSH public key")
		}
		key := vm.Properties.OSProfile.LinuxConfiguration.SSH.PublicKeys[0]
		expectedPath := "/home/azureuser/.ssh/authorized_keys"
		if key.Path == nil || *key.Path != expectedPath {
			t.Errorf("SSH key path should be '%s', got '%v'", expectedPath, key.Path)
		}
		if key.KeyData == nil || *key.KeyData != cfg.SSHPublicKey {
			t.Errorf("SSH key data should be set correctly")
		}
	})

	t.Run("password_auth_disabled", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		if vm.Properties.OSProfile.LinuxConfiguration.DisablePasswordAuthentication == nil {
			t.Fatal("DisablePasswordAuthentication should not be nil")
		}
		if !*vm.Properties.OSProfile.LinuxConfiguration.DisablePasswordAuthentication {
			t.Error("Password authentication should be disabled")
		}
	})

	t.Run("deterministic_disk_name", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		expectedDiskName := cfg.VMName + "-osdisk"
		if vm.Properties.StorageProfile.OSDisk.Name == nil || *vm.Properties.StorageProfile.OSDisk.Name != expectedDiskName {
			t.Errorf("OS disk name should be '%s', got '%v'", expectedDiskName, vm.Properties.StorageProfile.OSDisk.Name)
		}
	})

	t.Run("tags_applied", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		if vm.Tags == nil {
			t.Fatal("Tags should not be nil")
		}
		if vm.Tags["managedBy"] == nil || *vm.Tags["managedBy"] != "mx-azure" {
			t.Error("Tags should include managedBy=mx-azure")
		}
	})

	t.Run("no_zone_when_empty", func(t *testing.T) {
		cfgNoZone := cfg
		cfgNoZone.Zone = ""
		vm := BuildVM(cfgNoZone, nicID)

		if vm.Zones != nil {
			t.Error("Zones should be nil when zone is empty (non-zonal deployment)")
		}
	})

	t.Run("image_reference", func(t *testing.T) {
		vm := BuildVM(cfg, nicID)

		img := vm.Properties.StorageProfile.ImageReference
		if img == nil {
			t.Fatal("ImageReference should not be nil")
		}
		if *img.Publisher != "Canonical" {
			t.Errorf("Publisher should be 'Canonical', got '%s'", *img.Publisher)
		}
		if *img.Offer != "0001-com-ubuntu-server-jammy" {
			t.Errorf("Offer should be '0001-com-ubuntu-server-jammy', got '%s'", *img.Offer)
		}
		if *img.SKU != "22_04-lts-gen2" {
			t.Errorf("SKU should be '22_04-lts-gen2', got '%s'", *img.SKU)
		}
		if *img.Version != "latest" {
			t.Errorf("Version should be 'latest', got '%s'", *img.Version)
		}
	})
}

// TestBuildNSG_SecurityAudit is a dedicated security test ensuring NSG remains deny-all
func TestBuildNSG_SecurityAudit(t *testing.T) {
	// Test multiple invocations to ensure consistency
	locations := []string{"eastus", "westus", "canadacentral", "northeurope"}

	for _, loc := range locations {
		t.Run(loc, func(t *testing.T) {
			nsg := BuildNSG("security-test-nsg", loc)

			// SECURITY ASSERTION: Must have zero inbound allow rules
			if nsg.Properties.SecurityRules == nil {
				t.Error("SecurityRules should be empty slice, not nil")
				return
			}

			for _, rule := range nsg.Properties.SecurityRules {
				if rule.Properties == nil {
					continue
				}
				// Check for any inbound allow rule
				isInbound := rule.Properties.Direction != nil && *rule.Properties.Direction == armnetwork.SecurityRuleDirectionInbound
				isAllow := rule.Properties.Access != nil && *rule.Properties.Access == armnetwork.SecurityRuleAccessAllow

				if isInbound && isAllow {
					name := "unknown"
					if rule.Name != nil {
						name = *rule.Name
					}
					t.Errorf("SECURITY VIOLATION: NSG has inbound allow rule '%s' - this breaks deny-all security model", name)
				}
			}
		})
	}
}

// TestBuildersConsistentTags ensures all builders apply consistent tags
func TestBuildersConsistentTags(t *testing.T) {
	expectedTag := "mx-azure"

	checks := []struct {
		name string
		tags map[string]*string
	}{
		{"ResourceGroup", BuildResourceGroup("eastus").Tags},
		{"NSG", BuildNSG("nsg", "eastus").Tags},
		{"VNet", BuildVNet("vnet", "eastus", "10.0.0.0/16").Tags},
		{"PublicIP", BuildPublicIP("pip", "eastus").Tags},
		{"NIC", BuildNIC(NICParams{Name: "nic", Location: "eastus", SubnetID: "/sub"}).Tags},
		{"VM", BuildVM(VMConfig{Location: "eastus", VMName: "vm", AdminUsername: "user", SSHPublicKey: "key"}, "/nic").Tags},
	}

	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) {
			if c.tags == nil {
				t.Fatalf("%s should have tags", c.name)
			}
			val, ok := c.tags["managedBy"]
			if !ok {
				t.Errorf("%s missing 'managedBy' tag", c.name)
			} else if val == nil || *val != expectedTag {
				t.Errorf("%s has incorrect 'managedBy' tag: got %v, want %s", c.name, val, expectedTag)
			}
		})
	}
}

// TestBuildNIC_SubnetRequired ensures subnet ID is always set in the NIC
func TestBuildNIC_SubnetRequired(t *testing.T) {
	subnetID := "/subscriptions/xxx/resourceGroups/rg/providers/Microsoft.Network/virtualNetworks/vnet/subnets/subnet"
	params := NICParams{
		Name:     "test-nic",
		Location: "eastus",
		SubnetID: subnetID,
	}
	nic := BuildNIC(params)

	if len(nic.Properties.IPConfigurations) == 0 {
		t.Fatal("Must have at least one IP configuration")
	}

	ipConfig := nic.Properties.IPConfigurations[0]
	if ipConfig.Properties.Subnet == nil {
		t.Fatal("Subnet must be set in IP configuration")
	}
	if ipConfig.Properties.Subnet.ID == nil {
		t.Fatal("Subnet ID must not be nil")
	}
	if !strings.Contains(*ipConfig.Properties.Subnet.ID, "subnets/") {
		t.Errorf("Subnet ID should contain 'subnets/', got: %s", *ipConfig.Properties.Subnet.ID)
	}
}
