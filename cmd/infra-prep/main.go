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

	"github.com/vpatelsj/stargate/pkg/infra/providers"
	"github.com/vpatelsj/stargate/pkg/infra/providers/azure"
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

	flag.StringVar(&providerName, "provider", "azure", "Provider to use (azure).")
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
	flag.StringVar(&tailscaleAuthKey, "tailscale-auth-key", "", "Tailscale auth key (required).")

	flag.Parse()

	if len(vmNames) == 0 {
		vmNames = append(vmNames, "stargate-azure-vm")
	}

	switch providerName {
	case "azure":
		if subscriptionID == "" {
			die("missing --subscription-id or AZURE_SUBSCRIPTION_ID")
		}
		if tailscaleAuthKey == "" {
			die("missing --tailscale-auth-key")
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

		if err := runConnectivitySuite(nodes, adminUser); err != nil {
			die("connectivity checks failed: %v", err)
		}

		fmt.Println("Infrastructure ready and reachable.")
	default:
		die("unsupported provider %q", providerName)
	}
}

func runConnectivitySuite(nodes []providers.NodeInfo, adminUser string) error {
	for _, n := range nodes {
		hostForPing := n.TailnetFQDN
		if hostForPing == "" {
			hostForPing = n.PublicIP
		}

		fmt.Printf("[connectivity] tailscale ping %s...\n", hostForPing)
		if err := waitTailscalePing(hostForPing, 12, 10*time.Second); err != nil {
			return fmt.Errorf("tailscale ping %s: %w", hostForPing, err)
		}

		sshTargets := []string{}
		if n.TailnetFQDN != "" {
			sshTargets = append(sshTargets, n.TailnetFQDN)
		}
		if n.PublicIP != "" {
			sshTargets = append(sshTargets, n.PublicIP)
		}

		if len(sshTargets) == 0 {
			return fmt.Errorf("no ssh target for node %s", n.Name)
		}

		var sshErr error
		for _, target := range sshTargets {
			fmt.Printf("[connectivity] ssh %s@%s...\n", adminUser, target)
			if err := waitSSH(adminUser, target, 12, 10*time.Second); err == nil {
				sshErr = nil
				break
			} else {
				sshErr = err
			}
		}
		if sshErr != nil {
			return fmt.Errorf("ssh %s@%s: %w", adminUser, sshTargets[len(sshTargets)-1], sshErr)
		}
	}
	return nil
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

func execCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
