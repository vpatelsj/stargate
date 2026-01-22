package azure

import (
	"fmt"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// Tags returns the standard tags applied to all resources created by mx-azure
func Tags() map[string]*string {
	return map[string]*string{
		"managedBy": to.Ptr("mx-azure"),
	}
}

// BuildResourceGroup returns an armresources.ResourceGroup for creating a resource group.
func BuildResourceGroup(location string) armresources.ResourceGroup {
	return armresources.ResourceGroup{
		Location: to.Ptr(location),
		Tags:     Tags(),
	}
}

// BuildNSG returns an armnetwork.SecurityGroup with no inbound allow rules.
// By Azure default, all inbound traffic is denied when no rules are specified.
//
// SECURITY: This NSG intentionally has no inbound allow rules.
// Access is via Tailscale only. Do NOT add inbound rules unless absolutely necessary.
func BuildNSG(name, location string) armnetwork.SecurityGroup {
	return armnetwork.SecurityGroup{
		Name:     to.Ptr(name),
		Location: to.Ptr(location),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			// No explicit security rules - Azure default denies all inbound
			SecurityRules: []*armnetwork.SecurityRule{},
		},
		Tags: Tags(),
	}
}

// BuildVNet returns an armnetwork.VirtualNetwork for creating a virtual network.
func BuildVNet(name, location, addressSpace string) armnetwork.VirtualNetwork {
	return armnetwork.VirtualNetwork{
		Name:     to.Ptr(name),
		Location: to.Ptr(location),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{to.Ptr(addressSpace)},
			},
		},
		Tags: Tags(),
	}
}

// BuildSubnet returns an armnetwork.Subnet for creating a subnet.
// If nsgID is provided, the subnet will be associated with the NSG.
func BuildSubnet(name, prefix, nsgID string) armnetwork.Subnet {
	subnet := armnetwork.Subnet{
		Name: to.Ptr(name),
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: to.Ptr(prefix),
		},
	}
	if nsgID != "" {
		subnet.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{
			ID: to.Ptr(nsgID),
		}
	}
	return subnet
}

// BuildPublicIP returns an armnetwork.PublicIPAddress for creating a static public IP.
func BuildPublicIP(name, location string) armnetwork.PublicIPAddress {
	return armnetwork.PublicIPAddress{
		Name:     to.Ptr(name),
		Location: to.Ptr(location),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
			Tier: to.Ptr(armnetwork.PublicIPAddressSKUTierRegional),
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
		Tags: Tags(),
	}
}

// NICParams holds parameters for building a network interface
type NICParams struct {
	Name       string
	Location   string
	SubnetID   string
	PublicIPID string // Optional - if empty, no public IP is associated
	NSGID      string // Optional - if empty, no NSG is associated at NIC level
}

// BuildNIC returns an armnetwork.Interface for creating a network interface.
func BuildNIC(params NICParams) armnetwork.Interface {
	ipConfig := &armnetwork.InterfaceIPConfiguration{
		Name: to.Ptr("ipconfig1"),
		Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			Subnet: &armnetwork.Subnet{
				ID: to.Ptr(params.SubnetID),
			},
			PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
		},
	}

	if params.PublicIPID != "" {
		ipConfig.Properties.PublicIPAddress = &armnetwork.PublicIPAddress{
			ID: to.Ptr(params.PublicIPID),
		}
	}

	nic := armnetwork.Interface{
		Name:     to.Ptr(params.Name),
		Location: to.Ptr(params.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{ipConfig},
		},
		Tags: Tags(),
	}

	if params.NSGID != "" {
		nic.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{
			ID: to.Ptr(params.NSGID),
		}
	}

	return nic
}

// BuildVM returns an armcompute.VirtualMachine for creating a virtual machine.
// The NICID parameter is the Azure resource ID of the network interface to attach.
func BuildVM(cfg VMConfig, nicID string) armcompute.VirtualMachine {
	vm := armcompute.VirtualMachine{
		Name:     to.Ptr(cfg.VMName),
		Location: to.Ptr(cfg.Location),
		Zones:    zonesFromConfig(cfg.Zone),
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(cfg.VMSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: to.Ptr(cfg.ImagePublisher),
					Offer:     to.Ptr(cfg.ImageOffer),
					SKU:       to.Ptr(cfg.ImageSKU),
					Version:   to.Ptr("latest"),
				},
				OSDisk: &armcompute.OSDisk{
					Name:         to.Ptr(cfg.VMName + "-osdisk"),
					CreateOption: to.Ptr(armcompute.DiskCreateOptionTypesFromImage),
					Caching:      to.Ptr(armcompute.CachingTypesReadWrite),
					ManagedDisk: &armcompute.ManagedDiskParameters{
						StorageAccountType: to.Ptr(armcompute.StorageAccountTypesPremiumLRS),
					},
				},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(cfg.VMName),
				AdminUsername: to.Ptr(cfg.AdminUsername),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH: &armcompute.SSHConfiguration{
						PublicKeys: []*armcompute.SSHPublicKey{
							{
								Path:    to.Ptr(fmt.Sprintf("/home/%s/.ssh/authorized_keys", cfg.AdminUsername)),
								KeyData: to.Ptr(cfg.SSHPublicKey),
							},
						},
					},
				},
			},
			NetworkProfile: &armcompute.NetworkProfile{
				NetworkInterfaces: []*armcompute.NetworkInterfaceReference{
					{
						ID: to.Ptr(nicID),
						Properties: &armcompute.NetworkInterfaceReferenceProperties{
							Primary: to.Ptr(true),
						},
					},
				},
			},
		},
		Tags: Tags(),
	}

	// Add custom data (cloud-init) if provided
	if cfg.CustomData != "" {
		vm.Properties.OSProfile.CustomData = to.Ptr(cfg.CustomData)
	}

	return vm
}
