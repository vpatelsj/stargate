package qemu

import (
	"context"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"

	"github.com/go-logr/logr"
)

const (
	// DefaultBridgeName is the default bridge name for VM networking
	DefaultBridgeName = "stargate-br0"

	// DefaultBridgeIP is the default IP for the bridge
	DefaultBridgeIP = "192.168.100.1"

	// DefaultBridgeCIDR is the default CIDR for the bridge network
	DefaultBridgeCIDR = "192.168.100.0/24"

	// DefaultBridgeNetmask is the netmask for the bridge
	DefaultBridgeNetmask = "24"

	// VMIPStart is the starting IP for VMs
	VMIPStart = 11 // 192.168.100.11
)

// NetworkManager manages bridge and tap networking for VMs
type NetworkManager struct {
	BridgeName string
	BridgeIP   string
	BridgeCIDR string
	Logger     logr.Logger

	mu           sync.Mutex
	allocatedIPs map[string]string // vmName -> IP
	nextIP       int
}

// NewNetworkManager creates a new NetworkManager
func NewNetworkManager(logger logr.Logger) *NetworkManager {
	return &NetworkManager{
		BridgeName:   DefaultBridgeName,
		BridgeIP:     DefaultBridgeIP,
		BridgeCIDR:   DefaultBridgeCIDR,
		Logger:       logger.WithName("network"),
		allocatedIPs: make(map[string]string),
		nextIP:       VMIPStart,
	}
}

// SetupBridge creates and configures the bridge interface
func (nm *NetworkManager) SetupBridge(ctx context.Context) error {
	nm.Logger.Info("Setting up bridge", "name", nm.BridgeName, "ip", nm.BridgeIP)

	// Check if bridge already exists
	if nm.bridgeExists() {
		nm.Logger.Info("Bridge already exists")
		return nil
	}

	// Create bridge
	if err := nm.runIP(ctx, "link", "add", nm.BridgeName, "type", "bridge"); err != nil {
		return fmt.Errorf("failed to create bridge: %w", err)
	}

	// Set bridge IP
	if err := nm.runIP(ctx, "addr", "add", fmt.Sprintf("%s/%s", nm.BridgeIP, DefaultBridgeNetmask), "dev", nm.BridgeName); err != nil {
		return fmt.Errorf("failed to set bridge IP: %w", err)
	}

	// Bring bridge up
	if err := nm.runIP(ctx, "link", "set", nm.BridgeName, "up"); err != nil {
		return fmt.Errorf("failed to bring bridge up: %w", err)
	}

	// Enable IP forwarding
	if err := nm.enableIPForwarding(ctx); err != nil {
		return fmt.Errorf("failed to enable IP forwarding: %w", err)
	}

	// Setup NAT/masquerade
	if err := nm.setupNAT(ctx); err != nil {
		return fmt.Errorf("failed to setup NAT: %w", err)
	}

	nm.Logger.Info("Bridge setup complete")
	return nil
}

// CreateTap creates a tap device for a VM
func (nm *NetworkManager) CreateTap(ctx context.Context, vmName string) (string, error) {
	tapName := nm.getTapName(vmName)
	nm.Logger.Info("Creating tap device", "name", tapName, "vm", vmName)

	// Check if tap already exists
	if nm.tapExists(tapName) {
		nm.Logger.Info("Tap device already exists", "name", tapName)
		// Ensure it's attached to bridge (may have been detached)
		if err := nm.runIP(ctx, "link", "set", tapName, "master", nm.BridgeName); err != nil {
			nm.Logger.Info("Note: could not attach existing tap to bridge", "error", err)
		}
		// Ensure it's up
		if err := nm.runIP(ctx, "link", "set", tapName, "up"); err != nil {
			nm.Logger.Info("Note: could not bring up existing tap", "error", err)
		}
		return tapName, nil
	}

	// Create tap device
	if err := nm.runIP(ctx, "tuntap", "add", tapName, "mode", "tap"); err != nil {
		return "", fmt.Errorf("failed to create tap device: %w", err)
	}

	// Bring tap up
	if err := nm.runIP(ctx, "link", "set", tapName, "up"); err != nil {
		return "", fmt.Errorf("failed to bring tap up: %w", err)
	}

	// Add tap to bridge
	if err := nm.runIP(ctx, "link", "set", tapName, "master", nm.BridgeName); err != nil {
		return "", fmt.Errorf("failed to add tap to bridge: %w", err)
	}

	nm.Logger.Info("Tap device created", "name", tapName)
	return tapName, nil
}

// DeleteTap removes a tap device
func (nm *NetworkManager) DeleteTap(ctx context.Context, vmName string) error {
	tapName := nm.getTapName(vmName)
	nm.Logger.Info("Deleting tap device", "name", tapName)

	if !nm.tapExists(tapName) {
		return nil
	}

	if err := nm.runIP(ctx, "link", "delete", tapName); err != nil {
		return fmt.Errorf("failed to delete tap device: %w", err)
	}

	return nil
}

// AllocateIP allocates an IP address for a VM
func (nm *NetworkManager) AllocateIP(vmName string) string {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	// Check if already allocated
	if ip, ok := nm.allocatedIPs[vmName]; ok {
		return ip
	}

	// Allocate new IP
	ip := fmt.Sprintf("192.168.100.%d", nm.nextIP)
	nm.allocatedIPs[vmName] = ip
	nm.nextIP++

	nm.Logger.Info("Allocated IP", "vm", vmName, "ip", ip)
	return ip
}

// ReleaseIP releases an IP address for a VM
func (nm *NetworkManager) ReleaseIP(vmName string) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	delete(nm.allocatedIPs, vmName)
	nm.Logger.Info("Released IP", "vm", vmName)
}

// GetIP returns the allocated IP for a VM
func (nm *NetworkManager) GetIP(vmName string) (string, bool) {
	nm.mu.Lock()
	defer nm.mu.Unlock()

	ip, ok := nm.allocatedIPs[vmName]
	return ip, ok
}

// GenerateMAC generates a MAC address for a VM based on its name
func (nm *NetworkManager) GenerateMAC(vmName string) string {
	// Use a simple hash of the VM name to generate consistent MAC
	// Format: 52:54:00:XX:XX:XX (QEMU/KVM range)
	hash := 0
	for _, c := range vmName {
		hash = (hash*31 + int(c)) & 0xFFFFFF
	}

	return fmt.Sprintf("52:54:00:%02x:%02x:%02x",
		(hash>>16)&0xFF,
		(hash>>8)&0xFF,
		hash&0xFF,
	)
}

// TeardownBridge removes the bridge interface
func (nm *NetworkManager) TeardownBridge(ctx context.Context) error {
	nm.Logger.Info("Tearing down bridge", "name", nm.BridgeName)

	if !nm.bridgeExists() {
		return nil
	}

	// Remove NAT rules
	nm.removeNAT(ctx)

	// Delete bridge
	if err := nm.runIP(ctx, "link", "delete", nm.BridgeName); err != nil {
		return fmt.Errorf("failed to delete bridge: %w", err)
	}

	return nil
}

func (nm *NetworkManager) getTapName(vmName string) string {
	// Interface names max 15 chars, use hash suffix for uniqueness
	// e.g., "sim-worker-001" -> "tap-001" or use last part of name
	name := vmName
	// If name has dashes, use the last segment (e.g., "sim-worker-001" -> "001")
	parts := strings.Split(vmName, "-")
	if len(parts) > 1 {
		// Use "tap-" prefix + last part, e.g., "tap-001"
		name = fmt.Sprintf("tap-%s", parts[len(parts)-1])
	} else {
		name = fmt.Sprintf("tap-%s", vmName)
	}
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

func (nm *NetworkManager) bridgeExists() bool {
	_, err := net.InterfaceByName(nm.BridgeName)
	return err == nil
}

func (nm *NetworkManager) tapExists(tapName string) bool {
	_, err := net.InterfaceByName(tapName)
	return err == nil
}

func (nm *NetworkManager) runIP(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "ip", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (nm *NetworkManager) enableIPForwarding(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "sysctl", "-w", "net.ipv4.ip_forward=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (nm *NetworkManager) setupNAT(ctx context.Context) error {
	// Get default route interface
	defaultIface, err := nm.getDefaultInterface(ctx)
	if err != nil {
		nm.Logger.Error(err, "Failed to get default interface, NAT may not work")
		defaultIface = "eth0" // fallback
	}

	// Setup masquerade
	cmd := exec.CommandContext(ctx, "iptables",
		"-t", "nat",
		"-A", "POSTROUTING",
		"-s", nm.BridgeCIDR,
		"-o", defaultIface,
		"-j", "MASQUERADE",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		// Check if rule already exists
		if strings.Contains(string(output), "already exists") {
			return nil
		}
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}

	// Allow forwarding
	cmd = exec.CommandContext(ctx, "iptables",
		"-A", "FORWARD",
		"-i", nm.BridgeName,
		"-j", "ACCEPT",
	)
	cmd.Run() // ignore errors

	cmd = exec.CommandContext(ctx, "iptables",
		"-A", "FORWARD",
		"-o", nm.BridgeName,
		"-j", "ACCEPT",
	)
	cmd.Run() // ignore errors

	return nil
}

func (nm *NetworkManager) removeNAT(ctx context.Context) {
	defaultIface, _ := nm.getDefaultInterface(ctx)
	if defaultIface == "" {
		defaultIface = "eth0"
	}

	exec.CommandContext(ctx, "iptables",
		"-t", "nat",
		"-D", "POSTROUTING",
		"-s", nm.BridgeCIDR,
		"-o", defaultIface,
		"-j", "MASQUERADE",
	).Run()
}

func (nm *NetworkManager) getDefaultInterface(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "ip", "route", "show", "default")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}

	// Parse output like: "default via 192.168.1.1 dev eth0"
	parts := strings.Fields(string(output))
	for i, part := range parts {
		if part == "dev" && i+1 < len(parts) {
			return parts[i+1], nil
		}
	}

	return "", fmt.Errorf("could not determine default interface")
}
