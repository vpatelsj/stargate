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
	cfg              Config
	rgClient         *armresources.ResourceGroupsClient
	vnetClient       *armnetwork.VirtualNetworksClient
	subnetClient     *armnetwork.SubnetsClient
	pipClient        *armnetwork.PublicIPAddressesClient
	nicClient        *armnetwork.InterfacesClient
	vmClient         *armcompute.VirtualMachinesClient
	routeTableClient *armnetwork.RouteTablesClient
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

	routeTableClient, err := armnetwork.NewRouteTablesClient(cfg.SubscriptionID, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("route table client: %w", err)
	}

	return &Provider{cfg: cfg, rgClient: rgClient, vnetClient: vnetClient, subnetClient: subnetClient, pipClient: pipClient, nicClient: nicClient, vmClient: vmClient, routeTableClient: routeTableClient}, nil
}

// CreateNodes provisions the requested VMs and returns their addresses.
// Creates router first to get its private IP for workers.
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

	// Separate router and workers
	var routerSpecs, workerSpecs []providers.NodeSpec
	for _, spec := range specs {
		role := spec.Role
		if role == "" {
			role = providers.RoleWorker
		}
		if role == providers.RoleRouter {
			routerSpecs = append(routerSpecs, spec)
		} else {
			workerSpecs = append(workerSpecs, spec)
		}
	}

	var nodes []providers.NodeInfo
	var routerIP string

	// Create routers first
	for _, spec := range routerSpecs {
		nicName := fmt.Sprintf("%s-nic", spec.Name)
		pipName := fmt.Sprintf("%s-pip", spec.Name)

		pipID, err := p.ensurePublicIP(ctx, pipName)
		if err != nil {
			return nil, fmt.Errorf("public IP %s: %w", pipName, err)
		}

		nicID, err := p.ensureNICWithIPForwarding(ctx, nicName, subnetID, pipID)
		if err != nil {
			return nil, fmt.Errorf("NIC %s: %w", nicName, err)
		}

		cloudInit, err := buildRouterCloudInit(spec.Name, p.cfg.AdminUsername, string(sshKey), p.cfg.TailscaleAuthKey, p.cfg.SubnetCIDR)
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

		routerIP = privIP // Save for workers

		nodes = append(nodes, providers.NodeInfo{
			Name:        spec.Name,
			Role:        providers.RoleRouter,
			PublicIP:    pubIP,
			PrivateIP:   privIP,
			TailnetFQDN: spec.Name,
		})
	}

	// Create route table for workers to reach Tailscale network via router
	if routerIP != "" && len(workerSpecs) > 0 {
		if err := p.ensureRouteTable(ctx, subnetID, routerIP); err != nil {
			return nil, fmt.Errorf("route table: %w", err)
		}
	}

	// Create workers with router IP for Tailscale routing
	for _, spec := range workerSpecs {
		nicName := fmt.Sprintf("%s-nic", spec.Name)

		nicID, err := p.ensureNIC(ctx, nicName, subnetID, "")
		if err != nil {
			return nil, fmt.Errorf("NIC %s: %w", nicName, err)
		}

		cloudInit, err := buildWorkerCloudInit(spec.Name, p.cfg.AdminUsername, string(sshKey), routerIP)
		if err != nil {
			return nil, err
		}

		if err := p.ensureVM(ctx, spec.Name, nicID, cloudInit, string(sshKey)); err != nil {
			return nil, fmt.Errorf("VM %s: %w", spec.Name, err)
		}

		privIP, err := p.getPrivateIP(ctx, nicName)
		if err != nil {
			return nil, fmt.Errorf("get private IP %s: %w", nicName, err)
		}

		nodes = append(nodes, providers.NodeInfo{
			Name:      spec.Name,
			Role:      providers.RoleWorker,
			PublicIP:  "",
			PrivateIP: privIP,
			RouterIP:  routerIP,
		})
	}

	return nodes, nil
}

func baseCloudInitHeader(vmName, adminUser, sshPublicKey string) string {
	return fmt.Sprintf(`#cloud-config
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
`, vmName, adminUser, sshPublicKey)
}

func buildRouterCloudInit(vmName, adminUser, sshPublicKey, tailscaleAuthKey, subnetCIDR string) (string, error) {
	if tailscaleAuthKey == "" {
		return "", fmt.Errorf("missing tailscale auth key for router %s", vmName)
	}

	cloudInit := baseCloudInitHeader(vmName, adminUser, sshPublicKey)
	cloudInit += fmt.Sprintf(`
write_files:
  - path: /tmp/configure-router.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -ex
      sysctl -w net.ipv4.ip_forward=1
      sed -i 's/^#*net.ipv4.ip_forward.*/net.ipv4.ip_forward=1/' /etc/sysctl.conf
      curl -fsSL https://tailscale.com/install.sh | sh
      tailscale up --authkey %s --hostname %s --advertise-routes=%s --accept-routes
      # MASQUERADE for LAN traffic going to Tailscale network
      iptables -t nat -A POSTROUTING -o tailscale0 -j MASQUERADE
      mkdir -p /etc/iptables
      iptables-save > /etc/iptables/rules.v4 || true

runcmd:
  - /tmp/configure-router.sh
`, tailscaleAuthKey, vmName, subnetCIDR)

	cloudInit = strings.ReplaceAll(cloudInit, "\t", "    ")
	return cloudInit, nil
}

func buildWorkerCloudInit(vmName, adminUser, sshPublicKey, routerIP string) (string, error) {
	cloudInit := baseCloudInitHeader(vmName, adminUser, sshPublicKey)

	// If router IP is provided, set up route for Tailscale CGNAT traffic
	if routerIP != "" {
		cloudInit += fmt.Sprintf(`
runcmd:
  - ip route add 100.64.0.0/10 via %s || true
  - echo "100.64.0.0/10 via %s" >> /etc/network/routes.conf || true
  - echo "worker initialized with Tailscale route via router %s"
`, routerIP, routerIP, routerIP)
	} else {
		cloudInit += `
runcmd:
  - echo "worker initialized without tailscale"
`
	}

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

	ipProps := &armnetwork.InterfaceIPConfigurationPropertiesFormat{
		Subnet:                    &armnetwork.Subnet{ID: to.Ptr(subnetID)},
		PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
	}
	if publicIPID != "" {
		ipProps.PublicIPAddress = &armnetwork.PublicIPAddress{ID: to.Ptr(publicIPID)}
	}

	poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, armnetwork.Interface{
		Location: to.Ptr(p.cfg.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name:       to.Ptr("ipconfig1"),
				Properties: ipProps,
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

// ensureNICWithIPForwarding creates a NIC with IP forwarding enabled (for routers)
func (p *Provider) ensureNICWithIPForwarding(ctx context.Context, name, subnetID, publicIPID string) (string, error) {
	existing, err := p.nicClient.Get(ctx, p.cfg.ResourceGroup, name, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("NIC has no ID")
		}
		// Ensure IP forwarding is enabled
		if existing.Properties != nil && (existing.Properties.EnableIPForwarding == nil || !*existing.Properties.EnableIPForwarding) {
			existing.Properties.EnableIPForwarding = to.Ptr(true)
			poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, existing.Interface, nil)
			if err != nil {
				return "", fmt.Errorf("enable IP forwarding on existing NIC: %w", err)
			}
			_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
			if err != nil {
				return "", err
			}
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	ipProps := &armnetwork.InterfaceIPConfigurationPropertiesFormat{
		Subnet:                    &armnetwork.Subnet{ID: to.Ptr(subnetID)},
		PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
	}
	if publicIPID != "" {
		ipProps.PublicIPAddress = &armnetwork.PublicIPAddress{ID: to.Ptr(publicIPID)}
	}

	poller, err := p.nicClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, name, armnetwork.Interface{
		Location: to.Ptr(p.cfg.Location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			EnableIPForwarding: to.Ptr(true),
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{{
				Name:       to.Ptr("ipconfig1"),
				Properties: ipProps,
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

// ensureRouteTable creates a route table with Tailscale CGNAT route and associates it with the subnet
func (p *Provider) ensureRouteTable(ctx context.Context, subnetID, routerIP string) error {
	routeTableName := fmt.Sprintf("%s-route-table", p.cfg.ResourceGroup)
	routeName := "tailscale-route"

	// Check if route table exists
	existing, err := p.routeTableClient.Get(ctx, p.cfg.ResourceGroup, routeTableName, nil)
	if err != nil && !isNotFound(err) {
		return err
	}

	var routeTableID string
	if err == nil && existing.ID != nil {
		routeTableID = *existing.ID
	} else {
		// Create route table with route
		poller, err := p.routeTableClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, routeTableName, armnetwork.RouteTable{
			Location: to.Ptr(p.cfg.Location),
			Properties: &armnetwork.RouteTablePropertiesFormat{
				Routes: []*armnetwork.Route{{
					Name: to.Ptr(routeName),
					Properties: &armnetwork.RoutePropertiesFormat{
						AddressPrefix:    to.Ptr("100.64.0.0/10"),
						NextHopType:      to.Ptr(armnetwork.RouteNextHopTypeVirtualAppliance),
						NextHopIPAddress: to.Ptr(routerIP),
					},
				}},
			},
		}, nil)
		if err != nil {
			return fmt.Errorf("create route table: %w", err)
		}

		resp, err := poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
		if err != nil {
			return fmt.Errorf("poll route table creation: %w", err)
		}
		if resp.ID == nil {
			return errors.New("route table has no ID")
		}
		routeTableID = *resp.ID
	}

	// Associate route table with subnet
	subnet, err := p.subnetClient.Get(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, p.cfg.SubnetName, nil)
	if err != nil {
		return fmt.Errorf("get subnet for route association: %w", err)
	}

	// Check if already associated
	if subnet.Properties != nil && subnet.Properties.RouteTable != nil && subnet.Properties.RouteTable.ID != nil {
		if *subnet.Properties.RouteTable.ID == routeTableID {
			return nil // Already associated
		}
	}

	// Update subnet with route table
	if subnet.Properties == nil {
		subnet.Properties = &armnetwork.SubnetPropertiesFormat{}
	}
	subnet.Properties.RouteTable = &armnetwork.RouteTable{ID: to.Ptr(routeTableID)}

	poller, err := p.subnetClient.BeginCreateOrUpdate(ctx, p.cfg.ResourceGroup, p.cfg.VNetName, p.cfg.SubnetName, subnet.Subnet, nil)
	if err != nil {
		return fmt.Errorf("associate route table with subnet: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	if err != nil {
		return fmt.Errorf("poll subnet route table association: %w", err)
	}

	return nil
}
