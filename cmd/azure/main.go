package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	azruntime "github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/compute/armcompute"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources"
)

type config struct {
	subscriptionID       string
	location             string
	zone                 string
	resourceGroup        string
	vnetName             string
	vnetCIDR             string
	subnetName           string
	subnetCIDR           string
	vmName               string
	vmSize               string
	adminUsername        string
	sshPublicKeyPath     string
	publicIPName         string
	nicName              string
	cloudInitPath        string
	tailscaleAuthKey     string
	kindJoinCommand      string
	controlPlaneHostname string
}

func main() {
	var cfg config

	flag.StringVar(&cfg.subscriptionID, "subscription-id", os.Getenv("AZURE_SUBSCRIPTION_ID"), "Azure subscription ID (or AZURE_SUBSCRIPTION_ID env var).")
	flag.StringVar(&cfg.location, "location", "canadacentral", "Azure region.")
	flag.StringVar(&cfg.zone, "zone", "1", "Availability zone.")
	flag.StringVar(&cfg.resourceGroup, "resource-group", "stargate-vapa-rg", "Resource group name (must include -vapa-).")
	flag.StringVar(&cfg.vnetName, "vnet-name", "stargate-vnet", "Virtual network name.")
	flag.StringVar(&cfg.vnetCIDR, "vnet-cidr", "10.50.0.0/16", "Virtual network CIDR.")
	flag.StringVar(&cfg.subnetName, "subnet-name", "stargate-subnet", "Subnet name.")
	flag.StringVar(&cfg.subnetCIDR, "subnet-cidr", "10.50.1.0/24", "Subnet CIDR.")
	flag.StringVar(&cfg.vmName, "vm-name", "stargate-azure-vm", "Virtual machine name.")
	flag.StringVar(&cfg.vmSize, "vm-size", "Standard_D2s_v5", "Virtual machine size.")
	flag.StringVar(&cfg.adminUsername, "admin-username", "ubuntu", "Admin username.")
	flag.StringVar(&cfg.sshPublicKeyPath, "ssh-public-key", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa.pub"), "Path to SSH public key.")
	flag.StringVar(&cfg.publicIPName, "public-ip-name", "stargate-pip", "Public IP resource name.")
	flag.StringVar(&cfg.nicName, "nic-name", "stargate-nic", "Network interface resource name.")
	flag.StringVar(&cfg.cloudInitPath, "cloud-init", "", "Path to custom cloud-init user-data (optional).")
	flag.StringVar(&cfg.tailscaleAuthKey, "tailscale-auth-key", "", "Tailscale auth key for tailnet join.")
	flag.StringVar(&cfg.kindJoinCommand, "kind-join-command", "", "kubeadm join command for your kind cluster.")
	flag.StringVar(&cfg.controlPlaneHostname, "control-plane-hostname", "stargate-demo-control-plane", "Hostname of the Kind control plane (for /etc/hosts entry).")
	flag.Parse()

	if cfg.subscriptionID == "" {
		exitf("missing subscription ID; set --subscription-id or AZURE_SUBSCRIPTION_ID")
	}

	if !strings.Contains(cfg.resourceGroup, "-vapa-") {
		exitf("resource group name must include -vapa- (got %q)", cfg.resourceGroup)
	}

	if cfg.tailscaleAuthKey == "" {
		exitf("missing --tailscale-auth-key for tailnet connectivity")
	}

	if cfg.kindJoinCommand == "" {
		exitf("missing --kind-join-command (kubeadm join ...)")
	}

	sshPublicKey, err := os.ReadFile(cfg.sshPublicKeyPath)
	if err != nil {
		exitf("read SSH public key: %v", err)
	}

	cloudInit, err := buildCloudInit(cfg, strings.TrimSpace(string(sshPublicKey)))
	if err != nil {
		exitf("build cloud-init: %v", err)
	}

	cred, err := azidentity.NewAzureCLICredential(nil)
	if err != nil {
		exitf("create Azure CLI credential: %v", err)
	}

	ctx := context.Background()

	rgClient, err := armresources.NewResourceGroupsClient(cfg.subscriptionID, cred, nil)
	if err != nil {
		exitf("create resource group client: %v", err)
	}

	vnetClient, err := armnetwork.NewVirtualNetworksClient(cfg.subscriptionID, cred, nil)
	if err != nil {
		exitf("create vnet client: %v", err)
	}

	subnetClient, err := armnetwork.NewSubnetsClient(cfg.subscriptionID, cred, nil)
	if err != nil {
		exitf("create subnet client: %v", err)
	}

	pipClient, err := armnetwork.NewPublicIPAddressesClient(cfg.subscriptionID, cred, nil)
	if err != nil {
		exitf("create public IP client: %v", err)
	}

	nicClient, err := armnetwork.NewInterfacesClient(cfg.subscriptionID, cred, nil)
	if err != nil {
		exitf("create NIC client: %v", err)
	}

	vmClient, err := armcompute.NewVirtualMachinesClient(cfg.subscriptionID, cred, nil)
	if err != nil {
		exitf("create VM client: %v", err)
	}

	if err := ensureResourceGroup(ctx, rgClient, cfg); err != nil {
		exitf("resource group: %v", err)
	}

	if err := ensureVNet(ctx, vnetClient, cfg); err != nil {
		exitf("virtual network: %v", err)
	}

	subnetID, err := ensureSubnet(ctx, subnetClient, cfg)
	if err != nil {
		exitf("subnet: %v", err)
	}

	publicIPID, err := ensurePublicIP(ctx, pipClient, cfg)
	if err != nil {
		exitf("public IP: %v", err)
	}

	nicID, err := ensureNIC(ctx, nicClient, cfg, subnetID, publicIPID)
	if err != nil {
		exitf("NIC: %v", err)
	}

	if err := ensureVM(ctx, vmClient, cfg, nicID, cloudInit, strings.TrimSpace(string(sshPublicKey))); err != nil {
		exitf("VM: %v", err)
	}

	fmt.Printf("Azure VM %q is provisioned in resource group %q\n", cfg.vmName, cfg.resourceGroup)
}

func buildCloudInit(cfg config, sshPublicKey string) (string, error) {
	if cfg.cloudInitPath != "" {
		data, err := os.ReadFile(cfg.cloudInitPath)
		if err != nil {
			return "", fmt.Errorf("read cloud-init file: %w", err)
		}
		return string(data), nil
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
  - socat
  - conntrack
  - ipset

write_files:
  - path: /tmp/install-tailscale.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -ex
      curl -fsSL https://tailscale.com/install.sh | sh
      tailscale up --authkey %s --hostname %s --ssh

  - path: /tmp/kubeadm-join.sh
    permissions: '0700'
    content: |
      #!/bin/bash
      set -ex
      
      # Add host entry for control plane (Kind uses internal hostname)
      echo '%s %s' >> /etc/hosts
      
      # Extract join parameters from the command
      JOIN_CMD='%s'
      
      # Parse the join command to extract token and ca-cert-hash
      API_SERVER=$(echo "$JOIN_CMD" | grep -oP 'kubeadm join \K[^\s]+')
      TOKEN=$(echo "$JOIN_CMD" | grep -oP -- '--token \K[^\s]+')
      CA_CERT_HASH=$(echo "$JOIN_CMD" | grep -oP -- '--discovery-token-ca-cert-hash \K[^\s]+')
      
      # Create kubeadm config with correct cgroupRoot for real VMs (not Kind's /kubelet)
      cat > /tmp/kubeadm-join-config.yaml <<EOF
      apiVersion: kubeadm.k8s.io/v1beta3
      kind: JoinConfiguration
      discovery:
        bootstrapToken:
          apiServerEndpoint: $API_SERVER
          token: $TOKEN
          caCertHashes:
            - $CA_CERT_HASH
      nodeRegistration:
        kubeletExtraArgs:
          cgroup-root: "/"
      ---
      apiVersion: kubelet.config.k8s.io/v1beta1
      kind: KubeletConfiguration
      cgroupRoot: /
      EOF
      
      # Run kubeadm join with config (with timeout for expired tokens)
      echo "Running kubeadm join..."
      timeout 120 kubeadm join --config /tmp/kubeadm-join-config.yaml || {
        echo "ERROR: kubeadm join failed or timed out"
        echo "Common causes: expired token, network issues, API server unreachable"
        exit 1
      }
      
      echo "Node joined successfully!"

  - path: /tmp/install-k8s.sh
    permissions: '0755'
    content: |
      #!/bin/bash
      set -ex

      # Load kernel modules
      modprobe overlay
      modprobe br_netfilter
      cat <<EOF > /etc/modules-load.d/k8s.conf
      overlay
      br_netfilter
      EOF
      cat <<EOF > /etc/sysctl.d/k8s.conf
      net.bridge.bridge-nf-call-iptables  = 1
      net.bridge.bridge-nf-call-ip6tables = 1
      net.ipv4.ip_forward                 = 1
      EOF
      sysctl --system

      # Disable swap
      swapoff -a
      sed -i '/swap/d' /etc/fstab

      # Install containerd
      mkdir -p /etc/apt/keyrings
      curl -fsSL https://download.docker.com/linux/ubuntu/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg
      echo "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" > /etc/apt/sources.list.d/docker.list
      apt-get update
      apt-get install -y containerd.io

      # Configure containerd for Kubernetes
      mkdir -p /etc/containerd
      containerd config default > /etc/containerd/config.toml
      sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml
      systemctl restart containerd
      systemctl enable containerd

      # Install Kubernetes components (v1.29)
      curl -fsSL https://pkgs.k8s.io/core:/stable:/v1.29/deb/Release.key | gpg --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg
      echo 'deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.29/deb/ /' > /etc/apt/sources.list.d/kubernetes.list
      apt-get update
      apt-get install -y kubelet kubeadm kubectl
      apt-mark hold kubelet kubeadm kubectl

      # Join the cluster
      /tmp/kubeadm-join.sh

runcmd:
  - /tmp/install-tailscale.sh
  - /tmp/install-k8s.sh
`,
		cfg.vmName,
		cfg.adminUsername,
		sshPublicKey,
		cfg.tailscaleAuthKey,
		cfg.vmName,
		extractControlPlaneIP(cfg.kindJoinCommand),
		cfg.controlPlaneHostname,
		cfg.kindJoinCommand,
	)

	return cloudInit, nil
}

// extractControlPlaneIP extracts the IP address from a kubeadm join command.
// Example: "kubeadm join 100.66.238.55:6443 --token ..." -> "100.66.238.55"
func extractControlPlaneIP(joinCommand string) string {
	re := regexp.MustCompile(`kubeadm\s+join\s+([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+):\d+`)
	matches := re.FindStringSubmatch(joinCommand)
	if len(matches) >= 2 {
		return matches[1]
	}
	return ""
}

func ensureResourceGroup(ctx context.Context, client *armresources.ResourceGroupsClient, cfg config) error {
	_, err := client.Get(ctx, cfg.resourceGroup, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}

	_, err = client.CreateOrUpdate(ctx, cfg.resourceGroup, armresources.ResourceGroup{
		Location: to.Ptr(cfg.location),
	}, nil)
	return err
}

func ensureVNet(ctx context.Context, client *armnetwork.VirtualNetworksClient, cfg config) error {
	_, err := client.Get(ctx, cfg.resourceGroup, cfg.vnetName, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}

	poller, err := client.BeginCreateOrUpdate(ctx, cfg.resourceGroup, cfg.vnetName, armnetwork.VirtualNetwork{
		Location: to.Ptr(cfg.location),
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace: &armnetwork.AddressSpace{
				AddressPrefixes: []*string{to.Ptr(cfg.vnetCIDR)},
			},
		},
	}, nil)
	if err != nil {
		return err
	}

	_, err = poller.PollUntilDone(ctx, &azruntime.PollUntilDoneOptions{Frequency: 10 * time.Second})
	return err
}

func ensureSubnet(ctx context.Context, client *armnetwork.SubnetsClient, cfg config) (string, error) {
	subnet, err := client.Get(ctx, cfg.resourceGroup, cfg.vnetName, cfg.subnetName, nil)
	if err == nil {
		if subnet.ID == nil {
			return "", errors.New("subnet has no ID")
		}
		return *subnet.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := client.BeginCreateOrUpdate(ctx, cfg.resourceGroup, cfg.vnetName, cfg.subnetName, armnetwork.Subnet{
		Properties: &armnetwork.SubnetPropertiesFormat{
			AddressPrefix: to.Ptr(cfg.subnetCIDR),
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
		return "", errors.New("subnet has no ID")
	}
	return *resp.ID, nil
}

func ensurePublicIP(ctx context.Context, client *armnetwork.PublicIPAddressesClient, cfg config) (string, error) {
	existing, err := client.Get(ctx, cfg.resourceGroup, cfg.publicIPName, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("public IP has no ID")
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := client.BeginCreateOrUpdate(ctx, cfg.resourceGroup, cfg.publicIPName, armnetwork.PublicIPAddress{
		Location: to.Ptr(cfg.location),
		Zones:    []*string{to.Ptr(cfg.zone)},
		SKU: &armnetwork.PublicIPAddressSKU{
			Name: to.Ptr(armnetwork.PublicIPAddressSKUNameStandard),
		},
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

func ensureNIC(ctx context.Context, client *armnetwork.InterfacesClient, cfg config, subnetID, publicIPID string) (string, error) {
	existing, err := client.Get(ctx, cfg.resourceGroup, cfg.nicName, nil)
	if err == nil {
		if existing.ID == nil {
			return "", errors.New("NIC has no ID")
		}
		return *existing.ID, nil
	}
	if !isNotFound(err) {
		return "", err
	}

	poller, err := client.BeginCreateOrUpdate(ctx, cfg.resourceGroup, cfg.nicName, armnetwork.Interface{
		Location: to.Ptr(cfg.location),
		Properties: &armnetwork.InterfacePropertiesFormat{
			IPConfigurations: []*armnetwork.InterfaceIPConfiguration{
				{
					Name: to.Ptr("ipconfig1"),
					Properties: &armnetwork.InterfaceIPConfigurationPropertiesFormat{
						Subnet: &armnetwork.Subnet{
							ID: to.Ptr(subnetID),
						},
						PublicIPAddress: &armnetwork.PublicIPAddress{
							ID: to.Ptr(publicIPID),
						},
						PrivateIPAllocationMethod: to.Ptr(armnetwork.IPAllocationMethodDynamic),
					},
				},
			},
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

func ensureVM(ctx context.Context, client *armcompute.VirtualMachinesClient, cfg config, nicID, cloudInit, sshPublicKey string) error {
	_, err := client.Get(ctx, cfg.resourceGroup, cfg.vmName, nil)
	if err == nil {
		return nil
	}
	if !isNotFound(err) {
		return err
	}

	customData := base64.StdEncoding.EncodeToString([]byte(cloudInit))
	sshKeyPath := fmt.Sprintf("/home/%s/.ssh/authorized_keys", cfg.adminUsername)

	poller, err := client.BeginCreateOrUpdate(ctx, cfg.resourceGroup, cfg.vmName, armcompute.VirtualMachine{
		Location: to.Ptr(cfg.location),
		Zones:    []*string{to.Ptr(cfg.zone)},
		Properties: &armcompute.VirtualMachineProperties{
			HardwareProfile: &armcompute.HardwareProfile{
				VMSize: to.Ptr(armcompute.VirtualMachineSizeTypes(cfg.vmSize)),
			},
			StorageProfile: &armcompute.StorageProfile{
				ImageReference: &armcompute.ImageReference{
					Publisher: to.Ptr("Canonical"),
					Offer:     to.Ptr("0001-com-ubuntu-server-jammy"),
					SKU:       to.Ptr("22_04-lts-gen2"),
					Version:   to.Ptr("latest"),
				},
			},
			OSProfile: &armcompute.OSProfile{
				ComputerName:  to.Ptr(cfg.vmName),
				AdminUsername: to.Ptr(cfg.adminUsername),
				CustomData:    to.Ptr(customData),
				LinuxConfiguration: &armcompute.LinuxConfiguration{
					DisablePasswordAuthentication: to.Ptr(true),
					SSH: &armcompute.SSHConfiguration{
						PublicKeys: []*armcompute.SSHPublicKey{
							{
								Path:    to.Ptr(sshKeyPath),
								KeyData: to.Ptr(sshPublicKey),
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

func exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
