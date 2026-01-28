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
	Zone             string // Azure availability zone (e.g., "1", "2", "3")
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

// zonesFromConfig converts a zone string to the Azure Zones slice format.
// Returns nil if zone is empty (non-zonal deployment).
func zonesFromConfig(zone string) []*string {
	if zone == "" {
		return nil
	}
	return []*string{to.Ptr(zone)}
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

// ResourceStatus represents the existence status of a resource
type ResourceStatus struct {
	Name   string
	Exists bool
	ID     string
	Extra  map[string]string // Additional info like IP addresses
}

// ClusterStatus contains the status of all resources in a cluster
type ClusterStatus struct {
	ResourceGroup ResourceStatus
	NSG           ResourceStatus
	VNet          ResourceStatus
	Subnet        ResourceStatus
	PublicIP      ResourceStatus
	NIC           ResourceStatus
	VM            ResourceStatus
}

// GetStatus checks which resources exist and returns their status
func (c *Clients) GetStatus(ctx context.Context, cfg VMConfig) (*ClusterStatus, error) {
	log := c.Logger.With("operation", "GetStatus", "resourceGroup", cfg.ResourceGroup)
	status := &ClusterStatus{}

	// Check Resource Group
	log.Debug("checking resource group")
	rgResp, err := c.ResourceGroups.Get(ctx, cfg.ResourceGroup, nil)
	if err == nil {
		status.ResourceGroup = ResourceStatus{
			Name:   cfg.ResourceGroup,
			Exists: true,
			ID:     *rgResp.ID,
		}
	} else if isNotFound(err) {
		status.ResourceGroup = ResourceStatus{Name: cfg.ResourceGroup, Exists: false}
		// If RG doesn't exist, nothing else can exist
		return status, nil
	} else {
		return nil, fmt.Errorf("failed to get resource group: %w", err)
	}

	// Check NSG
	log.Debug("checking NSG")
	nsgResp, err := c.SecurityGroups.Get(ctx, cfg.ResourceGroup, cfg.NSGName, nil)
	if err == nil {
		status.NSG = ResourceStatus{
			Name:   cfg.NSGName,
			Exists: true,
			ID:     *nsgResp.ID,
		}
	} else if isNotFound(err) {
		status.NSG = ResourceStatus{Name: cfg.NSGName, Exists: false}
	} else {
		return nil, fmt.Errorf("failed to get NSG: %w", err)
	}

	// Check VNet
	log.Debug("checking VNet")
	vnetResp, err := c.VirtualNetworks.Get(ctx, cfg.ResourceGroup, cfg.VNetName, nil)
	if err == nil {
		status.VNet = ResourceStatus{
			Name:   cfg.VNetName,
			Exists: true,
			ID:     *vnetResp.ID,
		}
	} else if isNotFound(err) {
		status.VNet = ResourceStatus{Name: cfg.VNetName, Exists: false}
	} else {
		return nil, fmt.Errorf("failed to get VNet: %w", err)
	}

	// Check Subnet
	log.Debug("checking Subnet")
	subnetResp, err := c.Subnets.Get(ctx, cfg.ResourceGroup, cfg.VNetName, cfg.SubnetName, nil)
	if err == nil {
		status.Subnet = ResourceStatus{
			Name:   cfg.SubnetName,
			Exists: true,
			ID:     *subnetResp.ID,
		}
	} else if isNotFound(err) {
		status.Subnet = ResourceStatus{Name: cfg.SubnetName, Exists: false}
	} else {
		return nil, fmt.Errorf("failed to get Subnet: %w", err)
	}

	// Check Public IP
	log.Debug("checking Public IP")
	pipResp, err := c.PublicIPs.Get(ctx, cfg.ResourceGroup, cfg.PublicIPName, nil)
	if err == nil {
		extra := make(map[string]string)
		if pipResp.Properties.IPAddress != nil {
			extra["ipAddress"] = *pipResp.Properties.IPAddress
		}
		status.PublicIP = ResourceStatus{
			Name:   cfg.PublicIPName,
			Exists: true,
			ID:     *pipResp.ID,
			Extra:  extra,
		}
	} else if isNotFound(err) {
		status.PublicIP = ResourceStatus{Name: cfg.PublicIPName, Exists: false}
	} else {
		return nil, fmt.Errorf("failed to get Public IP: %w", err)
	}

	// Check NIC
	log.Debug("checking NIC")
	nicResp, err := c.Interfaces.Get(ctx, cfg.ResourceGroup, cfg.NICName, nil)
	if err == nil {
		extra := make(map[string]string)
		if len(nicResp.Properties.IPConfigurations) > 0 &&
			nicResp.Properties.IPConfigurations[0].Properties.PrivateIPAddress != nil {
			extra["privateIP"] = *nicResp.Properties.IPConfigurations[0].Properties.PrivateIPAddress
		}
		status.NIC = ResourceStatus{
			Name:   cfg.NICName,
			Exists: true,
			ID:     *nicResp.ID,
			Extra:  extra,
		}
	} else if isNotFound(err) {
		status.NIC = ResourceStatus{Name: cfg.NICName, Exists: false}
	} else {
		return nil, fmt.Errorf("failed to get NIC: %w", err)
	}

	// Check VM
	log.Debug("checking VM")
	vmResp, err := c.VirtualMachines.Get(ctx, cfg.ResourceGroup, cfg.VMName, nil)
	if err == nil {
		extra := make(map[string]string)
		if vmResp.Properties.ProvisioningState != nil {
			extra["provisioningState"] = *vmResp.Properties.ProvisioningState
		}
		status.VM = ResourceStatus{
			Name:   cfg.VMName,
			Exists: true,
			ID:     *vmResp.ID,
			Extra:  extra,
		}
	} else if isNotFound(err) {
		status.VM = ResourceStatus{Name: cfg.VMName, Exists: false}
	} else {
		return nil, fmt.Errorf("failed to get VM: %w", err)
	}

	return status, nil
}

// DeleteResourceGroup deletes the resource group and all its contents.
// This is a destructive operation that cannot be undone.
func (c *Clients) DeleteResourceGroup(ctx context.Context, resourceGroup string) error {
	log := c.Logger.With("operation", "DeleteResourceGroup", "resourceGroup", resourceGroup)

	// Check if resource group exists first
	log.Info("checking if resource group exists")
	_, err := c.ResourceGroups.Get(ctx, resourceGroup, nil)
	if err != nil {
		if isNotFound(err) {
			log.Info("resource group does not exist, nothing to delete")
			return nil
		}
		return fmt.Errorf("failed to check resource group: %w", err)
	}

	// Start deletion
	log.Info("deleting resource group (this may take several minutes)")
	poller, err := c.ResourceGroups.BeginDelete(ctx, resourceGroup, nil)
	if err != nil {
		return fmt.Errorf("failed to start resource group deletion: %w", err)
	}

	// Wait for completion
	log.Info("waiting for deletion to complete")
	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to complete resource group deletion: %w", err)
	}

	log.Info("resource group deleted successfully")
	return nil
}

// RouteTableConfig holds configuration for a route table
type RouteTableConfig struct {
	ResourceGroup  string
	RouteTableName string
	Location       string
}

// RouteConfig holds configuration for a single route
type RouteConfig struct {
	ResourceGroup    string
	RouteTableName   string
	RouteName        string
	AddressPrefix    string // e.g., "10.244.65.0/24"
	NextHopType      string // "VirtualAppliance" for routing through a VM
	NextHopIPAddress string // e.g., "10.237.0.4" (router IP)
}

// EnsureRouteTable creates a route table if it doesn't exist.
// Returns the route table ID.
func (c *Clients) EnsureRouteTable(ctx context.Context, cfg RouteTableConfig) (string, error) {
	log := c.Logger.With("resource", "RouteTable", "name", cfg.RouteTableName, "resourceGroup", cfg.ResourceGroup)

	// Check if route table exists
	log.Info("checking if route table exists")
	resp, err := c.RouteTables.Get(ctx, cfg.ResourceGroup, cfg.RouteTableName, nil)
	if err == nil {
		log.Info("route table already exists", "id", *resp.ID)
		return *resp.ID, nil
	}
	if !isNotFound(err) {
		return "", fmt.Errorf("failed to get route table: %w", err)
	}

	// Create route table
	log.Info("creating route table")
	poller, err := c.RouteTables.BeginCreateOrUpdate(ctx, cfg.ResourceGroup, cfg.RouteTableName, armnetwork.RouteTable{
		Location: to.Ptr(cfg.Location),
		Tags: map[string]*string{
			"managedBy": to.Ptr("mx-azure"),
		},
		Properties: &armnetwork.RouteTablePropertiesFormat{
			DisableBgpRoutePropagation: to.Ptr(false),
		},
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to start route table creation: %w", err)
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create route table: %w", err)
	}

	log.Info("route table created", "id", *result.ID)
	return *result.ID, nil
}

// EnsureRoute creates or updates a route in a route table.
// This is idempotent - if the route exists with the same config, it's a no-op.
// Returns the route ID.
func (c *Clients) EnsureRoute(ctx context.Context, cfg RouteConfig) (string, error) {
	log := c.Logger.With("resource", "Route", "name", cfg.RouteName,
		"routeTable", cfg.RouteTableName, "addressPrefix", cfg.AddressPrefix, "nextHop", cfg.NextHopIPAddress)

	// Check if route exists with correct config
	log.Info("checking if route exists")
	resp, err := c.Routes.Get(ctx, cfg.ResourceGroup, cfg.RouteTableName, cfg.RouteName, nil)
	if err == nil {
		// Route exists - check if config matches
		if resp.Properties != nil &&
			resp.Properties.AddressPrefix != nil && *resp.Properties.AddressPrefix == cfg.AddressPrefix &&
			resp.Properties.NextHopIPAddress != nil && *resp.Properties.NextHopIPAddress == cfg.NextHopIPAddress {
			log.Info("route already exists with correct config", "id", *resp.ID)
			return *resp.ID, nil
		}
		log.Info("route exists but config differs, updating")
	} else if !isNotFound(err) {
		return "", fmt.Errorf("failed to get route: %w", err)
	}

	// Create or update route
	log.Info("creating/updating route")

	nextHopType := armnetwork.RouteNextHopTypeVirtualAppliance
	if cfg.NextHopType == "VnetLocal" {
		nextHopType = armnetwork.RouteNextHopTypeVnetLocal
	}

	routeProps := &armnetwork.RoutePropertiesFormat{
		AddressPrefix: to.Ptr(cfg.AddressPrefix),
		NextHopType:   to.Ptr(nextHopType),
	}
	if cfg.NextHopIPAddress != "" {
		routeProps.NextHopIPAddress = to.Ptr(cfg.NextHopIPAddress)
	}

	poller, err := c.Routes.BeginCreateOrUpdate(ctx, cfg.ResourceGroup, cfg.RouteTableName, cfg.RouteName, armnetwork.Route{
		Properties: routeProps,
	}, nil)
	if err != nil {
		return "", fmt.Errorf("failed to start route creation: %w", err)
	}

	result, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create route: %w", err)
	}

	log.Info("route created/updated", "id", *result.ID)
	return *result.ID, nil
}

// AssociateRouteTableToSubnet associates a route table with a subnet.
// This modifies the subnet to use the specified route table.
func (c *Clients) AssociateRouteTableToSubnet(ctx context.Context, resourceGroup, vnetName, subnetName, routeTableID string) error {
	log := c.Logger.With("resource", "SubnetRouteTable", "subnet", subnetName, "routeTable", routeTableID)

	// Get current subnet config
	log.Info("getting current subnet configuration")
	resp, err := c.Subnets.Get(ctx, resourceGroup, vnetName, subnetName, nil)
	if err != nil {
		return fmt.Errorf("failed to get subnet: %w", err)
	}

	// Check if already associated
	if resp.Properties.RouteTable != nil && resp.Properties.RouteTable.ID != nil {
		if *resp.Properties.RouteTable.ID == routeTableID {
			log.Info("subnet already associated with route table")
			return nil
		}
	}

	// Update subnet with route table
	log.Info("associating route table with subnet")
	resp.Properties.RouteTable = &armnetwork.RouteTable{
		ID: to.Ptr(routeTableID),
	}

	poller, err := c.Subnets.BeginCreateOrUpdate(ctx, resourceGroup, vnetName, subnetName, resp.Subnet, nil)
	if err != nil {
		return fmt.Errorf("failed to start subnet update: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to update subnet: %w", err)
	}

	log.Info("route table associated with subnet")
	return nil
}

// AKSNodeInfo holds information about an AKS node for routing purposes
type AKSNodeInfo struct {
	Name      string // Node name (e.g., "aks-nodepool1-12345678-vmss000000")
	PrivateIP string // Node's private IP address
	PodCIDR   string // Node's pod CIDR (e.g., "10.244.0.0/24")
}

// GetAKSNodeInfo retrieves information about all nodes in an AKS cluster.
// This queries the VMSS instances to get private IPs, and uses kubectl-style
// pod CIDR assignment (sequential based on node index).
func (c *Clients) GetAKSNodeInfo(ctx context.Context, clusterRG, clusterName string) ([]AKSNodeInfo, error) {
	log := c.Logger.With("operation", "GetAKSNodeInfo", "cluster", clusterName, "resourceGroup", clusterRG)

	// Get the AKS cluster to find the node resource group
	log.Info("getting AKS cluster info")
	cluster, err := c.ManagedClusters.Get(ctx, clusterRG, clusterName, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get AKS cluster: %w", err)
	}

	if cluster.Properties == nil || cluster.Properties.NodeResourceGroup == nil {
		return nil, fmt.Errorf("AKS cluster has no node resource group")
	}
	nodeRG := *cluster.Properties.NodeResourceGroup

	// Get pod CIDR from cluster config (for calculating per-node CIDRs)
	var podCIDRBase string
	if cluster.Properties.NetworkProfile != nil && cluster.Properties.NetworkProfile.PodCidr != nil {
		podCIDRBase = *cluster.Properties.NetworkProfile.PodCidr
	} else {
		podCIDRBase = "10.244.0.0/16" // Default for Azure CNI Overlay
	}
	log.Info("using pod CIDR base", "podCIDR", podCIDRBase)

	// List all VMSS in the node resource group
	log.Info("listing VMSS in node resource group", "nodeRG", nodeRG)
	var nodes []AKSNodeInfo
	nodeIndex := 0

	pager := c.VMSS.NewListPager(nodeRG, nil)
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list VMSS: %w", err)
		}

		for _, vmss := range page.Value {
			if vmss.Name == nil {
				continue
			}
			vmssName := *vmss.Name

			// List VMs in this VMSS
			vmPager := c.VMSSVMs.NewListPager(nodeRG, vmssName, nil)
			for vmPager.More() {
				vmPage, err := vmPager.NextPage(ctx)
				if err != nil {
					log.Info("failed to list VMSS VMs", "vmss", vmssName, "error", err)
					continue
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
						// Parse NIC ID and get the NIC
						nicID := *nicRef.ID
						privateIP, err = c.getPrivateIPFromNIC(ctx, nicID)
						if err != nil {
							log.Info("failed to get private IP from NIC", "nicID", nicID, "error", err)
							continue
						}
						break
					}

					if privateIP == "" {
						continue
					}

					// Calculate pod CIDR for this node (10.244.X.0/24 where X is node index)
					podCIDR := fmt.Sprintf("10.244.%d.0/24", nodeIndex)

					nodeName := ""
					if vm.Name != nil {
						nodeName = fmt.Sprintf("%s_%s", vmssName, *vm.Name)
					}

					nodes = append(nodes, AKSNodeInfo{
						Name:      nodeName,
						PrivateIP: privateIP,
						PodCIDR:   podCIDR,
					})

					log.Info("found AKS node", "name", nodeName, "privateIP", privateIP, "podCIDR", podCIDR)
					nodeIndex++
				}
			}
		}
	}

	log.Info("discovered AKS nodes", "count", len(nodes))
	return nodes, nil
}

// getPrivateIPFromNIC extracts the private IP address from a NIC by its resource ID.
func (c *Clients) getPrivateIPFromNIC(ctx context.Context, nicID string) (string, error) {
	// Parse the NIC ID to get resource group and name
	// Format: /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/networkInterfaces/{name}
	parts := make(map[string]string)
	segments := splitResourceID(nicID)
	for i := 0; i < len(segments)-1; i += 2 {
		parts[segments[i]] = segments[i+1]
	}

	rg := parts["resourceGroups"]
	name := parts["networkInterfaces"]
	if rg == "" || name == "" {
		return "", fmt.Errorf("invalid NIC ID format: %s", nicID)
	}

	nic, err := c.Interfaces.Get(ctx, rg, name, nil)
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

	return "", fmt.Errorf("no private IP found for NIC %s", name)
}

// splitResourceID splits an Azure resource ID into its path segments.
func splitResourceID(resourceID string) []string {
	// Remove leading slash and split
	if len(resourceID) > 0 && resourceID[0] == '/' {
		resourceID = resourceID[1:]
	}
	result := []string{}
	current := ""
	for _, c := range resourceID {
		if c == '/' {
			if current != "" {
				result = append(result, current)
				current = ""
			}
		} else {
			current += string(c)
		}
	}
	if current != "" {
		result = append(result, current)
	}
	return result
}

// SyncAKSNodeRoutes ensures routes exist for all AKS node pod CIDRs.
// It creates routes in the specified route table for each node's pod CIDR
// pointing to that node's private IP.
func (c *Clients) SyncAKSNodeRoutes(ctx context.Context, routeTableRG, routeTableName, clusterRG, clusterName string) error {
	log := c.Logger.With("operation", "SyncAKSNodeRoutes", "routeTable", routeTableName, "cluster", clusterName)

	// Get AKS node information
	nodes, err := c.GetAKSNodeInfo(ctx, clusterRG, clusterName)
	if err != nil {
		return fmt.Errorf("failed to get AKS node info: %w", err)
	}

	// Create routes for each node
	for i, node := range nodes {
		routeName := fmt.Sprintf("aks-pod-cidr-%d", i)

		log.Info("ensuring route for AKS node", "node", node.Name, "podCIDR", node.PodCIDR, "nextHop", node.PrivateIP)

		_, err := c.EnsureRoute(ctx, RouteConfig{
			ResourceGroup:    routeTableRG,
			RouteTableName:   routeTableName,
			RouteName:        routeName,
			AddressPrefix:    node.PodCIDR,
			NextHopType:      "VirtualAppliance",
			NextHopIPAddress: node.PrivateIP,
		})
		if err != nil {
			return fmt.Errorf("failed to create route for node %s: %w", node.Name, err)
		}
	}

	log.Info("synced AKS node routes", "count", len(nodes))
	return nil
}
