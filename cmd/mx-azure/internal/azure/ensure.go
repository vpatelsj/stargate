package azure

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// VMConfig holds configuration for creating a virtual machine
type VMConfig struct {
	Location         string
	ResourceGroup    string
	VNetName         string
	VNetAddressSpace string
	SubnetName       string
	SubnetPrefix     string
	NSGName          string
	PublicIPName     string
	NICName          string
	VMName           string
	VMSize           string
	AdminUsername    string
	SSHPublicKey     string
	ImagePublisher   string
	ImageOffer       string
	ImageSKU         string
	CustomData       string // base64-encoded cloud-init
}

// isNotFound checks if the error is an Azure 404 Not Found response
func isNotFound(err error) bool {
	var respErr *azcore.ResponseError
	if errors.As(err, &respErr) {
		return respErr.StatusCode == http.StatusNotFound
	}
	return false
}

// EnsureResourceGroup creates a resource group if it doesn't exist.
// Returns the resource group ID.
func (c *Clients) EnsureResourceGroup(ctx context.Context, name, location string) (string, error) {
	log := c.Logger.With("resource", "ResourceGroup", "name", name, "location", location)

	// Check if resource group exists
	log.Info("checking if resource group exists")
	resp, err := c.ResourceGroups.Get(ctx, name, nil)
	if err == nil {
		log.Info("resource group already exists", "id", *resp.ResourceGroup.ID)
		return *resp.ResourceGroup.ID, nil
	}
	if !isNotFound(err) {
		return "", fmt.Errorf("failed to get resource group: %w", err)
	}

	// Create resource group
	log.Info("creating resource group")
	createResp, err := c.ResourceGroups.CreateOrUpdate(ctx, name, armresources.ResourceGroup{
		Location: to.Ptr(location),
		Tags: map[string]*string{
			"managedBy": to.Ptr("mx-azure"),
		},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create resource group: %w", err)
	}

	log.Info("resource group created", "id", *createResp.ResourceGroup.ID)
	return *createResp.ResourceGroup.ID, nil
}

// EnsureNSG creates a network security group if it doesn't exist.
// The NSG has no inbound allow rules (deny-all by default).
// Returns the NSG ID.
//
// SECURITY: Even with a public IP, all inbound traffic is blocked.
// Access to the VM is via Tailscale only. The public IP exists for:
//   - Azure platform communication (extensions, monitoring)
//   - Outbound internet access (package downloads)
//   - Emergency diagnostics (if Tailscale is broken, you can add a temporary rule)
//
// Do NOT add inbound allow rules unless absolutely necessary.
func (c *Clients) EnsureNSG(ctx context.Context, resourceGroup, name, location string) (string, error) {
	log := c.Logger.With("resource", "NSG", "name", name, "resourceGroup", resourceGroup)

	// Check if NSG exists
	log.Info("checking if NSG exists")
	resp, err := c.SecurityGroups.Get(ctx, resourceGroup, name, nil)
	if err == nil {
		log.Info("NSG already exists", "id", *resp.SecurityGroup.ID)
		return *resp.SecurityGroup.ID, nil
	}
	if !isNotFound(err) {
		return "", fmt.Errorf("failed to get NSG: %w", err)
	}

	// Create NSG with no inbound allow rules (Azure default denies all inbound)
	log.Info("creating NSG")
	poller, err := c.SecurityGroups.BeginCreateOrUpdate(ctx, resourceGroup, name, armnetwork.SecurityGroup{
		Location: to.Ptr(location),
		Properties: &armnetwork.SecurityGroupPropertiesFormat{
			// No explicit security rules - default Azure behavior denies inbound
			SecurityRules: []*armnetwork.SecurityRule{},
		},
		Tags: map[string]*string{
			"managedBy": to.Ptr("mx-azure"),
		},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to begin NSG creation: %w", err)
	}

	log.Info("waiting for NSG creation to complete")
	createResp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to poll NSG creation: %w", err)
	}

	log.Info("NSG created", "id", *createResp.SecurityGroup.ID)
	return *createResp.SecurityGroup.ID, nil
}

// VNetAndSubnetResult contains the IDs of the created VNet and Subnet
type VNetAndSubnetResult struct {
	VNetID   string
	SubnetID string
}

// EnsureVNetAndSubnet creates a virtual network and subnet if they don't exist.
// Returns the VNet and Subnet IDs.
func (c *Clients) EnsureVNetAndSubnet(ctx context.Context, resourceGroup, vnetName, location, addressSpace, subnetName, subnetPrefix, nsgID string) (*VNetAndSubnetResult, error) {
	log := c.Logger.With("resource", "VNet", "vnetName", vnetName, "resourceGroup", resourceGroup)

	var vnetID string

	// Check if VNet exists
	log.Info("checking if VNet exists")
	vnetResp, err := c.VirtualNetworks.Get(ctx, resourceGroup, vnetName, nil)
	if err == nil {
		vnetID = *vnetResp.VirtualNetwork.ID
		log.Info("VNet already exists", "id", vnetID)
	} else if !isNotFound(err) {
		return nil, fmt.Errorf("failed to get VNet: %w", err)
	} else {
		// Create VNet
		log.Info("creating VNet", "addressSpace", addressSpace)
		poller, err := c.VirtualNetworks.BeginCreateOrUpdate(ctx, resourceGroup, vnetName, armnetwork.VirtualNetwork{
			Location: to.Ptr(location),
			Properties: &armnetwork.VirtualNetworkPropertiesFormat{
				AddressSpace: &armnetwork.AddressSpace{
					AddressPrefixes: []*string{to.Ptr(addressSpace)},
				},
			},
			Tags: map[string]*string{
				"managedBy": to.Ptr("mx-azure"),
			},
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to begin VNet creation: %w", err)
		}

		log.Info("waiting for VNet creation to complete")
		createResp, err := poller.PollUntilDone(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to poll VNet creation: %w", err)
		}
		vnetID = *createResp.VirtualNetwork.ID
		log.Info("VNet created", "id", vnetID)
	}

	// Now handle subnet
	subnetLog := c.Logger.With("resource", "Subnet", "name", subnetName, "vnet", vnetName)

	// Check if subnet exists
	subnetLog.Info("checking if subnet exists")
	subnetResp, err := c.Subnets.Get(ctx, resourceGroup, vnetName, subnetName, nil)
	if err == nil {
		subnetLog.Info("subnet already exists", "id", *subnetResp.Subnet.ID)
		return &VNetAndSubnetResult{
			VNetID:   vnetID,
			SubnetID: *subnetResp.Subnet.ID,
		}, nil
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("failed to get subnet: %w", err)
	}

	// Create subnet with NSG association
	subnetLog.Info("creating subnet", "prefix", subnetPrefix, "nsgID", nsgID)
	subnetParams := armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: to.Ptr(subnetPrefix),
		},
	}
	if nsgID != "" {
		subnetParams.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{
			ID: to.Ptr(nsgID),
		}
	}

	poller, err := c.Subnets.BeginCreateOrUpdate(ctx, resourceGroup, vnetName, subnetName, subnetParams, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin subnet creation: %w", err)
	}

	subnetLog.Info("waiting for subnet creation to complete")
	subnetCreateResp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to poll subnet creation: %w", err)
	}

	subnetLog.Info("subnet created", "id", *subnetCreateResp.Subnet.ID)
	return &VNetAndSubnetResult{
		VNetID:   vnetID,
		SubnetID: *subnetCreateResp.Subnet.ID,
	}, nil
}

// PublicIPResult contains the ID and IP address of the created public IP
type PublicIPResult struct {
	ID        string
	IPAddress string
}

// EnsurePublicIP creates a static public IP address if it doesn't exist.
// Returns the public IP ID and address.
func (c *Clients) EnsurePublicIP(ctx context.Context, resourceGroup, name, location string) (*PublicIPResult, error) {
	log := c.Logger.With("resource", "PublicIP", "name", name, "resourceGroup", resourceGroup)

	// Check if public IP exists
	log.Info("checking if public IP exists")
	resp, err := c.PublicIPs.Get(ctx, resourceGroup, name, nil)
	if err == nil {
		ipAddr := ""
		if resp.PublicIPAddress.Properties.IPAddress != nil {
			ipAddr = *resp.PublicIPAddress.Properties.IPAddress
		}
		log.Info("public IP already exists", "id", *resp.PublicIPAddress.ID, "address", ipAddr)
		return &PublicIPResult{
			ID:        *resp.PublicIPAddress.ID,
			IPAddress: ipAddr,
		}, nil
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("failed to get public IP: %w", err)
	}

	// Create static public IP
	log.Info("creating static public IP")
	poller, err := c.PublicIPs.BeginCreateOrUpdate(ctx, resourceGroup, name, armnetwork.PublicIPAddress{
		Location: to.Ptr(location),
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
			Tier: to.Ptr(armnetwork.PublicIPAddressSKUTierRegional),
		},
		Properties: &armnetwork.PublicIPAddressPropertiesFormat{
			PublicIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodStatic),
		},
		Tags: map[string]*string{
			"managedBy": to.Ptr("mx-azure"),
		},
	}, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin public IP creation: %w", err)
	}

	log.Info("waiting for public IP creation to complete")
	createResp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to poll public IP creation: %w", err)
	}

	ipAddr := ""
	if createResp.PublicIPAddress.Properties.IPAddress != nil {
		ipAddr = *createResp.PublicIPAddress.Properties.IPAddress
	}
	log.Info("public IP created", "id", *createResp.PublicIPAddress.ID, "address", ipAddr)
	return &PublicIPResult{
		ID:        *createResp.PublicIPAddress.ID,
		IPAddress: ipAddr,
	}, nil
}

// NICResult contains the ID of the created NIC
type NICResult struct {
	ID               string
	PrivateIPAddress string
}

// EnsureNIC creates a network interface if it doesn't exist.
// Associates the NIC with the specified subnet, public IP, and NSG.
// Returns the NIC ID.
func (c *Clients) EnsureNIC(ctx context.Context, resourceGroup, name, location, subnetID, publicIPID, nsgID string) (*NICResult, error) {
	log := c.Logger.With("resource", "NIC", "name", name, "resourceGroup", resourceGroup)

	// Check if NIC exists
	log.Info("checking if NIC exists")
	resp, err := c.Interfaces.Get(ctx, resourceGroup, name, nil)
	if err == nil {
		privateIP := ""
		if len(resp.Interface.Properties.IPConfigurations) > 0 &&
			resp.Interface.Properties.IPConfigurations[0].Properties.PrivateIPAddress != nil {
			privateIP = *resp.Interface.Properties.IPConfigurations[0].Properties.PrivateIPAddress
		}
		log.Info("NIC already exists", "id", *resp.Interface.ID, "privateIP", privateIP)
		return &NICResult{
			ID:               *resp.Interface.ID,
			PrivateIPAddress: privateIP,
		}, nil
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("failed to get NIC: %w", err)
	}

	// Build NIC parameters
	log.Info("creating NIC", "subnetID", subnetID, "publicIPID", publicIPID, "nsgID", nsgID)

	ipConfig := &armnetwork.InterfaceIPConfiguration{
		Name: to.Ptr("ipconfig1"),
		Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
			Subnet: &armnetwork.Subnet{
				ID: to.Ptr(subnetID),
			},
			PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
		},
	}

	if publicIPID != "" {
		ipConfig.Properties.PublicIPAddress = &armnetwork.PublicIPAddress{
			ID: to.Ptr(publicIPID),
		}
	}

	nicParams := armnetwork.Interface{
		Location: to.Ptr(location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{ipConfig},
		},
		Tags: map[string]*string{
			"managedBy": to.Ptr("mx-azure"),
		},
	}

	if nsgID != "" {
		nicParams.Properties.NetworkSecurityGroup = &armnetwork.SecurityGroup{
			ID: to.Ptr(nsgID),
		}
	}

	poller, err := c.Interfaces.BeginCreateOrUpdate(ctx, resourceGroup, name, nicParams, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin NIC creation: %w", err)
	}

	log.Info("waiting for NIC creation to complete")
	createResp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to poll NIC creation: %w", err)
	}

	privateIP := ""
	if len(createResp.Interface.Properties.IPConfigurations) > 0 &&
		createResp.Interface.Properties.IPConfigurations[0].Properties.PrivateIPAddress != nil {
		privateIP = *createResp.Interface.Properties.IPConfigurations[0].Properties.PrivateIPAddress
	}

	log.Info("NIC created", "id", *createResp.Interface.ID, "privateIP", privateIP)
	return &NICResult{
		ID:               *createResp.Interface.ID,
		PrivateIPAddress: privateIP,
	}, nil
}

// VMResult contains the ID of the created VM and associated public IP
type VMResult struct {
	ID              string
	Name            string
	PublicIPAddress string
}

// EnsureVM creates a virtual machine if it doesn't exist.
// Returns the VM ID and public IP address.
//
// The VM is configured with:
//   - SSH public key authentication (LinuxConfiguration.SSH.PublicKeys)
//   - Cloud-init custom data (OSProfile.CustomData) - must be base64-encoded
//
// NOTE: Azure customData has a size limit of 64KB (base64-encoded).
// Keep cloud-init scripts compact. For larger payloads, consider using
// Azure Custom Script Extension or downloading scripts from blob storage.
func (c *Clients) EnsureVM(ctx context.Context, cfg VMConfig, nicID, publicIPAddress string) (*VMResult, error) {
	log := c.Logger.With("resource", "VM", "name", cfg.VMName, "resourceGroup", cfg.ResourceGroup)

	// Check if VM exists
	log.Info("checking if VM exists")
	resp, err := c.VirtualMachines.Get(ctx, cfg.ResourceGroup, cfg.VMName, nil)
	if err == nil {
		log.Info("VM already exists", "id", *resp.VirtualMachine.ID, "publicIP", publicIPAddress)
		return &VMResult{
			ID:              *resp.VirtualMachine.ID,
			Name:            cfg.VMName,
			PublicIPAddress: publicIPAddress,
		}, nil
	}
	if !isNotFound(err) {
		return nil, fmt.Errorf("failed to get VM: %w", err)
	}

	log.Info("creating VM", "vmSize", cfg.VMSize, "image", fmt.Sprintf("%s/%s/%s", cfg.ImagePublisher, cfg.ImageOffer, cfg.ImageSKU))

	vmParams := armcompute.VirtualMachine{
		Location: to.Ptr(cfg.Location),
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
		Tags: map[string]*string{
			"managedBy": to.Ptr("mx-azure"),
		},
	}

	// Add custom data (cloud-init) if provided
	if cfg.CustomData != "" {
		vmParams.Properties.OSProfile.CustomData = to.Ptr(cfg.CustomData)
	}

	poller, err := c.VirtualMachines.BeginCreateOrUpdate(ctx, cfg.ResourceGroup, cfg.VMName, vmParams, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin VM creation: %w", err)
	}

	log.Info("waiting for VM creation to complete")
	createResp, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to poll VM creation: %w", err)
	}

	log.Info("VM created", "id", *createResp.VirtualMachine.ID, "publicIP", publicIPAddress)
	return &VMResult{
		ID:              *createResp.VirtualMachine.ID,
		Name:            cfg.VMName,
		PublicIPAddress: publicIPAddress,
	}, nil
}

// ProvisionAllResult contains the IDs of all provisioned resources
type ProvisionAllResult struct {
	ResourceGroupID string
	NSGID           string
	VNetID          string
	SubnetID        string
	PublicIPID      string
	PublicIPAddress string
	NICID           string
	VMID            string
}

// ProvisionAll provisions all Azure resources in order
func (c *Clients) ProvisionAll(ctx context.Context, cfg VMConfig) (*ProvisionAllResult, error) {
	result := &ProvisionAllResult{}
	log := c.Logger.With("operation", "ProvisionAll", "vmName", cfg.VMName)

	// 1. Resource Group
	rgID, err := c.EnsureResourceGroup(ctx, cfg.ResourceGroup, cfg.Location)
	if err != nil {
		return nil, err
	}
	result.ResourceGroupID = rgID

	// 2. Network Security Group
	nsgID, err := c.EnsureNSG(ctx, cfg.ResourceGroup, cfg.NSGName, cfg.Location)
	if err != nil {
		return nil, err
	}
	result.NSGID = nsgID

	// 3. Virtual Network and Subnet
	vnetResult, err := c.EnsureVNetAndSubnet(ctx, cfg.ResourceGroup, cfg.VNetName, cfg.Location, cfg.VNetAddressSpace, cfg.SubnetName, cfg.SubnetPrefix, nsgID)
	if err != nil {
		return nil, err
	}
	result.VNetID = vnetResult.VNetID
	result.SubnetID = vnetResult.SubnetID

	// 4. Public IP
	publicIPResult, err := c.EnsurePublicIP(ctx, cfg.ResourceGroup, cfg.PublicIPName, cfg.Location)
	if err != nil {
		return nil, err
	}
	result.PublicIPID = publicIPResult.ID
	result.PublicIPAddress = publicIPResult.IPAddress

	// 5. Network Interface
	nicResult, err := c.EnsureNIC(ctx, cfg.ResourceGroup, cfg.NICName, cfg.Location, result.SubnetID, result.PublicIPID, nsgID)
	if err != nil {
		return nil, err
	}
	result.NICID = nicResult.ID

	// 6. Virtual Machine
	vmResult, err := c.EnsureVM(ctx, cfg, result.NICID, result.PublicIPAddress)
	if err != nil {
		return nil, err
	}
	result.VMID = vmResult.ID

	log.Info("all resources provisioned successfully",
		"resourceGroupID", result.ResourceGroupID,
		"vmID", result.VMID,
		"publicIP", result.PublicIPAddress,
	)

	return result, nil
}
