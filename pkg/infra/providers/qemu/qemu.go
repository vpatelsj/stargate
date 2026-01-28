package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/vpatelsj/stargate/pkg/infra/providers"
	pkgqemu "github.com/vpatelsj/stargate/pkg/qemu"
)

// Config holds QEMU-specific settings for provisioning local VMs.
type Config struct {
	WorkDir          string // Directory for VM storage (default: /var/lib/stargate/vms)
	ImageCacheDir    string // Directory for cached images (default: /var/lib/stargate/images)
	ImageURL         string // URL for base image (default: Ubuntu cloud image)
	CPUs             int    // Number of CPUs per VM
	MemoryMB         int    // Memory in MB per VM
	DiskSizeGB       int    // Disk size in GB per VM
	TailscaleAuthKey string // Tailscale auth key for router VM
	SSHPublicKeyPath string // Path to SSH public key
	AdminUsername    string // Admin username for VMs
	SubnetCIDR       string // Subnet CIDR for route advertisement (default: 192.168.100.0/24)
}

// Provider provisions local QEMU VMs with Tailscale and returns node addresses.
type Provider struct {
	cfg     Config
	logger  logr.Logger
	network *pkgqemu.NetworkManager
	image   *pkgqemu.ImageManager
	ciGen   *pkgqemu.CloudInitGenerator
}

// NewProvider initializes QEMU provider.
func NewProvider(ctx context.Context, cfg Config) (*Provider, error) {
	logger := zap.New(zap.UseDevMode(true))

	// Set defaults
	if cfg.WorkDir == "" {
		cfg.WorkDir = pkgqemu.DefaultVMDir
	}
	if cfg.ImageCacheDir == "" {
		cfg.ImageCacheDir = pkgqemu.DefaultImageCacheDir
	}
	if cfg.CPUs == 0 {
		cfg.CPUs = pkgqemu.DefaultCPUs
	}
	if cfg.MemoryMB == 0 {
		cfg.MemoryMB = pkgqemu.DefaultMemoryMB
	}
	if cfg.DiskSizeGB == 0 {
		cfg.DiskSizeGB = pkgqemu.DefaultDiskSizeGB
	}
	if cfg.AdminUsername == "" {
		cfg.AdminUsername = "ubuntu"
	}
	if cfg.SubnetCIDR == "" {
		cfg.SubnetCIDR = pkgqemu.DefaultBridgeCIDR
	}

	network := pkgqemu.NewNetworkManager(logger)
	image := pkgqemu.NewImageManager(cfg.ImageCacheDir, logger)
	ciGen := pkgqemu.NewCloudInitGenerator(cfg.WorkDir, logger)

	return &Provider{
		cfg:     cfg,
		logger:  logger,
		network: network,
		image:   image,
		ciGen:   ciGen,
	}, nil
}

// CreateNodes provisions the requested VMs and returns their addresses.
func (p *Provider) CreateNodes(ctx context.Context, specs []providers.NodeSpec) ([]providers.NodeInfo, error) {
	// Ensure we're running as root (required for networking)
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("QEMU provider requires root privileges for networking setup")
	}

	// Setup bridge network
	fmt.Println("[qemu] setting up bridge network...")
	if err := p.network.SetupBridge(ctx); err != nil {
		return nil, fmt.Errorf("setup bridge: %w", err)
	}

	// Download/cache base image
	fmt.Println("[qemu] ensuring base image...")
	baseImage, err := p.image.EnsureImage(ctx, p.cfg.ImageURL)
	if err != nil {
		return nil, fmt.Errorf("ensure image: %w", err)
	}

	// Read SSH public key
	sshPubKey, err := os.ReadFile(p.cfg.SSHPublicKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read SSH public key: %w", err)
	}

	var nodes []providers.NodeInfo
	var routerIP string // Track router IP for workers to use as gateway for Tailscale traffic

	for i, spec := range specs {
		role := spec.Role
		if role == "" {
			role = providers.RoleWorker
		}
		isRouter := role == providers.RoleRouter

		fmt.Printf("[qemu] provisioning VM %s (role: %s)...\n", spec.Name, role)

		// Allocate IP and create tap device
		vmIP := p.network.AllocateIP(spec.Name)

		// Track router IP for workers
		if isRouter {
			routerIP = vmIP
		}

		tapDevice, err := p.network.CreateTap(ctx, spec.Name)
		if err != nil {
			return nil, fmt.Errorf("create tap for %s: %w", spec.Name, err)
		}

		// Generate a deterministic MAC address based on VM name
		mac := fmt.Sprintf("52:54:00:12:34:%02x", 10+i)

		// Generate cloud-init - only router gets Tailscale, workers stay local
		var userData string
		if isRouter {
			userData = p.generateRouterCloudInit(spec.Name, string(sshPubKey))
		} else {
			userData = p.generateWorkerCloudInit(spec.Name, string(sshPubKey), routerIP)
		}

		cloudInitISO, err := p.ciGen.GenerateISO(ctx, pkgqemu.CloudInitConfig{
			InstanceID: spec.Name,
			Hostname:   spec.Name,
			UserData:   userData,
			IPAddress:  vmIP,
			Gateway:    p.network.BridgeIP,
		})
		if err != nil {
			return nil, fmt.Errorf("generate cloud-init for %s: %w", spec.Name, err)
		}

		// Create and start VM
		vm := pkgqemu.NewVM(pkgqemu.VMConfig{
			Name:         spec.Name,
			BaseImage:    baseImage,
			CloudInitISO: cloudInitISO,
			TapDevice:    tapDevice,
			MACAddress:   mac,
			CPUs:         p.cfg.CPUs,
			MemoryMB:     p.cfg.MemoryMB,
			DiskSizeGB:   p.cfg.DiskSizeGB,
			WorkDir:      p.cfg.WorkDir,
		}, p.logger)

		if err := vm.Create(ctx); err != nil {
			return nil, fmt.Errorf("create VM %s: %w", spec.Name, err)
		}

		if err := vm.Start(ctx); err != nil {
			return nil, fmt.Errorf("start VM %s: %w", spec.Name, err)
		}

		fmt.Printf("[qemu] VM %s started with IP %s\n", spec.Name, vmIP)

		nodeInfo := providers.NodeInfo{
			Name:      spec.Name,
			Role:      role,
			PrivateIP: vmIP,
		}

		// Only router joins Tailscale; workers stay on local network
		if isRouter {
			fmt.Printf("[qemu] waiting for Tailscale on router %s...\n", spec.Name)
			tailscaleIP, tailnetFQDN, err := p.waitForTailscale(ctx, spec.Name)
			if err != nil {
				return nil, fmt.Errorf("wait for tailscale on %s: %w", spec.Name, err)
			}
			nodeInfo.TailnetFQDN = tailnetFQDN
			nodeInfo.TailscaleIP = tailscaleIP
			fmt.Printf("[qemu] router %s ready: Tailscale IP %s, FQDN %s\n", spec.Name, tailscaleIP, tailnetFQDN)
		} else {
			// Workers get router IP for SSH proxy access
			nodeInfo.RouterIP = routerIP
			fmt.Printf("[qemu] worker %s ready: local IP %s (accessible via router)\n", spec.Name, vmIP)
		}

		nodes = append(nodes, nodeInfo)
	}

	return nodes, nil
}

// generateRouterCloudInit creates cloud-init for the router VM with Tailscale and subnet advertisement
func (p *Provider) generateRouterCloudInit(hostname, sshPubKey string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true

users:
  - name: %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

package_update: true
package_upgrade: false

packages:
  - curl
  - apt-transport-https
  - ca-certificates

runcmd:
  # Enable IP forwarding for subnet routing
  - sysctl -w net.ipv4.ip_forward=1
  - sysctl -w net.ipv6.conf.all.forwarding=1
  - sed -i 's/^#*net.ipv4.ip_forward.*/net.ipv4.ip_forward=1/' /etc/sysctl.conf
  # Install Tailscale
  - curl -fsSL https://tailscale.com/install.sh | sh
  # Start Tailscale with auth key and advertise subnet route
  - tailscale up --authkey '%s' --hostname '%s' --advertise-routes=%s --accept-routes --snat-subnet-routes=true
  # Disable Tailscale SSH to allow regular SSH via Tailscale IP
  - tailscale set --ssh=false
  # Add MASQUERADE for LAN traffic going to Tailscale (for workers to reach control plane)
  - iptables -t nat -A POSTROUTING -s %s -o tailscale0 -j MASQUERADE
  # Signal that we're ready
  - touch /var/run/cloud-init-complete

final_message: "Cloud-init complete after $UPTIME seconds"
`, hostname, p.cfg.AdminUsername, strings.TrimSpace(sshPubKey), p.cfg.TailscaleAuthKey, hostname, p.cfg.SubnetCIDR, p.cfg.SubnetCIDR)
}

// generateWorkerCloudInit creates cloud-init for worker VMs (no Tailscale, local network only)
// Workers need a route to Tailscale IPs via the router VM
func (p *Provider) generateWorkerCloudInit(hostname, sshPubKey, routerIP string) string {
	return fmt.Sprintf(`#cloud-config
hostname: %s
manage_etc_hosts: true

users:
  - name: %s
    sudo: ALL=(ALL) NOPASSWD:ALL
    shell: /bin/bash
    ssh_authorized_keys:
      - %s

package_update: true
package_upgrade: false

packages:
  - curl
  - apt-transport-https
  - ca-certificates

runcmd:
  # Add route for Tailscale CGNAT range via the router VM
  - ip route add 100.64.0.0/10 via %s || true
  # Make the route persistent
  - echo "100.64.0.0/10 via %s" >> /etc/network/routes.conf || true
  # Workers don't join Tailscale - they're accessed via the router
  - echo "Worker VM initialized without Tailscale"
  # Signal that we're ready
  - touch /var/run/cloud-init-complete

final_message: "Cloud-init complete after $UPTIME seconds"
`, hostname, p.cfg.AdminUsername, strings.TrimSpace(sshPubKey), routerIP, routerIP)
}

// waitForTailscale waits for a VM to appear in Tailscale and returns its IP
func (p *Provider) waitForTailscale(ctx context.Context, vmName string) (tailscaleIP, tailnetFQDN string, err error) {
	timeout := time.After(5 * time.Minute)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", "", ctx.Err()
		case <-timeout:
			return "", "", fmt.Errorf("timeout waiting for Tailscale on %s", vmName)
		case <-ticker.C:
			// Check tailscale status for this host
			ip, fqdn, found := p.getTailscaleInfo(vmName)
			if found {
				return ip, fqdn, nil
			}
		}
	}
}

// getTailscaleInfo checks tailscale status for a given hostname
func (p *Provider) getTailscaleInfo(hostname string) (ip, fqdn string, found bool) {
	cmd := exec.Command("tailscale", "status", "--json")
	output, err := cmd.Output()
	if err != nil {
		return "", "", false
	}

	// Parse JSON output looking for the hostname
	// Simple string matching since we just need the IP
	lines := strings.Split(string(output), "\n")
	for _, line := range lines {
		if strings.Contains(line, fmt.Sprintf(`"HostName":"%s"`, hostname)) ||
			strings.Contains(line, fmt.Sprintf(`"HostName": "%s"`, hostname)) {
			// Found it, now need to extract IP from nearby lines
			// This is a simple approach; proper JSON parsing would be better
		}
	}

	// Use tailscale status text format as fallback
	cmd = exec.Command("tailscale", "status")
	output, err = cmd.Output()
	if err != nil {
		return "", "", false
	}

	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			// Format: IP hostname user@ OS ...
			name := fields[1]
			if name == hostname || strings.HasPrefix(name, hostname+".") {
				return fields[0], name, true
			}
		}
	}

	return "", "", false
}
