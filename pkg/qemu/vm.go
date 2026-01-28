package qemu

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
)

const (
	// DefaultVMDir is the default directory for VM storage
	DefaultVMDir = "/var/lib/stargate/vms"

	// DefaultCPUs for VMs
	DefaultCPUs = 2

	// DefaultMemoryMB for VMs
	DefaultMemoryMB = 4096

	// DefaultDiskSizeGB for VMs
	DefaultDiskSizeGB = 20
)

// VMConfig holds configuration for creating a VM
type VMConfig struct {
	Name         string
	BaseImage    string // Path to base qcow2 image
	CloudInitISO string // Path to cloud-init ISO
	TapDevice    string // Tap device name for networking
	MACAddress   string // MAC address for the VM
	CPUs         int
	MemoryMB     int
	DiskSizeGB   int
	WorkDir      string // Directory to store VM files
}

// VM represents a QEMU virtual machine
type VM struct {
	Config   VMConfig
	PIDFile  string
	DiskPath string
	Logger   logr.Logger
}

// VMStatus represents the current status of a VM
type VMStatus struct {
	Running bool
	PID     int
}

// NewVM creates a new VM instance
func NewVM(config VMConfig, logger logr.Logger) *VM {
	if config.CPUs == 0 {
		config.CPUs = DefaultCPUs
	}
	if config.MemoryMB == 0 {
		config.MemoryMB = DefaultMemoryMB
	}
	if config.DiskSizeGB == 0 {
		config.DiskSizeGB = DefaultDiskSizeGB
	}
	if config.WorkDir == "" {
		config.WorkDir = DefaultVMDir
	}

	vmDir := filepath.Join(config.WorkDir, config.Name)
	return &VM{
		Config:   config,
		PIDFile:  filepath.Join(vmDir, "qemu.pid"),
		DiskPath: filepath.Join(vmDir, "disk.qcow2"),
		Logger:   logger.WithValues("vm", config.Name),
	}
}

// Create prepares the VM disk and directory structure
func (vm *VM) Create(ctx context.Context) error {
	vmDir := filepath.Dir(vm.DiskPath)

	// Create VM directory
	if err := os.MkdirAll(vmDir, 0755); err != nil {
		return fmt.Errorf("failed to create VM directory: %w", err)
	}

	// Check if disk already exists
	if _, err := os.Stat(vm.DiskPath); err == nil {
		vm.Logger.Info("VM disk already exists, skipping creation")
		return nil
	}

	// Create disk from base image
	vm.Logger.Info("Creating VM disk", "base", vm.Config.BaseImage, "size", fmt.Sprintf("%dG", vm.Config.DiskSizeGB))

	// Create a qcow2 disk backed by the base image
	cmd := exec.CommandContext(ctx, "qemu-img", "create",
		"-f", "qcow2",
		"-F", "qcow2",
		"-b", vm.Config.BaseImage,
		vm.DiskPath,
		fmt.Sprintf("%dG", vm.Config.DiskSizeGB),
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("failed to create disk: %w: %s", err, output)
	}

	vm.Logger.Info("VM disk created successfully")
	return nil
}

// Start launches the QEMU VM
func (vm *VM) Start(ctx context.Context) error {
	status, err := vm.Status()
	if err != nil {
		return fmt.Errorf("failed to check VM status: %w", err)
	}
	if status.Running {
		vm.Logger.Info("VM is already running", "pid", status.PID)
		return nil
	}

	// Create log file for serial console output
	vmDir := filepath.Dir(vm.DiskPath)
	serialLog := filepath.Join(vmDir, "serial.log")

	// Build QEMU command
	// Note: -daemonize is incompatible with -nographic, so we use -display none
	// and redirect serial to a file
	args := []string{
		"-name", vm.Config.Name,
		"-machine", "type=q35,accel=kvm",
		"-cpu", "host",
		"-smp", strconv.Itoa(vm.Config.CPUs),
		"-m", strconv.Itoa(vm.Config.MemoryMB),
		"-drive", fmt.Sprintf("file=%s,format=qcow2,if=virtio", vm.DiskPath),
		"-netdev", fmt.Sprintf("tap,id=net0,ifname=%s,script=no,downscript=no", vm.Config.TapDevice),
		"-device", fmt.Sprintf("virtio-net-pci,netdev=net0,mac=%s", vm.Config.MACAddress),
		"-display", "none",
		"-serial", fmt.Sprintf("file:%s", serialLog),
		"-pidfile", vm.PIDFile,
		"-daemonize",
	}

	// Add cloud-init ISO if specified
	if vm.Config.CloudInitISO != "" {
		args = append(args, "-drive", fmt.Sprintf("file=%s,format=raw,if=virtio", vm.Config.CloudInitISO))
	}

	vm.Logger.Info("Starting QEMU VM", "args", args)

	cmd := exec.CommandContext(ctx, "qemu-system-x86_64", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start QEMU: %w", err)
	}

	// Wait a moment and verify it's running
	time.Sleep(500 * time.Millisecond)
	status, err = vm.Status()
	if err != nil {
		return fmt.Errorf("failed to verify VM status: %w", err)
	}
	if !status.Running {
		return fmt.Errorf("VM failed to start")
	}

	vm.Logger.Info("VM started successfully", "pid", status.PID)
	return nil
}

// Stop gracefully shuts down the VM
func (vm *VM) Stop(ctx context.Context) error {
	status, err := vm.Status()
	if err != nil {
		return fmt.Errorf("failed to check VM status: %w", err)
	}
	if !status.Running {
		vm.Logger.Info("VM is not running")
		return nil
	}

	vm.Logger.Info("Stopping VM", "pid", status.PID)

	// Send SIGTERM first for graceful shutdown
	process, err := os.FindProcess(status.PID)
	if err != nil {
		return fmt.Errorf("failed to find process: %w", err)
	}

	if err := process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("failed to send SIGTERM: %w", err)
	}

	// Wait for process to exit (with timeout)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		status, _ := vm.Status()
		if !status.Running {
			vm.Logger.Info("VM stopped successfully")
			return nil
		}
		time.Sleep(1 * time.Second)
	}

	// Force kill if still running
	vm.Logger.Info("VM did not stop gracefully, sending SIGKILL")
	if err := process.Signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("failed to send SIGKILL: %w", err)
	}

	return nil
}

// Status returns the current status of the VM
func (vm *VM) Status() (VMStatus, error) {
	status := VMStatus{}

	// Read PID file
	data, err := os.ReadFile(vm.PIDFile)
	if err != nil {
		if os.IsNotExist(err) {
			return status, nil
		}
		return status, fmt.Errorf("failed to read PID file: %w", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return status, fmt.Errorf("invalid PID in file: %w", err)
	}

	// Check if process is running
	process, err := os.FindProcess(pid)
	if err != nil {
		return status, nil
	}

	// On Unix, FindProcess always succeeds, so we need to send signal 0
	err = process.Signal(syscall.Signal(0))
	if err != nil {
		// Process is not running, clean up PID file
		os.Remove(vm.PIDFile)
		return status, nil
	}

	status.Running = true
	status.PID = pid
	return status, nil
}

// Destroy removes the VM and all its files
func (vm *VM) Destroy(ctx context.Context) error {
	// Stop the VM first if running
	if err := vm.Stop(ctx); err != nil {
		vm.Logger.Error(err, "Failed to stop VM during destroy")
	}

	vmDir := filepath.Dir(vm.DiskPath)
	vm.Logger.Info("Destroying VM", "dir", vmDir)

	if err := os.RemoveAll(vmDir); err != nil {
		return fmt.Errorf("failed to remove VM directory: %w", err)
	}

	return nil
}
