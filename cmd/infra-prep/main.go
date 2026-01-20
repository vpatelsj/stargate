package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vpatelsj/stargate/pkg/infra/providers"
	"github.com/vpatelsj/stargate/pkg/infra/providers/azure"
	"github.com/vpatelsj/stargate/pkg/infra/providers/qemu"
)

type stringSlice []string

func (s *stringSlice) String() string { return strings.Join(*s, ",") }
func (s *stringSlice) Set(val string) error {
	for _, part := range strings.Split(val, ",") {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			*s = append(*s, trimmed)
		}
	}
	return nil
}

func main() {
	var vmNames stringSlice
	var providerName string

	// Azure flags
	var subscriptionID, location, zone, resourceGroup string
	var vnetName, vnetCIDR, subnetName, subnetCIDR string
	var vmSize, adminUser, sshPubKeyPath, tailscaleAuthKey string

	// QEMU flags
	var qemuWorkDir, qemuImageCacheDir, qemuImageURL string
	var qemuCPUs, qemuMemoryMB, qemuDiskSizeGB int

	// Server CR flags
	var kubeconfig, namespace string
	var skipServerCR bool

	flag.StringVar(&providerName, "provider", "azure", "Provider to use (azure, qemu).")
	flag.Var(&vmNames, "vm", "VM name (can be repeated or comma-separated).")

	flag.StringVar(&subscriptionID, "subscription-id", os.Getenv("AZURE_SUBSCRIPTION_ID"), "Azure subscription ID.")
	flag.StringVar(&location, "location", "canadacentral", "Azure region.")
	flag.StringVar(&zone, "zone", "1", "Azure availability zone.")
	flag.StringVar(&resourceGroup, "resource-group", "stargate-vapa-rg", "Azure resource group.")
	flag.StringVar(&vnetName, "vnet-name", "stargate-vnet", "Azure VNet name.")
	flag.StringVar(&vnetCIDR, "vnet-cidr", "10.50.0.0/16", "Azure VNet CIDR.")
	flag.StringVar(&subnetName, "subnet-name", "stargate-subnet", "Azure subnet name.")
	flag.StringVar(&subnetCIDR, "subnet-cidr", "10.50.1.0/24", "Azure subnet CIDR.")
	flag.StringVar(&vmSize, "vm-size", "Standard_D2s_v5", "VM size.")
	flag.StringVar(&adminUser, "admin-username", "ubuntu", "Admin username.")
	flag.StringVar(&sshPubKeyPath, "ssh-public-key", filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa.pub"), "SSH public key path.")
	flag.StringVar(&tailscaleAuthKey, "tailscale-auth-key", os.Getenv("TAILSCALE_AUTH_KEY"), "Tailscale auth key (required).")

	// QEMU flags
	flag.StringVar(&qemuWorkDir, "qemu-work-dir", "/var/lib/stargate/vms", "QEMU: directory for VM storage.")
	flag.StringVar(&qemuImageCacheDir, "qemu-image-cache", "/var/lib/stargate/images", "QEMU: directory for cached images.")
	flag.StringVar(&qemuImageURL, "qemu-image-url", "", "QEMU: URL for base image (default: Ubuntu cloud image).")
	flag.IntVar(&qemuCPUs, "qemu-cpus", 2, "QEMU: number of CPUs per VM.")
	flag.IntVar(&qemuMemoryMB, "qemu-memory", 4096, "QEMU: memory in MB per VM.")
	flag.IntVar(&qemuDiskSizeGB, "qemu-disk", 20, "QEMU: disk size in GB per VM.")

	// Server CR flags
	// Handle kubeconfig path when running as sudo
	defaultKubeconfig := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		defaultKubeconfig = filepath.Join("/home", sudoUser, ".kube", "config")
	}
	flag.StringVar(&kubeconfig, "kubeconfig", defaultKubeconfig, "Path to kubeconfig file.")
	flag.StringVar(&namespace, "namespace", "azure-dc", "Namespace for Server CRs.")
	flag.BoolVar(&skipServerCR, "skip-server-cr", false, "Skip creating Server CRs after provisioning.")

	flag.Parse()

	if tailscaleAuthKey == "" {
		die("missing --tailscale-auth-key or TAILSCALE_AUTH_KEY")
	}

	if len(vmNames) == 0 {
		switch providerName {
		case "azure":
			vmNames = append(vmNames, "stargate-azure-vm")
		case "qemu":
			vmNames = append(vmNames, "stargate-qemu-vm")
		default:
			vmNames = append(vmNames, "stargate-vm")
		}
	}

	switch providerName {
	case "azure":
		if subscriptionID == "" {
			die("missing --subscription-id or AZURE_SUBSCRIPTION_ID")
		}

		ctx := context.Background()
		prov, err := azure.NewProvider(ctx, azure.Config{
			SubscriptionID:   subscriptionID,
			Location:         location,
			Zone:             zone,
			ResourceGroup:    resourceGroup,
			VNetName:         vnetName,
			VNetCIDR:         vnetCIDR,
			SubnetName:       subnetName,
			SubnetCIDR:       subnetCIDR,
			VMSize:           vmSize,
			AdminUsername:    adminUser,
			SSHPublicKeyPath: sshPubKeyPath,
			TailscaleAuthKey: tailscaleAuthKey,
		})
		if err != nil {
			die("azure provider init: %v", err)
		}

		var specs []providers.NodeSpec
		for _, name := range vmNames {
			specs = append(specs, providers.NodeSpec{Name: name})
		}

		nodes, err := prov.CreateNodes(ctx, specs)
		if err != nil {
			die("provision: %v", err)
		}

		// Run connectivity checks and fetch Tailscale IPs
		nodes, err = runConnectivitySuite(nodes, adminUser)
		if err != nil {
			die("connectivity checks failed: %v", err)
		}

		fmt.Println("Infrastructure ready and reachable.")

		// Create Server CRs
		if !skipServerCR {
			if err := createServerCRs(ctx, kubeconfig, namespace, nodes, adminUser); err != nil {
				die("create server CRs: %v", err)
			}
			fmt.Println("Server CRs created successfully.")
		}

	case "qemu":
		ctx := context.Background()
		prov, err := qemu.NewProvider(ctx, qemu.Config{
			WorkDir:          qemuWorkDir,
			ImageCacheDir:    qemuImageCacheDir,
			ImageURL:         qemuImageURL,
			CPUs:             qemuCPUs,
			MemoryMB:         qemuMemoryMB,
			DiskSizeGB:       qemuDiskSizeGB,
			TailscaleAuthKey: tailscaleAuthKey,
			SSHPublicKeyPath: sshPubKeyPath,
			AdminUsername:    adminUser,
		})
		if err != nil {
			die("qemu provider init: %v", err)
		}

		var specs []providers.NodeSpec
		for _, name := range vmNames {
			specs = append(specs, providers.NodeSpec{Name: name})
		}

		nodes, err := prov.CreateNodes(ctx, specs)
		if err != nil {
			die("provision: %v", err)
		}

		fmt.Println("Infrastructure ready and reachable.")

		// Create Server CRs
		if !skipServerCR {
			if err := createServerCRs(ctx, kubeconfig, namespace, nodes, adminUser); err != nil {
				die("create server CRs: %v", err)
			}
			fmt.Println("Server CRs created successfully.")
		}

	default:
		die("unsupported provider %q", providerName)
	}
}

func runConnectivitySuite(nodes []providers.NodeInfo, adminUser string) ([]providers.NodeInfo, error) {
	updatedNodes := make([]providers.NodeInfo, len(nodes))
	copy(updatedNodes, nodes)

	for i, n := range updatedNodes {
		hostForPing := n.TailnetFQDN
		if hostForPing == "" {
			hostForPing = n.PublicIP
		}

		fmt.Printf("[connectivity] tailscale ping %s...\n", hostForPing)
		if err := waitTailscalePing(hostForPing, 12, 10*time.Second); err != nil {
			return nil, fmt.Errorf("tailscale ping %s: %w", hostForPing, err)
		}

		sshTargets := []string{}
		if n.TailnetFQDN != "" {
			sshTargets = append(sshTargets, n.TailnetFQDN)
		}
		if n.PublicIP != "" {
			sshTargets = append(sshTargets, n.PublicIP)
		}

		if len(sshTargets) == 0 {
			return nil, fmt.Errorf("no ssh target for node %s", n.Name)
		}

		var sshErr error
		var successfulSSHTarget string
		for _, target := range sshTargets {
			fmt.Printf("[connectivity] ssh %s@%s...\n", adminUser, target)
			if err := waitSSH(adminUser, target, 12, 10*time.Second); err == nil {
				sshErr = nil
				successfulSSHTarget = target
				break
			} else {
				sshErr = err
			}
		}
		if sshErr != nil {
			return nil, fmt.Errorf("ssh %s@%s: %w", adminUser, sshTargets[len(sshTargets)-1], sshErr)
		}

		// Fetch Tailscale IP from the VM
		tailscaleIP, err := fetchTailscaleIP(successfulSSHTarget, adminUser)
		if err != nil {
			return nil, fmt.Errorf("fetch tailscale ip from %s: %w", n.Name, err)
		}
		fmt.Printf("[connectivity] %s tailscale IP: %s\n", n.Name, tailscaleIP)
		updatedNodes[i].TailscaleIP = tailscaleIP
	}
	return updatedNodes, nil
}

func waitTailscalePing(target string, attempts int, delay time.Duration) error {
	for i := 1; i <= attempts; i++ {
		if err := tailscalePing(target); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("tailscale ping to %s did not succeed after %d attempts", target, attempts)
}

func waitSSH(user, host string, attempts int, delay time.Duration) error {
	for i := 1; i <= attempts; i++ {
		if err := sshCheck(user, host); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return fmt.Errorf("ssh to %s@%s did not succeed after %d attempts", user, host, attempts)
}

func tailscalePing(target string) error {
	cmd := execCommand("tailscale", "ping", "--timeout=5s", "--until-direct=false", target)
	return cmd.Run()
}

func sshCheck(user, host string) error {
	cmd := execCommand("ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=5",
		fmt.Sprintf("%s@%s", user, host),
		"echo", "ok",
	)
	return cmd.Run()
}

// fetchTailscaleIP retrieves the Tailscale IPv4 address from a remote host
func fetchTailscaleIP(host, user string) (string, error) {
	cmd := execCommand("ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@%s", user, host),
		"tailscale", "ip", "-4",
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("run tailscale ip: %w", err)
	}
	ip := strings.TrimSpace(string(out))
	if ip == "" {
		return "", fmt.Errorf("empty tailscale ip returned")
	}
	// Take only the first IP if multiple are returned
	lines := strings.Split(ip, "\n")
	return strings.TrimSpace(lines[0]), nil
}

func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// createServerCRs creates Server custom resources for each provisioned node
func createServerCRs(ctx context.Context, kubeconfigPath, namespace string, nodes []providers.NodeInfo, adminUser string) error {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return fmt.Errorf("load kubeconfig: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create dynamic client: %w", err)
	}

	// Ensure namespace exists
	nsGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}
	ns := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "v1",
			"kind":       "Namespace",
			"metadata": map[string]interface{}{
				"name": namespace,
			},
		},
	}
	_, err = dynClient.Resource(nsGVR).Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		// Namespace doesn't exist, create it
		fmt.Printf("[server-cr] creating namespace %s\n", namespace)
		_, err = dynClient.Resource(nsGVR).Create(ctx, ns, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("create namespace %s: %w", namespace, err)
		}
	}

	serverGVR := schema.GroupVersionResource{
		Group:    "stargate.io",
		Version:  "v1alpha1",
		Resource: "servers",
	}

	for _, node := range nodes {
		// Fetch MAC address via SSH
		mac, err := fetchMACAddress(node, adminUser)
		if err != nil {
			return fmt.Errorf("fetch MAC for %s: %w", node.Name, err)
		}

		// Use Tailscale IP for the Server CR - this is how the controller will SSH
		ipv4 := node.TailscaleIP
		if ipv4 == "" {
			// Fallback to private IP if no tailscale IP
			ipv4 = node.PrivateIP
		}

		// Build the Server CR
		server := &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "stargate.io/v1alpha1",
				"kind":       "Server",
				"metadata": map[string]interface{}{
					"name":      node.Name,
					"namespace": namespace,
				},
				"spec": map[string]interface{}{
					"mac":  mac,
					"ipv4": ipv4,
				},
			},
		}

		// Check if it already exists
		existing, err := dynClient.Resource(serverGVR).Namespace(namespace).Get(ctx, node.Name, metav1.GetOptions{})
		if err == nil {
			// Update existing
			server.SetResourceVersion(existing.GetResourceVersion())
			_, err = dynClient.Resource(serverGVR).Namespace(namespace).Update(ctx, server, metav1.UpdateOptions{})
			if err != nil {
				return fmt.Errorf("update Server CR %s: %w", node.Name, err)
			}
			fmt.Printf("[server-cr] updated Server %s/%s\n", namespace, node.Name)
		} else {
			// Create new
			_, err = dynClient.Resource(serverGVR).Namespace(namespace).Create(ctx, server, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("create Server CR %s: %w", node.Name, err)
			}
			fmt.Printf("[server-cr] created Server %s/%s\n", namespace, node.Name)
		}
	}

	return nil
}

// fetchMACAddress retrieves the primary MAC address from the node via SSH
func fetchMACAddress(node providers.NodeInfo, adminUser string) (string, error) {
	// Prefer private IP for regular SSH (avoids Tailscale SSH auth issues)
	target := node.PrivateIP
	if target == "" {
		target = node.TailscaleIP
	}
	if target == "" {
		target = node.PublicIP
	}
	if target == "" {
		target = node.TailnetFQDN
	}
	if target == "" {
		return "", fmt.Errorf("no reachable address for node %s", node.Name)
	}

	fmt.Printf("[mac-fetch] SSHing to %s@%s to get MAC address...\n", adminUser, target)

	// Use explicit SSH key to avoid Tailscale SSH
	sshKeyPath := filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa")
	// When running as sudo, HOME might be root's home, check SUDO_USER
	if sudoUser := os.Getenv("SUDO_USER"); sudoUser != "" {
		sshKeyPath = filepath.Join("/home", sudoUser, ".ssh", "id_rsa")
	}

	// First, try to find the primary non-loopback interface dynamically
	findIfaceCmd := exec.Command("ssh",
		"-i", sshKeyPath,
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "ConnectTimeout=10",
		fmt.Sprintf("%s@%s", adminUser, target),
		"sh", "-c", "ls /sys/class/net | grep -v lo | head -1",
	)

	ifaceOut, err := findIfaceCmd.Output()
	if err == nil {
		iface := strings.TrimSpace(string(ifaceOut))
		if iface != "" {
			macCmd := exec.Command("ssh",
				"-i", sshKeyPath,
				"-o", "BatchMode=yes",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-o", "ConnectTimeout=10",
				fmt.Sprintf("%s@%s", adminUser, target),
				"cat", fmt.Sprintf("/sys/class/net/%s/address", iface),
			)
			out, err := macCmd.Output()
			if err == nil {
				mac := strings.TrimSpace(string(out))
				if mac != "" {
					fmt.Printf("[mac-fetch] Got MAC %s from interface %s\n", mac, iface)
					return mac, nil
				}
			}
		}
	}

	// Fallback: try known interface names
	interfaces := []string{"eth0", "enp0s2", "enp0s3", "enp1s0", "ens3", "ens4", "ens5", "ens160", "ens192"}
	for _, iface := range interfaces {
		cmd := exec.Command("ssh",
			"-i", sshKeyPath,
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=no",
			"-o", "UserKnownHostsFile=/dev/null",
			"-o", "ConnectTimeout=10",
			fmt.Sprintf("%s@%s", adminUser, target),
			"cat", fmt.Sprintf("/sys/class/net/%s/address", iface),
		)

		out, err := cmd.Output()
		if err == nil {
			mac := strings.TrimSpace(string(out))
			if mac != "" {
				fmt.Printf("[mac-fetch] Got MAC %s from interface %s\n", mac, iface)
				return mac, nil
			}
		}
	}

	return "", fmt.Errorf("could not find MAC address on any interface")
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
