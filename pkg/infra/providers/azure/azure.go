package azure

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"

	"github.com/vpatelsj/stargate/pkg/infra/providers"
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
}

// Provider provisions Azure VMs with base cloud-init (tailscale only) and returns node addresses.
type Provider struct {
	cfg          Config
	rgClient     *armresources.ResourceGroupsClient
	vnetClient   *armnetwork.VirtualNetworksClient
	subnetClient *armnetwork.SubnetsClient
	pipClient    *armnetwork.PublicIPAddressesClient
	nicClient    *armnetwork.InterfacesClient
	vmClient     *armcompute.VirtualMachinesClient
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

	return &Provider{cfg: cfg, rgClient: rgClient, vnetClient: vnetClient, subnetClient: subnetClient, pipClient: pipClient, nicClient: nicClient, vmClient: vmClient}, nil
}

// CreateNodes provisions the requested VMs and returns their addresses.
func (p *Provider) CreateNodes(ctx context.Context, specs []providers.NodeSpec) ([]providers.NodeInfo, error) {
	if err := p.ensureResourceGroup(ctx); err != nil {
		return nil, err
	}
	if err := p.ensureVNet(ctx); err != nil {
		return nil, err
	}
	subnetID, err := p.ensureSubnet(ctx)
	if err != nil {
		return nil, err
	}

	sshKey, err := os.ReadFile(p.cfg.SSHPublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read SSH public key: %w", err)
	}

	var nodes []providers.NodeInfo
	for _, spec := range specs {
		nicName := fmt.Sprintf("%s-nic", spec.Name)
		pipName := fmt.Sprintf("%s-pip", spec.Name)

		pipID, err := p.ensurePublicIP(ctx, pipName)
		if err != nil {
			return nil, fmt.Errorf("public IP %s: %w", pipName, err)
		}

		nicID, err := p.ensureNIC(ctx, nicName, subnetID, pipID)
		if err != nil {
			return nil, fmt.Errorf("NIC %s: %w", nicName, err)
		}

		cloudInit, err := buildBaseCloudInit(spec.Name, p.cfg.AdminUsername, string(sshKey), p.cfg.TailscaleAuthKey)
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

		nodes = append(nodes, providers.NodeInfo{
			Name:        spec.Name,
			PublicIP:    pubIP,
			PrivateIP:   privIP,
			TailnetFQDN: spec.Name,
		})
	}

	return nodes, nil
}

func buildBaseCloudInit(vmName, adminUser, sshPublicKey, tailscaleAuthKey string) (string, error) {
	if tailscaleAuthKey == "" {
		return "", fmt.Errorf("missing tailscale auth key")
	}

	if sshPublicKey == "" {
		return "", fmt.Errorf("missing SSH public key")
	}

	cloudInit := fmt.Sprintf(`#cloud-config
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

write_files:
  - path: /tmp/install-tailscale.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -ex
      curl -fsSL https://tailscale.com/install.sh | sh
			tailscale up --authkey %s --hostname %s

runcmd:
  - /tmp/install-tailscale.sh
`,
		vmName,
		adminUser,
		sshPublicKey,
		tailscaleAuthKey,
		vmName,
	)

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
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, armnetwork.Interface{
		Location: to.Ptr(p.cfg.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name: to.Ptr("ipconfig1"),
				Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
					Subnet:                    &armnetwork.Subnet{ID: to.Ptr(subnetID)},
					PublicIPAddress:           &armnetwork.PublicIPAddress{ID: to.Ptr(publicIPID)},
					PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
				},
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
