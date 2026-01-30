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

func TestRunner_StartOperation_Success(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, err := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	// Track events
	var events []*pb.OperationEvent
	var mu sync.Mutex
	runner.SubscribeEvents(func(e *pb.OperationEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	// Start run
	err = runner.StartOperation(context.Background(), run.OperationId)
	if err != nil {
		t.Fatalf("StartOperation failed: %v", err)
	}

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && (r.Phase == pb.Operation_SUCCEEDED || r.Phase == pb.Operation_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded
	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_SUCCEEDED {
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

func TestRunner_StartOperation_Reimage(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	// First enter maintenance (required for reimage)
	enterMaintenanceOp, _, err := s.CreateOperationIfNotExists("req-0", machine.MachineId, pb.Operation_ENTER_MAINTENANCE, nil)
	if err != nil {
		t.Fatalf("failed to create enter-maintenance op: %v", err)
	}
	if err := runner.StartOperation(context.Background(), enterMaintenanceOp.OperationId); err != nil {
		t.Fatalf("StartOperation for enter-maintenance failed: %v", err)
	}
	// Wait for maintenance
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, ok := s.GetOperation(enterMaintenanceOp.OperationId)
		if ok && lifecycle.IsTerminalOperationPhase(op.Phase) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Now start reimage
	run, _, err := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("failed to create reimage op: %v", err)
	}

	err = runner.StartOperation(context.Background(), run.OperationId)
	if err != nil {
		t.Fatalf("StartOperation failed: %v", err)
	}

	// Wait for completion
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && (r.Phase == pb.Operation_SUCCEEDED || r.Phase == pb.Operation_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded
	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_SUCCEEDED {
		t.Errorf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify machine is still in MAINTENANCE after reimage
	finalMachine, _ := s.GetMachine(machine.MachineId)
	if finalMachine.Status.Phase != pb.MachineStatus_MAINTENANCE {
		t.Errorf("Expected machine phase MAINTENANCE, got %v", finalMachine.Status.Phase)
	}

	// Verify Provisioned condition
	hasCondition := false
	for _, c := range finalMachine.Status.Conditions {
		if c.Type == lifecycle.ConditionProvisioned && c.Status {
			hasCondition = true
			break
		}
	}
	if !hasCondition {
		t.Error("Expected Provisioned condition to be true")
	}
}

func TestRunner_StartOperation_EnterMaintenance(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, err := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_ENTER_MAINTENANCE, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	err = runner.StartOperation(context.Background(), run.OperationId)
	if err != nil {
		t.Fatalf("StartOperation failed: %v", err)
	}

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && (r.Phase == pb.Operation_SUCCEEDED || r.Phase == pb.Operation_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_SUCCEEDED {
		t.Errorf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify machine phase is MAINTENANCE
	finalMachine, _ := s.GetMachine(machine.MachineId)
	if finalMachine.Status.Phase != pb.MachineStatus_MAINTENANCE {
		t.Errorf("Expected machine phase MAINTENANCE, got %v", finalMachine.Status.Phase)
	}
}

func TestRunner_CancelOperation(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config for cancel test
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 2 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	runner.StartOperation(context.Background(), run.OperationId)

	// Wait a bit then cancel
	time.Sleep(100 * time.Millisecond)
	err := runner.CancelOperation(run.OperationId)
	if err != nil {
		t.Fatalf("CancelOperation failed: %v", err)
	}

	// Wait for cancellation
	time.Sleep(200 * time.Millisecond)

	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_CANCELED {
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
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	runner.StartOperation(context.Background(), run.OperationId)

	// Wait for failure
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && r.Phase == pb.Operation_FAILED {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_FAILED {
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
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	var logCount int32
	runner.SubscribeLogs(run.OperationId, func(chunk *pb.LogChunk) {
		atomic.AddInt32(&logCount, 1)
	})

	runner.StartOperation(context.Background(), run.OperationId)

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && r.Phase == pb.Operation_SUCCEEDED {
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
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	var events []*pb.OperationEvent
	var mu sync.Mutex
	unsubscribe := runner.SubscribeEvents(func(e *pb.OperationEvent) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	})

	runner.StartOperation(context.Background(), run.OperationId)

	// Wait for completion
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && r.Phase == pb.Operation_SUCCEEDED {
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
	run2, _, _ := s.CreateOperationIfNotExists("req-2", machine.MachineId, pb.Operation_REBOOT, nil)
	runner.StartOperation(context.Background(), run2.OperationId)

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
	var runs []*pb.Operation
	for i, m := range machines {
		run, _, _ := s.CreateOperationIfNotExists("req-"+string(rune('0'+i)), m.MachineId, pb.Operation_REBOOT, nil)
		runs = append(runs, run)
	}

	var wg sync.WaitGroup
	for _, run := range runs {
		wg.Add(1)
		go func(r *pb.Operation) {
			defer wg.Done()
			runner.StartOperation(context.Background(), r.OperationId)
		}(run)
	}

	// Wait for all to start - use polling instead of fixed sleep
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runner.ActiveOperationCount() >= 3 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	activeCount := runner.ActiveOperationCount()
	if activeCount < 1 {
		t.Errorf("Expected at least 1 active run, got %d", activeCount)
	}

	// Wait for all to complete
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		allDone := true
		for _, run := range runs {
			r, _ := s.GetOperation(run.OperationId)
			if r.Phase != pb.Operation_SUCCEEDED && r.Phase != pb.Operation_FAILED {
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
		r, _ := s.GetOperation(run.OperationId)
		if r.Phase != pb.Operation_SUCCEEDED {
			t.Errorf("Run %s: expected SUCCEEDED, got %v", run.OperationId, r.Phase)
		}
	}
}

func TestRunner_RunNotFound(t *testing.T) {
	runner, _ := setupRunner(t)

	err := runner.StartOperation(context.Background(), "nonexistent")
	if err == nil {
		t.Error("Expected error for nonexistent run")
	}
}

func TestRunner_RunNotPending(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	// Complete the run first
	s.CompleteOperation(run.OperationId, pb.Operation_SUCCEEDED)

	// StartOperation on a non-pending run should be idempotent success (no error)
	err := runner.StartOperation(context.Background(), run.OperationId)
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
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	runner.StartOperation(context.Background(), run.OperationId)

	// Wait for it to start
	time.Sleep(100 * time.Millisecond)

	// Shutdown
	runner.Shutdown()

	// Wait for cancellation
	time.Sleep(200 * time.Millisecond)

	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_CANCELED {
		t.Errorf("Expected CANCELED after shutdown, got %v", finalRun.Phase)
	}
}

func TestRunner_IsOperationRunning(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 1 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	if runner.IsRunning(run.OperationId) {
		t.Error("Run should not be running before start")
	}

	runner.StartOperation(context.Background(), run.OperationId)
	time.Sleep(100 * time.Millisecond)

	if !runner.IsRunning(run.OperationId) {
		t.Error("Run should be running after start")
	}

	// Wait for completion
	time.Sleep(2 * time.Second)

	if runner.IsRunning(run.OperationId) {
		t.Error("Run should not be running after completion")
	}
}

func TestRunner_CancelOperation_SetsMachineState(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config so we have time to cancel
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 2 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	// Start run
	runner.StartOperation(context.Background(), run.OperationId)
	time.Sleep(100 * time.Millisecond)

	// Cancel the run
	err := runner.CancelOperation(run.OperationId)
	if err != nil {
		t.Fatalf("CancelOperation failed: %v", err)
	}

	// Wait for cancellation to complete
	time.Sleep(200 * time.Millisecond)

	// Verify run is canceled
	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_CANCELED {
		t.Errorf("Expected CANCELED, got %v", finalRun.Phase)
	}

	// Verify machine state was updated
	finalMachine, _ := s.GetMachine(machine.MachineId)

	// Phase should NOT be changed by cancellation - phase is imperative intent
	// Only EnterMaintenance/ExitMaintenance should change phase
	// The original phase (FACTORY_READY) should remain unchanged
	if finalMachine.Status.Phase != pb.MachineStatus_FACTORY_READY {
		t.Errorf("Expected machine phase FACTORY_READY (unchanged), got %v", finalMachine.Status.Phase)
	}

	// ActiveOperationId should be cleared
	if finalMachine.Status.ActiveOperationId != "" {
		t.Errorf("Expected empty ActiveOperationId, got %v", finalMachine.Status.ActiveOperationId)
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

func TestRunner_CancelOperation_Idempotent(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config to ensure run is still in progress when we cancel
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 2 * time.Second
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	// Start run
	runner.StartOperation(context.Background(), run.OperationId)
	time.Sleep(100 * time.Millisecond)

	// First cancel
	err := runner.CancelOperation(run.OperationId)
	if err != nil {
		t.Fatalf("First CancelOperation failed: %v", err)
	}

	// Wait for cancellation
	time.Sleep(100 * time.Millisecond)

	// Second cancel should also succeed (idempotent)
	err = runner.CancelOperation(run.OperationId)
	if err != nil {
		t.Fatalf("Second CancelOperation failed: %v", err)
	}

	// Verify still canceled
	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_CANCELED {
		t.Errorf("Expected CANCELED, got %v", finalRun.Phase)
	}
}

func TestRunner_VerifyRequiredForProvisioned(t *testing.T) {
	runner, s := setupRunner(t)

	machine := createTestMachine(t, s)

	// First enter maintenance (required for reimage)
	enterOp, _, err := s.CreateOperationIfNotExists("req-0", machine.MachineId, pb.Operation_ENTER_MAINTENANCE, nil)
	if err != nil {
		t.Fatalf("failed to create enter-maintenance op: %v", err)
	}
	if err := runner.StartOperation(context.Background(), enterOp.OperationId); err != nil {
		t.Fatalf("StartOperation for enter-maintenance failed: %v", err)
	}
	// Wait for maintenance
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, ok := s.GetOperation(enterOp.OperationId)
		if ok && lifecycle.IsTerminalOperationPhase(op.Phase) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Start a reimage operation (which has a verify step via repave-join plan)
	run, _, err := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("failed to create reimage op: %v", err)
	}

	// Start run
	err = runner.StartOperation(context.Background(), run.OperationId)
	if err != nil {
		t.Fatalf("StartOperation failed: %v", err)
	}

	// Wait for completion
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && (r.Phase == pb.Operation_SUCCEEDED || r.Phase == pb.Operation_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded
	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_SUCCEEDED {
		t.Fatalf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify machine has Provisioned condition
	finalMachine, _ := s.GetMachine(machine.MachineId)
	var hasProvisioned bool
	for _, c := range finalMachine.Status.Conditions {
		if c.Type == lifecycle.ConditionProvisioned && c.Status {
			hasProvisioned = true
			break
		}
	}
	if !hasProvisioned {
		t.Error("Expected Provisioned condition to be set after reimage")
	}

	// Verify machine stays in MAINTENANCE after reimage
	if finalMachine.Status.Phase != pb.MachineStatus_MAINTENANCE {
		t.Errorf("Expected MAINTENANCE, got %v", finalMachine.Status.Phase)
	}
}

func TestRunner_StartOperation_DuplicatePrevention(t *testing.T) {
	runner, s := setupRunner(t)

	// Use slower config so run takes time to complete
	cfg := fake.DefaultConfig()
	cfg.RebootDuration = 500 * time.Millisecond
	p := fake.New(cfg, nil)
	runner.provider = p

	machine := createTestMachine(t, s)
	run, _, _ := s.CreateOperationIfNotExists("req-1", machine.MachineId, pb.Operation_REBOOT, nil)

	// Call StartOperation twice quickly - only one should actually execute
	err1 := runner.StartOperation(context.Background(), run.OperationId)
	err2 := runner.StartOperation(context.Background(), run.OperationId)

	// Both should succeed (idempotent)
	if err1 != nil {
		t.Errorf("First StartOperation failed: %v", err1)
	}
	if err2 != nil {
		t.Errorf("Second StartOperation failed: %v", err2)
	}

	// Wait for run to complete
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, ok := s.GetOperation(run.OperationId)
		if ok && (r.Phase == pb.Operation_SUCCEEDED || r.Phase == pb.Operation_FAILED) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Verify run succeeded (only executed once)
	finalRun, _ := s.GetOperation(run.OperationId)
	if finalRun.Phase != pb.Operation_SUCCEEDED {
		t.Errorf("Expected SUCCEEDED, got %v", finalRun.Phase)
	}

	// Verify workflow has step execution (workflow is internal, not on public Operation)
	wf, ok := s.GetWorkflow(run.OperationId)
	if !ok {
		t.Fatal("Workflow not found")
	}
	if len(wf.Steps) != 1 {
		t.Errorf("Expected 1 step in workflow, got %d", len(wf.Steps))
	}
}
