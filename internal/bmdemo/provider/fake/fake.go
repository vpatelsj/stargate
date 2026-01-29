// Package fake provides a simulated provider for demo and testing purposes.
package fake

import (
	"context"
	"fmt"
	"time"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

// LogCallback is called to stream logs from provider operations.
// stream is "stdout" or "stderr", data is the log content.
type LogCallback func(runID, stream string, data []byte)

// JoinMaterial contains the information needed to join a node to a cluster.
type JoinMaterial struct {
	Endpoint    string
	Token       string
	CAHash      string
	ExpiresAt   time.Time
	ClusterID   string
	Certificate []byte // optional client cert
}

// Config holds timing configuration for the fake provider.
type Config struct {
	// Step durations (defaults to fast demo timing)
	NetbootDuration  time.Duration
	RebootDuration   time.Duration
	RepaveDuration   time.Duration
	MintJoinDuration time.Duration
	JoinNodeDuration time.Duration
	VerifyDuration   time.Duration
	RMADuration      time.Duration

	// Failure simulation
	FailNetboot bool
	FailReboot  bool
	FailRepave  bool
	FailJoin    bool
	FailVerify  bool
	FailRMA     bool
}

// DefaultConfig returns fast demo timings.
func DefaultConfig() *Config {
	return &Config{
		NetbootDuration:  250 * time.Millisecond,
		RebootDuration:   500 * time.Millisecond,
		RepaveDuration:   1 * time.Second,
		MintJoinDuration: 100 * time.Millisecond,
		JoinNodeDuration: 1 * time.Second,
		VerifyDuration:   500 * time.Millisecond,
		RMADuration:      250 * time.Millisecond,
	}
}

// SlowConfig returns more realistic timings for demos.
func SlowConfig() *Config {
	return &Config{
		NetbootDuration:  2 * time.Second,
		RebootDuration:   5 * time.Second,
		RepaveDuration:   10 * time.Second,
		MintJoinDuration: 500 * time.Millisecond,
		JoinNodeDuration: 8 * time.Second,
		VerifyDuration:   3 * time.Second,
		RMADuration:      2 * time.Second,
	}
}

// Provider is a fake provider that simulates baremetal operations.
type Provider struct {
	config  *Config
	logFunc LogCallback
}

// New creates a new fake provider with the given configuration.
func New(cfg *Config, logFunc LogCallback) *Provider {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if logFunc == nil {
		logFunc = func(runID, stream string, data []byte) {} // no-op
	}
	return &Provider{
		config:  cfg,
		logFunc: logFunc,
	}
}

// log streams a log message to the callback.
func (p *Provider) log(runID, stream, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	p.logFunc(runID, stream, []byte(msg+"\n"))
}

// stdout streams to stdout.
func (p *Provider) stdout(runID, format string, args ...interface{}) {
	p.log(runID, "stdout", format, args...)
}

// stderr streams to stderr.
func (p *Provider) stderr(runID, format string, args ...interface{}) {
	p.log(runID, "stderr", format, args...)
}

// sleep with context cancellation support.
func (p *Provider) sleep(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// SetNetboot simulates setting the netboot profile on a machine.
func (p *Provider) SetNetboot(ctx context.Context, runID string, machine *pb.Machine, profile string) error {
	p.stdout(runID, "[netboot] Setting netboot profile: %s", profile)
	p.stdout(runID, "[netboot] Machine: %s", machine.GetMachineId())

	if err := p.sleep(ctx, p.config.NetbootDuration/2); err != nil {
		return err
	}

	if p.config.FailNetboot {
		p.stderr(runID, "[netboot] ERROR: Failed to set netboot profile")
		return fmt.Errorf("simulated netboot failure")
	}

	p.stdout(runID, "[netboot] IPXE configuration updated")

	if err := p.sleep(ctx, p.config.NetbootDuration/2); err != nil {
		return err
	}

	p.stdout(runID, "[netboot] Netboot profile set successfully")
	return nil
}

// Reboot simulates rebooting a machine.
func (p *Provider) Reboot(ctx context.Context, runID string, machine *pb.Machine, force bool) error {
	mode := "graceful"
	if force {
		mode = "forced"
	}
	p.stdout(runID, "[reboot] Initiating %s reboot", mode)
	p.stdout(runID, "[reboot] Machine: %s", machine.GetMachineId())

	if err := p.sleep(ctx, p.config.RebootDuration/3); err != nil {
		return err
	}

	if p.config.FailReboot {
		p.stderr(runID, "[reboot] ERROR: Failed to reboot machine")
		return fmt.Errorf("simulated reboot failure")
	}

	p.stdout(runID, "[reboot] Sending reboot command via BMC...")

	if err := p.sleep(ctx, p.config.RebootDuration/3); err != nil {
		return err
	}

	p.stdout(runID, "[reboot] Machine is rebooting...")

	if err := p.sleep(ctx, p.config.RebootDuration/3); err != nil {
		return err
	}

	p.stdout(runID, "[reboot] Reboot completed, machine coming back online")
	return nil
}

// Repave simulates reprovisioning a machine with a new image.
func (p *Provider) Repave(ctx context.Context, runID string, machine *pb.Machine, imageRef, cloudInitRef string) error {
	p.stdout(runID, "[repave] Starting repave operation")
	p.stdout(runID, "[repave] Machine: %s", machine.GetMachineId())
	p.stdout(runID, "[repave] Image: %s", imageRef)
	p.stdout(runID, "[repave] Cloud-init: %s", cloudInitRef)

	steps := []string{
		"Downloading image...",
		"Verifying image checksum...",
		"Preparing cloud-init configuration...",
		"Writing image to disk...",
		"Configuring bootloader...",
		"Finalizing installation...",
	}

	stepDuration := p.config.RepaveDuration / time.Duration(len(steps))

	for _, step := range steps {
		p.stdout(runID, "[repave] %s", step)
		if err := p.sleep(ctx, stepDuration); err != nil {
			return err
		}
	}

	if p.config.FailRepave {
		p.stderr(runID, "[repave] ERROR: Repave failed during finalization")
		return fmt.Errorf("simulated repave failure")
	}

	p.stdout(runID, "[repave] Repave completed successfully")
	return nil
}

// MintJoinMaterial generates join material for adding a node to a cluster.
func (p *Provider) MintJoinMaterial(ctx context.Context, runID string, targetCluster *pb.TargetClusterRef) (*JoinMaterial, error) {
	p.stdout(runID, "[join-material] Generating join material for cluster: %s", targetCluster.GetClusterId())

	if err := p.sleep(ctx, p.config.MintJoinDuration); err != nil {
		return nil, err
	}

	// Generate dummy join material
	material := &JoinMaterial{
		Endpoint:  "https://k8s-api.example.com:6443",
		Token:     fmt.Sprintf("abcdef.%d", time.Now().UnixNano()%1000000),
		CAHash:    "sha256:1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
		ExpiresAt: time.Now().Add(24 * time.Hour),
		ClusterID: targetCluster.GetClusterId(),
	}

	p.stdout(runID, "[join-material] Join token generated (expires: %s)", material.ExpiresAt.Format(time.RFC3339))
	return material, nil
}

// JoinNode simulates joining a node to a Kubernetes cluster.
func (p *Provider) JoinNode(ctx context.Context, runID string, machine *pb.Machine, material *JoinMaterial) error {
	p.stdout(runID, "[join] Joining node to cluster: %s", material.ClusterID)
	p.stdout(runID, "[join] Machine: %s", machine.GetMachineId())
	p.stdout(runID, "[join] API endpoint: %s", material.Endpoint)

	steps := []string{
		"Configuring kubelet...",
		"Downloading cluster CA...",
		"Initializing node registration...",
		"Waiting for kubelet to start...",
		"Node registered with control plane...",
	}

	stepDuration := p.config.JoinNodeDuration / time.Duration(len(steps))

	for _, step := range steps {
		p.stdout(runID, "[join] %s", step)
		if err := p.sleep(ctx, stepDuration); err != nil {
			return err
		}
	}

	if p.config.FailJoin {
		p.stderr(runID, "[join] ERROR: Failed to join cluster - connection refused")
		return fmt.Errorf("simulated join failure")
	}

	p.stdout(runID, "[join] Node successfully joined cluster")
	return nil
}

// VerifyInCluster verifies that a machine is properly joined to a cluster.
func (p *Provider) VerifyInCluster(ctx context.Context, runID string, machine *pb.Machine, targetCluster *pb.TargetClusterRef) error {
	p.stdout(runID, "[verify] Verifying node membership in cluster: %s", targetCluster.GetClusterId())
	p.stdout(runID, "[verify] Machine: %s", machine.GetMachineId())

	if err := p.sleep(ctx, p.config.VerifyDuration/3); err != nil {
		return err
	}

	p.stdout(runID, "[verify] Checking node registration...")

	if err := p.sleep(ctx, p.config.VerifyDuration/3); err != nil {
		return err
	}

	if p.config.FailVerify {
		p.stderr(runID, "[verify] ERROR: Node not found in cluster")
		return fmt.Errorf("simulated verify failure: node not in cluster")
	}

	p.stdout(runID, "[verify] Node status: Ready")
	p.stdout(runID, "[verify] Kubelet version: v1.33.0")
	p.stdout(runID, "[verify] Pod CIDR assigned: 10.244.1.0/24")

	if err := p.sleep(ctx, p.config.VerifyDuration/3); err != nil {
		return err
	}

	p.stdout(runID, "[verify] Verification passed - node is healthy")
	return nil
}

// RMA simulates the RMA (Return Merchandise Authorization) process.
func (p *Provider) RMA(ctx context.Context, runID string, machine *pb.Machine, reason string) error {
	p.stdout(runID, "[rma] Initiating RMA process")
	p.stdout(runID, "[rma] Machine: %s", machine.GetMachineId())
	p.stdout(runID, "[rma] Reason: %s", reason)

	if err := p.sleep(ctx, p.config.RMADuration/2); err != nil {
		return err
	}

	if p.config.FailRMA {
		p.stderr(runID, "[rma] ERROR: Failed to initiate RMA")
		return fmt.Errorf("simulated RMA failure")
	}

	p.stdout(runID, "[rma] Machine marked for RMA")
	p.stdout(runID, "[rma] Ticket created: RMA-%d", time.Now().UnixNano()%100000)

	if err := p.sleep(ctx, p.config.RMADuration/2); err != nil {
		return err
	}

	p.stdout(runID, "[rma] RMA process initiated successfully")
	return nil
}

// ExecuteSSHCommand simulates running an SSH command on a machine.
func (p *Provider) ExecuteSSHCommand(ctx context.Context, runID string, machine *pb.Machine, scriptRef string, args map[string]string) error {
	p.stdout(runID, "[ssh] Executing script: %s", scriptRef)
	p.stdout(runID, "[ssh] Machine: %s (endpoint: %s)", machine.GetMachineId(), machine.GetSpec().GetSshEndpoint())

	if len(args) > 0 {
		p.stdout(runID, "[ssh] Arguments:")
		for k, v := range args {
			p.stdout(runID, "[ssh]   %s=%s", k, v)
		}
	}

	// Simulate script execution with some output
	if err := p.sleep(ctx, 200*time.Millisecond); err != nil {
		return err
	}

	p.stdout(runID, "[ssh] Connecting to %s...", machine.GetSpec().GetSshEndpoint())

	if err := p.sleep(ctx, 300*time.Millisecond); err != nil {
		return err
	}

	p.stdout(runID, "[ssh] Running %s...", scriptRef)
	p.stdout(runID, "[ssh] + apt-get update")

	if err := p.sleep(ctx, 200*time.Millisecond); err != nil {
		return err
	}

	p.stdout(runID, "[ssh] + apt-get install -y containerd")

	if err := p.sleep(ctx, 300*time.Millisecond); err != nil {
		return err
	}

	p.stdout(runID, "[ssh] Script completed with exit code 0")
	return nil
}
