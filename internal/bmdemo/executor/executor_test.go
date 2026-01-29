package executor

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/lifecycle"
	"github.com/vpatelsj/stargate/internal/bmdemo/plans"
	"github.com/vpatelsj/stargate/internal/bmdemo/provider/fake"
	"github.com/vpatelsj/stargate/internal/bmdemo/store"
)

func setupRunner(t *testing.T) (*Runner, *store.Store) {
	t.Helper()
	s := store.New()
	cfg := fake.DefaultConfig()
	// Make tests fast
	cfg.NetbootDuration = 10 * time.Millisecond
	cfg.RebootDuration = 10 * time.Millisecond
	cfg.RepaveDuration = 20 * time.Millisecond
	cfg.MintJoinDuration = 5 * time.Millisecond
	cfg.JoinNodeDuration = 20 * time.Millisecond
	cfg.VerifyDuration = 10 * time.Millisecond
	cfg.RMADuration = 10 * time.Millisecond

	p := fake.New(cfg, nil)
	pl := plans.NewRegistry()
	r := NewRunner(s, p, pl)
	return r, s
}

func createTestMachine(t *testing.T, s *store.Store) *pb.Machine {
	t.Helper()
	m, err := s.UpsertMachine(&pb.Machine{
		Spec: &pb.MachineSpec{
			SshEndpoint: "10.0.0.1:22",
		},
	})
	if err != nil {
		t.Fatalf("failed to create machine: %v", err)
	}
	return m
}

func TestRunner_StartRun_Success(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, err := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	// Track events
	var events []*pb.RunEvent
	var mu sync.Mutex
	runner.SubscribeEvents(func(e *pb.RunEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	// Start run
	err = runner.StartRun(context.Background(), run.RunId)
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && (r.Phase == pb.Run_SUCCEEDED || r.Phase == pb.Run_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded
	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_SUCCEEDED {
		t.Errorf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify events were emitted
	mu.Lock()
	eventCount := len(events)
	mu.Unlock()
	if eventCount == 0 {
		t.Error("Expected some events to be emitted")
	}
}

func TestRunner_StartRun_RepaveJoin(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, err := s.CreateRunIfNotExists("req-1", machine.MachineId, "REPAVE", "")
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	err = runner.StartRun(context.Background(), run.RunId)
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	// Wait for completion
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && (r.Phase == pb.Run_SUCCEEDED || r.Phase == pb.Run_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded
	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_SUCCEEDED {
		t.Errorf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify machine phase is IN_SERVICE (join completed)
	finalMachine, _ := s.GetMachine(machine.MachineId)
	if finalMachine.Status.Phase != pb.MachineStatus_IN_SERVICE {
		t.Errorf("Expected machine phase IN_SERVICE, got %v", finalMachine.Status.Phase)
	}

	// Verify InCustomerCluster condition
	hasCondition := false
	for _, c := range finalMachine.Status.Conditions {
		if c.Type == lifecycle.ConditionInCustomerCluster && c.Status {
			hasCondition = true
			break
		}
	}
	if !hasCondition {
		t.Error("Expected InCustomerCluster condition to be true")
	}
}

func TestRunner_StartRun_RMA(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, err := s.CreateRunIfNotExists("req-1", machine.MachineId, "RMA", "")
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	err = runner.StartRun(context.Background(), run.RunId)
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && (r.Phase == pb.Run_SUCCEEDED || r.Phase == pb.Run_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_SUCCEEDED {
		t.Errorf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify machine phase is RMA
	finalMachine, _ := s.GetMachine(machine.MachineId)
	if finalMachine.Status.Phase != pb.MachineStatus_RMA {
		t.Errorf("Expected machine phase RMA, got %v", finalMachine.Status.Phase)
	}
}

func TestRunner_CancelRun(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config for cancel test
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 2 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	runner.StartRun(context.Background(), run.RunId)

	// Wait a bit then cancel
	time.Sleep(100 * time.Millisecond)
	err := runner.CancelRun(run.RunId)
	if err != nil {
		t.Fatalf("CancelRun failed: %v", err)
	}

	// Wait for cancellation
	time.Sleep(200 * time.Millisecond)

	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_CANCELED {
		t.Errorf("Expected CANCELED, got %v", finalRun.Phase)
	}
}

func TestRunner_StepRetries(t *testing.T) {
	s := store.New()
	cfg := fake.DefaultConfig()
	cfg.NetbootDuration = 10 * time.Millisecond
	cfg.RebootDuration = 10 * time.Millisecond
	cfg.FailReboot = true // Force failure

	p := fake.New(cfg, nil)
	pl := plans.NewRegistry()
	runner := NewRunner(s, p, pl)
	runner.baseRetryWait = 10 * time.Millisecond

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	runner.StartRun(context.Background(), run.RunId)

	// Wait for failure
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && r.Phase == pb.Run_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_FAILED {
		t.Errorf("Expected FAILED, got %v", finalRun.Phase)
	}

	// Verify machine has NeedsIntervention
	finalMachine, _ := s.GetMachine(machine.MachineId)
	hasIntervention := false
	for _, c := range finalMachine.Status.Conditions {
		if c.Type == lifecycle.ConditionNeedsIntervention && c.Status {
			hasIntervention = true
			break
		}
	}
	if !hasIntervention {
		t.Error("Expected NeedsIntervention condition on failure")
	}
}

func TestRunner_LogStreaming(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	var logCount int32
	runner.SubscribeLogs(run.RunId, func(chunk *pb.LogChunk) {
		atomic.AddInt32(&logCount, 1)
	})

	runner.StartRun(context.Background(), run.RunId)

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && r.Phase == pb.Run_SUCCEEDED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if atomic.LoadInt32(&logCount) == 0 {
		t.Error("Expected log chunks to be streamed")
	}
}

func TestRunner_EventStreaming(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	var events []*pb.RunEvent
	var mu sync.Mutex
	unsubscribe := runner.SubscribeEvents(func(e *pb.RunEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	runner.StartRun(context.Background(), run.RunId)

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && r.Phase == pb.Run_SUCCEEDED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	eventCount := len(events)
	mu.Unlock()

	if eventCount < 3 {
		t.Errorf("Expected at least 3 events (start, step, success), got %d", eventCount)
	}

	// Test unsubscribe
	unsubscribe()

	// Create another run and verify no more events
	run2, _, _ := s.CreateRunIfNotExists("req-2", machine.MachineId, "REBOOT", "")
	runner.StartRun(context.Background(), run2.RunId)

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	newEventCount := len(events)
	mu.Unlock()

	if newEventCount > eventCount+5 {
		t.Error("Events still being received after unsubscribe")
	}
}

func TestRunner_ConcurrentRuns(t *testing.T) {
	s := store.New()
	cfg := fake.DefaultConfig()
	// Use slightly slower config so we can catch active runs
	cfg.NetbootDuration = 100 * time.Millisecond
	cfg.RebootDuration = 200 * time.Millisecond
	cfg.RepaveDuration = 100 * time.Millisecond
	cfg.MintJoinDuration = 50 * time.Millisecond
	cfg.JoinNodeDuration = 100 * time.Millisecond
	cfg.VerifyDuration = 100 * time.Millisecond
	cfg.RMADuration = 100 * time.Millisecond

	p := fake.New(cfg, nil)
	pl := plans.NewRegistry()
	runner := NewRunner(s, p, pl)

	// Create multiple machines
	var machines []*pb.Machine
	for i := 0; i < 3; i++ {
		m, _ := s.UpsertMachine(&pb.Machine{
			Spec: &pb.MachineSpec{
				SshEndpoint: "10.0.0.1:22",
			},
		})
		machines = append(machines, m)
	}

	// Start runs on all machines concurrently
	var runs []*pb.Run
	for i, m := range machines {
		run, _, _ := s.CreateRunIfNotExists("req-"+string(rune('0'+i)), m.MachineId, "REBOOT", "")
		runs = append(runs, run)
	}

	var wg sync.WaitGroup
	for _, run := range runs {
		wg.Add(1)
		go func(r *pb.Run) {
			defer wg.Done()
			runner.StartRun(context.Background(), r.RunId)
		}(run)
	}

	// Wait for all to start - use polling instead of fixed sleep
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runner.ActiveRunCount() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	activeCount := runner.ActiveRunCount()
	if activeCount < 1 {
		t.Errorf("Expected at least 1 active run, got %d", activeCount)
	}

	// Wait for all to complete
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		allDone := true
		for _, run := range runs {
			r, _ := s.GetRun(run.RunId)
			if r.Phase != pb.Run_SUCCEEDED && r.Phase != pb.Run_FAILED {
				allDone = false
				break
			}
		}
		if allDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify all succeeded
	for _, run := range runs {
		r, _ := s.GetRun(run.RunId)
		if r.Phase != pb.Run_SUCCEEDED {
			t.Errorf("Run %s: expected SUCCEEDED, got %v", run.RunId, r.Phase)
		}
	}
}

func TestRunner_RunNotFound(t *testing.T) {
	runner, _ := setupRunner(t)

	err := runner.StartRun(context.Background(), "nonexistent")
	if err == nil {
		t.Error("Expected error for nonexistent run")
	}
}

func TestRunner_RunNotPending(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	// Complete the run first
	s.CompleteRun(run.RunId, pb.Run_SUCCEEDED)

	// StartRun on a non-pending run should be idempotent success (no error)
	err := runner.StartRun(context.Background(), run.RunId)
	if err != nil {
		t.Errorf("Expected idempotent success for completed run, got error: %v", err)
	}
}

func TestRunner_Shutdown(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 5 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	runner.StartRun(context.Background(), run.RunId)

	// Wait for it to start
	time.Sleep(100 * time.Millisecond)

	// Shutdown
	runner.Shutdown()

	// Wait for cancellation
	time.Sleep(200 * time.Millisecond)

	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_CANCELED {
		t.Errorf("Expected CANCELED after shutdown, got %v", finalRun.Phase)
	}
}

func TestRunner_IsRunning(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 1 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	if runner.IsRunning(run.RunId) {
		t.Error("Run should not be running before start")
	}

	runner.StartRun(context.Background(), run.RunId)
	time.Sleep(100 * time.Millisecond)

	if !runner.IsRunning(run.RunId) {
		t.Error("Run should be running after start")
	}

	// Wait for completion
	time.Sleep(2 * time.Second)

	if runner.IsRunning(run.RunId) {
		t.Error("Run should not be running after completion")
	}
}

func TestRunner_CancelRun_SetsMachineState(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config so we have time to cancel
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 2 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	// Start run
	runner.StartRun(context.Background(), run.RunId)
	time.Sleep(100 * time.Millisecond)

	// Cancel the run
	err := runner.CancelRun(run.RunId)
	if err != nil {
		t.Fatalf("CancelRun failed: %v", err)
	}

	// Wait for cancellation to complete
	time.Sleep(200 * time.Millisecond)

	// Verify run is canceled
	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_CANCELED {
		t.Errorf("Expected CANCELED, got %v", finalRun.Phase)
	}

	// Verify machine state was updated
	finalMachine, _ := s.GetMachine(machine.MachineId)

	// Phase should be MAINTENANCE
	if finalMachine.Status.Phase != pb.MachineStatus_MAINTENANCE {
		t.Errorf("Expected machine phase MAINTENANCE, got %v", finalMachine.Status.Phase)
	}

	// ActiveRunId should be cleared
	if finalMachine.Status.ActiveRunId != "" {
		t.Errorf("Expected empty ActiveRunId, got %v", finalMachine.Status.ActiveRunId)
	}

	// OperationCanceled condition should be set (not NeedsIntervention - cancels are user-initiated)
	var hasCanceled bool
	for _, c := range finalMachine.Status.Conditions {
		if c.Type == lifecycle.ConditionOperationCanceled && c.Status {
			hasCanceled = true
			break
		}
	}
	if !hasCanceled {
		t.Error("Expected OperationCanceled condition to be set")
	}
}

func TestRunner_CancelRun_Idempotent(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config to ensure run is still in progress when we cancel
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 2 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	// Start run
	runner.StartRun(context.Background(), run.RunId)
	time.Sleep(100 * time.Millisecond)

	// First cancel
	err := runner.CancelRun(run.RunId)
	if err != nil {
		t.Fatalf("First CancelRun failed: %v", err)
	}

	// Wait for cancellation
	time.Sleep(100 * time.Millisecond)

	// Second cancel should also succeed (idempotent)
	err = runner.CancelRun(run.RunId)
	if err != nil {
		t.Fatalf("Second CancelRun failed: %v", err)
	}

	// Verify still canceled
	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_CANCELED {
		t.Errorf("Expected CANCELED, got %v", finalRun.Phase)
	}
}

func TestRunner_VerifyRequiredForInCustomerCluster(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)

	// Start a repave-join run (which has a verify step)
	run, _, err := s.CreateRunIfNotExists("req-1", machine.MachineId, "REPAVE", "plan/repave-join")
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	// Start run
	err = runner.StartRun(context.Background(), run.RunId)
	if err != nil {
		t.Fatalf("StartRun failed: %v", err)
	}

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && (r.Phase == pb.Run_SUCCEEDED || r.Phase == pb.Run_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded
	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_SUCCEEDED {
		t.Fatalf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify machine has InCustomerCluster condition
	finalMachine, _ := s.GetMachine(machine.MachineId)
	var hasInCluster bool
	for _, c := range finalMachine.Status.Conditions {
		if c.Type == lifecycle.ConditionInCustomerCluster && c.Status {
			hasInCluster = true
			break
		}
	}
	if !hasInCluster {
		t.Error("Expected InCustomerCluster condition to be set after repave-join with verify")
	}

	// Verify machine is IN_SERVICE
	if finalMachine.Status.Phase != pb.MachineStatus_IN_SERVICE {
		t.Errorf("Expected IN_SERVICE, got %v", finalMachine.Status.Phase)
	}
}

func TestRunner_StartRun_DuplicatePrevention(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config so run takes time to complete
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 500 * time.Millisecond
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateRunIfNotExists("req-1", machine.MachineId, "REBOOT", "")

	// Call StartRun twice quickly - only one should actually execute
	err1 := runner.StartRun(context.Background(), run.RunId)
	err2 := runner.StartRun(context.Background(), run.RunId)

	// Both should succeed (idempotent)
	if err1 != nil {
		t.Errorf("First StartRun failed: %v", err1)
	}
	if err2 != nil {
		t.Errorf("Second StartRun failed: %v", err2)
	}

	// Wait for run to complete
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetRun(run.RunId)
		if ok && (r.Phase == pb.Run_SUCCEEDED || r.Phase == pb.Run_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded (only executed once)
	finalRun, _ := s.GetRun(run.RunId)
	if finalRun.Phase != pb.Run_SUCCEEDED {
		t.Errorf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify only one step execution (reboot plan has one step)
	if len(finalRun.Steps) != 1 {
		t.Errorf("Expected 1 step, got %d", len(finalRun.Steps))
	}
}
