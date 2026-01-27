package azure

import (
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

// Clients holds all Azure ARM clients needed for provisioning
type Clients struct {
	Credential     azcore.TokenCredential
	SubscriptionID string
	Logger         *slog.Logger

	ResourceGroups  *armresources.ResourceGroupsClient
	VirtualNetworks *armnetwork.VirtualNetworksClient
	Subnets         *armnetwork.SubnetsClient
	SecurityGroups  *armnetwork.SecurityGroupsClient
	PublicIPs       *armnetwork.PublicIPAddressesClient
	Interfaces      *armnetwork.InterfacesClient
	VirtualMachines *armcompute.VirtualMachinesClient
}

// NewClients creates Azure ARM clients using DefaultAzureCredential
// This supports environment variables, Azure CLI, managed identity, etc.
func NewClients(subscriptionID string, logger *slog.Logger) (*Clients, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// DefaultAzureCredential tries multiple authentication methods:
	// 1. Environment variables (AZURE_CLIENT_ID, AZURE_TENANT_ID, AZURE_CLIENT_SECRET)
	// 2. Workload Identity
	// 3. Managed Identity
	// 4. Azure CLI
	// 5. Azure Developer CLI
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, err
	}

	resourceGroupsClient, err := armresources.NewResourceGroupsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, err
	}

	vnetClient, err := armnetwork.NewVirtualNetworksClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, err
	}

	subnetClient, err := armnetwork.NewSubnetsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, err
	}

	nsgClient, err := armnetwork.NewSecurityGroupsClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, err
	}

	publicIPClient, err := armnetwork.NewPublicIPAddressesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, err
	}

	nicClient, err := armnetwork.NewInterfacesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, err
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(subscriptionID, cred, nil)
	if err != nil {
		return nil, err
	}

	return &Clients{
		Credential:      cred,
		SubscriptionID:  subscriptionID,
		Logger:          logger,
		ResourceGroups:  resourceGroupsClient,
		VirtualNetworks: vnetClient,
		Subnets:         subnetClient,
		SecurityGroups:  nsgClient,
		PublicIPs:       publicIPClient,
		Interfaces:      nicClient,
		VirtualMachines: vmClient,
	}, nil
}
