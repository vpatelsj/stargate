package fake

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

// logCollector collects logs for testing.
type logCollector struct {
	mu     sync.Mutex
	logs   []string
	stdout bytes.Buffer
	stderr bytes.Buffer
}

func newLogCollector() *logCollector {
	return &logCollector{}
}

func (c *logCollector) callback(runID, stream string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.logs = append(c.logs, string(data))
	if stream == "stdout" {
		c.stdout.Write(data)
	} else {
		c.stderr.Write(data)
	}
}

func (c *logCollector) contains(s string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, log := range c.logs {
		if strings.Contains(log, s) {
			return true
		}
	}
	return false
}

func (c *logCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.logs)
}

func testMachine() *pb.Machine {
	return &pb.Machine{
		MachineId: "test-machine-1",
		Spec: &pb.MachineSpec{
			SshEndpoint: "10.0.0.1:22",
		},
	}
}

func TestSetNetboot(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.NetbootDuration = 50 * time.Millisecond
	p := New(cfg, collector.callback)

	err := p.SetNetboot(context.Background(), "run-1", testMachine(), "pxe-ubuntu-22.04")
	if err != nil {
		t.Fatalf("SetNetboot failed: %v", err)
	}

	if !collector.contains("Setting netboot profile") {
		t.Error("Expected log about setting netboot profile")
	}
	if !collector.contains("successfully") {
		t.Error("Expected success message")
	}
}

func TestSetNetboot_Failure(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.NetbootDuration = 10 * time.Millisecond
	cfg.FailNetboot = true
	p := New(cfg, collector.callback)

	err := p.SetNetboot(context.Background(), "run-1", testMachine(), "pxe-ubuntu-22.04")
	if err == nil {
		t.Fatal("Expected SetNetboot to fail")
	}

	if !collector.contains("ERROR") {
		t.Error("Expected error in logs")
	}
}

func TestReboot(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.RebootDuration = 50 * time.Millisecond
	p := New(cfg, collector.callback)

	err := p.Reboot(context.Background(), "run-1", testMachine(), false)
	if err != nil {
		t.Fatalf("Reboot failed: %v", err)
	}

	if !collector.contains("graceful") {
		t.Error("Expected graceful reboot")
	}

	// Test forced reboot
	collector2 := newLogCollector()
	p2 := New(cfg, collector2.callback)
	err = p2.Reboot(context.Background(), "run-1", testMachine(), true)
	if err != nil {
		t.Fatalf("Forced reboot failed: %v", err)
	}
	if !collector2.contains("forced") {
		t.Error("Expected forced reboot")
	}
}

func TestRepave(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.RepaveDuration = 100 * time.Millisecond
	p := New(cfg, collector.callback)

	err := p.Repave(context.Background(), "run-1", testMachine(), "ubuntu:22.04", "cloud-init-worker")
	if err != nil {
		t.Fatalf("Repave failed: %v", err)
	}

	// Check for expected log messages
	expectedLogs := []string{
		"Starting repave",
		"Downloading image",
		"Writing image to disk",
		"completed successfully",
	}
	for _, expected := range expectedLogs {
		if !collector.contains(expected) {
			t.Errorf("Expected log containing %q", expected)
		}
	}
}

func TestMintJoinMaterial(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.MintJoinDuration = 10 * time.Millisecond
	p := New(cfg, collector.callback)

	targetCluster := &pb.TargetClusterRef{ClusterId: "prod-cluster-1"}
	material, err := p.MintJoinMaterial(context.Background(), "run-1", targetCluster)
	if err != nil {
		t.Fatalf("MintJoinMaterial failed: %v", err)
	}

	if material.Endpoint == "" {
		t.Error("Expected non-empty endpoint")
	}
	if material.Token == "" {
		t.Error("Expected non-empty token")
	}
	if material.CAHash == "" {
		t.Error("Expected non-empty CA hash")
	}
	if material.ExpiresAt.Before(time.Now()) {
		t.Error("Expected expiry in the future")
	}
	if material.ClusterID != "prod-cluster-1" {
		t.Errorf("Expected cluster ID prod-cluster-1, got %s", material.ClusterID)
	}
}

func TestJoinNode(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.JoinNodeDuration = 100 * time.Millisecond
	p := New(cfg, collector.callback)

	material := &JoinMaterial{
		Endpoint:  "https://k8s-api:6443",
		Token:     "test-token",
		CAHash:    "sha256:abc123",
		ClusterID: "test-cluster",
	}

	err := p.JoinNode(context.Background(), "run-1", testMachine(), material)
	if err != nil {
		t.Fatalf("JoinNode failed: %v", err)
	}

	if !collector.contains("Joining node") {
		t.Error("Expected joining log")
	}
	if !collector.contains("successfully joined") {
		t.Error("Expected success message")
	}
}

func TestVerifyInCluster(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.VerifyDuration = 50 * time.Millisecond
	p := New(cfg, collector.callback)

	targetCluster := &pb.TargetClusterRef{ClusterId: "prod-cluster-1"}
	err := p.VerifyInCluster(context.Background(), "run-1", testMachine(), targetCluster)
	if err != nil {
		t.Fatalf("VerifyInCluster failed: %v", err)
	}

	if !collector.contains("Verifying node") {
		t.Error("Expected verify log")
	}
	if !collector.contains("Verification passed") {
		t.Error("Expected verification passed")
	}
	if !collector.contains("Ready") {
		t.Error("Expected Ready status")
	}
}

func TestRMA(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.RMADuration = 50 * time.Millisecond
	p := New(cfg, collector.callback)

	err := p.RMA(context.Background(), "run-1", testMachine(), "Hardware failure - disk")
	if err != nil {
		t.Fatalf("RMA failed: %v", err)
	}

	if !collector.contains("Initiating RMA") {
		t.Error("Expected RMA init log")
	}
	if !collector.contains("Hardware failure") {
		t.Error("Expected reason in logs")
	}
	if !collector.contains("Ticket created") {
		t.Error("Expected ticket creation")
	}
}

func TestContextCancellation(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	cfg.RepaveDuration = 5 * time.Second // Long duration
	p := New(cfg, collector.callback)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := p.Repave(ctx, "run-1", testMachine(), "ubuntu:22.04", "cloud-init")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Expected error due to context cancellation")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("Expected quick cancellation, took %v", elapsed)
	}
}

func TestExecuteSSHCommand(t *testing.T) {
	collector := newLogCollector()
	cfg := DefaultConfig()
	p := New(cfg, collector.callback)

	args := map[string]string{
		"version": "1.33",
		"arch":    "amd64",
	}

	err := p.ExecuteSSHCommand(context.Background(), "run-1", testMachine(), "install-kubelet.sh", args)
	if err != nil {
		t.Fatalf("ExecuteSSHCommand failed: %v", err)
	}

	if !collector.contains("Executing script") {
		t.Error("Expected script execution log")
	}
	if !collector.contains("install-kubelet.sh") {
		t.Error("Expected script name in logs")
	}
	if !collector.contains("exit code 0") {
		t.Error("Expected exit code 0")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.NetbootDuration == 0 {
		t.Error("Expected non-zero NetbootDuration")
	}
	if cfg.RepaveDuration == 0 {
		t.Error("Expected non-zero RepaveDuration")
	}
	if cfg.JoinNodeDuration == 0 {
		t.Error("Expected non-zero JoinNodeDuration")
	}
}

func TestSlowConfig(t *testing.T) {
	cfg := SlowConfig()

	if cfg.RepaveDuration < 5*time.Second {
		t.Error("SlowConfig should have longer RepaveDuration")
	}
	if cfg.JoinNodeDuration < 5*time.Second {
		t.Error("SlowConfig should have longer JoinNodeDuration")
	}
}

func TestNilLogCallback(t *testing.T) {
	cfg := DefaultConfig()
	cfg.NetbootDuration = 10 * time.Millisecond
	p := New(cfg, nil) // nil callback should work

	err := p.SetNetboot(context.Background(), "run-1", testMachine(), "test-profile")
	if err != nil {
		t.Fatalf("SetNetboot with nil callback failed: %v", err)
	}
}
