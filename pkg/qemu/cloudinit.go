package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/go-logr/logr"
)

// CloudInitConfig holds the cloud-init configuration
type CloudInitConfig struct {
	InstanceID string
	Hostname   string
	UserData   string // The cloud-init user-data content
	IPAddress  string // Static IP address for the VM
	Gateway    string // Gateway IP address
}

// CloudInitGenerator generates cloud-init ISOs
type CloudInitGenerator struct {
	WorkDir string
	Logger  logr.Logger
}

// NewCloudInitGenerator creates a new CloudInitGenerator
func NewCloudInitGenerator(workDir string, logger logr.Logger) *CloudInitGenerator {
	return &CloudInitGenerator{
		WorkDir: workDir,
		Logger:  logger.WithName("cloudinit"),
	}
}

// GenerateISO creates a NoCloud cloud-init ISO from the given config
func (g *CloudInitGenerator) GenerateISO(ctx context.Context, config CloudInitConfig) (string, error) {
	// Create temp directory for cloud-init files
	tempDir := filepath.Join(g.WorkDir, config.InstanceID, "cloudinit")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cloud-init temp dir: %w", err)
	}

	// Write meta-data
	metaDataPath := filepath.Join(tempDir, "meta-data")
	metaData := fmt.Sprintf(`instance-id: %s
local-hostname: %s
`, config.InstanceID, config.Hostname)

	if err := os.WriteFile(metaDataPath, []byte(metaData), 0644); err != nil {
		return "", fmt.Errorf("failed to write meta-data: %w", err)
	}

	// Write user-data
	userDataPath := filepath.Join(tempDir, "user-data")
	if err := os.WriteFile(userDataPath, []byte(config.UserData), 0644); err != nil {
		return "", fmt.Errorf("failed to write user-data: %w", err)
	}

	// Write network-config for static IP
	if config.IPAddress != "" {
		networkConfigPath := filepath.Join(tempDir, "network-config")
		networkConfig := fmt.Sprintf(`version: 2
ethernets:
  enp0s2:
    dhcp4: false
    addresses:
      - %s/24
    routes:
      - to: default
        via: %s
    nameservers:
      addresses:
        - 8.8.8.8
        - 8.8.4.4
`, config.IPAddress, config.Gateway)
		if err := os.WriteFile(networkConfigPath, []byte(networkConfig), 0644); err != nil {
			return "", fmt.Errorf("failed to write network-config: %w", err)
		}
	}

	// Generate ISO
	isoPath := filepath.Join(g.WorkDir, config.InstanceID, "cloudinit.iso")

	g.Logger.Info("Generating cloud-init ISO", "iso", isoPath)

	// Try different ISO generation tools
	if err := g.generateISOWithTool(ctx, tempDir, isoPath); err != nil {
		return "", fmt.Errorf("failed to generate ISO: %w", err)
	}

	g.Logger.Info("Cloud-init ISO generated successfully", "iso", isoPath)
	return isoPath, nil
}

// generateISOWithTool tries to generate ISO using available tools
func (g *CloudInitGenerator) generateISOWithTool(ctx context.Context, sourceDir, isoPath string) error {
	// Try genisoimage first (most common on Debian/Ubuntu)
	if err := g.tryGenisoimge(ctx, sourceDir, isoPath); err == nil {
		return nil
	}

	// Try mkisofs (common on RHEL/CentOS)
	if err := g.tryMkisofs(ctx, sourceDir, isoPath); err == nil {
		return nil
	}

	// Try xorrisofs (modern alternative)
	if err := g.tryXorrisofs(ctx, sourceDir, isoPath); err == nil {
		return nil
	}

	return fmt.Errorf("no ISO generation tool found (tried genisoimage, mkisofs, xorrisofs)")
}

func (g *CloudInitGenerator) tryGenisoimge(ctx context.Context, sourceDir, isoPath string) error {
	cmd := exec.CommandContext(ctx, "genisoimage",
		"-output", isoPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		sourceDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		g.Logger.V(1).Info("genisoimage failed", "error", err, "output", string(output))
		return err
	}
	return nil
}

func (g *CloudInitGenerator) tryMkisofs(ctx context.Context, sourceDir, isoPath string) error {
	cmd := exec.CommandContext(ctx, "mkisofs",
		"-output", isoPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		sourceDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		g.Logger.V(1).Info("mkisofs failed", "error", err, "output", string(output))
		return err
	}
	return nil
}

func (g *CloudInitGenerator) tryXorrisofs(ctx context.Context, sourceDir, isoPath string) error {
	cmd := exec.CommandContext(ctx, "xorrisofs",
		"-output", isoPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		sourceDir,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		g.Logger.V(1).Info("xorrisofs failed", "error", err, "output", string(output))
		return err
	}
	return nil
}
