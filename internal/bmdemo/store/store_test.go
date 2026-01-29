package store

import (
	"sync"
	"testing"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

func TestUpsertMachine(t *testing.T) {
	s := New()

	// Test insert with auto-generated ID
	m, err := s.UpsertMachine(&pb.Machine{
		Spec: &pb.MachineSpec{SshEndpoint: "10.0.0.1:22"},
	})
	if err != nil {
		t.Fatalf("UpsertMachine failed: %v", err)
	}
	if m.MachineId == "" {
		t.Error("Expected machine ID to be generated")
	}
	if m.Status.Phase != pb.MachineStatus_FACTORY_READY {
		t.Errorf("Expected phase FACTORY_READY, got %v", m.Status.Phase)
	}

	// Test update
	m.Labels = map[string]string{"role": "worker"}
	m2, err := s.UpsertMachine(m)
	if err != nil {
		t.Fatalf("UpsertMachine update failed: %v", err)
	}
	if m2.Labels["role"] != "worker" {
		t.Error("Labels not updated")
	}
}

func TestGetMachine(t *testing.T) {
	s := New()

	// Not found
	_, ok := s.GetMachine("nonexistent")
	if ok {
		t.Error("Expected machine not found")
	}

	// Create and get
	m, _ := s.UpsertMachine(&pb.Machine{MachineId: "m-1"})
	got, ok := s.GetMachine(m.MachineId)
	if !ok {
		t.Error("Expected machine to be found")
	}
	if got.MachineId != m.MachineId {
		t.Errorf("Expected %s, got %s", m.MachineId, got.MachineId)
	}
}

func TestListMachines(t *testing.T) {
	s := New()

	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})
	s.UpsertMachine(&pb.Machine{MachineId: "m-2"})
	s.UpsertMachine(&pb.Machine{MachineId: "m-3"})

	machines := s.ListMachines()
	if len(machines) != 3 {
		t.Errorf("Expected 3 machines, got %d", len(machines))
	}
}

func TestIdempotentCreateRun(t *testing.T) {
	s := New()

	// Create a machine first
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// First create with request_id
	run1, created, err := s.CreateRunIfNotExists("req-123", "m-1", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected run to be created")
	}
	if run1.RequestId != "req-123" {
		t.Errorf("Expected request_id req-123, got %s", run1.RequestId)
	}

	// Complete the first run so we can test idempotency cleanly
	s.CompleteRun(run1.RunId, pb.Run_SUCCEEDED)

	// Second create with same request_id should return same run
	run2, created, err := s.CreateRunIfNotExists("req-123", "m-1", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists failed: %v", err)
	}
	if created {
		t.Error("Expected run NOT to be created (idempotent)")
	}
	if run2.RunId != run1.RunId {
		t.Errorf("Expected same run ID %s, got %s", run1.RunId, run2.RunId)
	}

	// Different request_id should create new run
	run3, created, err := s.CreateRunIfNotExists("req-456", "m-1", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected run to be created with different request_id")
	}
	if run3.RunId == run1.RunId {
		t.Error("Expected different run ID for different request_id")
	}
}

func TestNoTwoActiveRunsPerMachine(t *testing.T) {
	s := New()

	// Create a machine
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Start first run
	run1, created, err := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected run to be created")
	}

	// Try to start second run - should fail
	_, _, err = s.CreateRunIfNotExists("req-2", "m-1", "REPAVE", "")
	if err == nil {
		t.Error("Expected error when creating second run for same machine")
	}

	// Complete first run
	err = s.CompleteRun(run1.RunId, pb.Run_SUCCEEDED)
	if err != nil {
		t.Fatalf("CompleteRun failed: %v", err)
	}

	// Now we should be able to start a new run
	run3, created, err := s.CreateRunIfNotExists("req-3", "m-1", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists failed after completing first run: %v", err)
	}
	if !created {
		t.Error("Expected run to be created after first run completed")
	}
	if run3.RunId == run1.RunId {
		t.Error("Expected different run ID")
	}
}

func TestConcurrentCreateRuns(t *testing.T) {
	s := New()

	// Create machine
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Try to create 10 runs concurrently with different request_ids
	var wg sync.WaitGroup
	results := make(chan error, 10)
	successCount := make(chan bool, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, created, err := s.CreateRunIfNotExists("", "m-1", "REPAVE", "")
			if err != nil {
				results <- err
				return
			}
			if created {
				successCount <- true
			}
			results <- nil
		}(i)
	}

	wg.Wait()
	close(results)
	close(successCount)

	// Count successes and errors
	successes := 0
	errors := 0
	for range successCount {
		successes++
	}
	for err := range results {
		if err != nil {
			errors++
		}
	}

	// Only 1 run should succeed, the rest should error
	if successes != 1 {
		t.Errorf("Expected exactly 1 successful run creation, got %d", successes)
	}
	if errors != 9 {
		t.Errorf("Expected 9 errors, got %d", errors)
	}
}

func TestConcurrentIdempotentRequests(t *testing.T) {
	s := New()

	// Create machine
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Try to create runs concurrently with SAME request_id
	var wg sync.WaitGroup
	runIDs := make(chan string, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run, _, err := s.CreateRunIfNotExists("idempotent-req", "m-1", "REPAVE", "")
			if err != nil {
				// May get "already has active run" error, which is fine
				return
			}
			runIDs <- run.RunId
		}()
	}

	wg.Wait()
	close(runIDs)

	// All successful requests should return the same run ID
	var firstID string
	for id := range runIDs {
		if firstID == "" {
			firstID = id
		} else if id != firstID {
			t.Errorf("Expected all runs to have same ID %s, got %s", firstID, id)
		}
	}
}

func TestCancelRun(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	run, _, _ := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "")

	// Cancel the run
	cancelled, err := s.CancelRun(run.RunId)
	if err != nil {
		t.Fatalf("CancelRun failed: %v", err)
	}
	if cancelled.Phase != pb.Run_CANCELED {
		t.Errorf("Expected phase CANCELED, got %v", cancelled.Phase)
	}

	// Machine should no longer have active run
	machine, _ := s.GetMachine("m-1")
	if machine.Status.ActiveRunId != "" {
		t.Errorf("Expected empty active_run_id, got %s", machine.Status.ActiveRunId)
	}

	// Should be able to start new run now
	_, created, err := s.CreateRunIfNotExists("req-2", "m-1", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists after cancel failed: %v", err)
	}
	if !created {
		t.Error("Expected run to be created after cancel")
	}
}

func TestUpdateRunStep(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	run, _, _ := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "")

	// Add step
	err := s.UpdateRunStep(run.RunId, &pb.StepStatus{
		Name:  "step-1",
		State: pb.StepStatus_RUNNING,
	})
	if err != nil {
		t.Fatalf("UpdateRunStep failed: %v", err)
	}

	// Check step was added
	r, _ := s.GetRun(run.RunId)
	if len(r.Steps) != 1 {
		t.Errorf("Expected 1 step, got %d", len(r.Steps))
	}
	if r.CurrentStep != "step-1" {
		t.Errorf("Expected current step step-1, got %s", r.CurrentStep)
	}

	// Update same step
	err = s.UpdateRunStep(run.RunId, &pb.StepStatus{
		Name:  "step-1",
		State: pb.StepStatus_SUCCEEDED,
	})
	if err != nil {
		t.Fatalf("UpdateRunStep update failed: %v", err)
	}

	r, _ = s.GetRun(run.RunId)
	if len(r.Steps) != 1 {
		t.Errorf("Expected still 1 step, got %d", len(r.Steps))
	}
	if r.Steps[0].State != pb.StepStatus_SUCCEEDED {
		t.Errorf("Expected state SUCCEEDED, got %v", r.Steps[0].State)
	}
}

func TestMachineStatusUpdatedOnRunCompletion(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	run, _, _ := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "")

	// Machine should be PROVISIONING
	machine, _ := s.GetMachine("m-1")
	if machine.Status.Phase != pb.MachineStatus_PROVISIONING {
		t.Errorf("Expected PROVISIONING, got %v", machine.Status.Phase)
	}

	// Complete run successfully
	s.CompleteRun(run.RunId, pb.Run_SUCCEEDED)

	// Machine phase should NOT be changed by CompleteRun - that's the executor's job
	// CompleteRun should only clear active_run_id
	machine, _ = s.GetMachine("m-1")
	if machine.Status.ActiveRunId != "" {
		t.Errorf("Expected empty ActiveRunId, got %v", machine.Status.ActiveRunId)
	}
	// Phase should still be PROVISIONING (executor updates it, not store)
	if machine.Status.Phase != pb.MachineStatus_PROVISIONING {
		t.Errorf("Expected phase to remain PROVISIONING, got %v", machine.Status.Phase)
	}
}

func TestIdempotencyForInFlightRun(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Create a run (which becomes the active run)
	run1, created, err := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "plan/repave-join")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected run to be created")
	}

	// Verify machine has active run
	machine, _ := s.GetMachine("m-1")
	if machine.Status.ActiveRunId != run1.RunId {
		t.Errorf("Expected active run %s, got %s", run1.RunId, machine.Status.ActiveRunId)
	}

	// Same request_id should return the existing run (even though machine has active run)
	run2, created, err := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "plan/repave-join")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists for same request_id failed: %v", err)
	}
	if created {
		t.Error("Expected run NOT to be created (idempotent)")
	}
	if run2.RunId != run1.RunId {
		t.Errorf("Expected same run ID %s, got %s", run1.RunId, run2.RunId)
	}

	// Different request_id should fail because machine has active run
	_, _, err = s.CreateRunIfNotExists("req-2", "m-1", "REPAVE", "plan/repave-join")
	if err == nil {
		t.Error("Expected error when creating run with different request_id while machine has active run")
	}
}

func TestIdempotencyScopedToMachine(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})
	s.UpsertMachine(&pb.Machine{MachineId: "m-2"})

	// Create run on m-1 with request_id "req-1"
	run1, created, err := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected run to be created for m-1")
	}

	// Complete run1 so m-1 doesn't have an active run
	s.CompleteRun(run1.RunId, pb.Run_SUCCEEDED)

	// Same request_id on DIFFERENT machine should create a NEW run
	run2, created, err := s.CreateRunIfNotExists("req-1", "m-2", "REPAVE", "")
	if err != nil {
		t.Fatalf("CreateRunIfNotExists for m-2 failed: %v", err)
	}
	if !created {
		t.Error("Expected run to be created for m-2 (different machine, same request_id)")
	}
	if run2.RunId == run1.RunId {
		t.Error("Expected different run ID for different machine")
	}
	if run2.MachineId != "m-2" {
		t.Errorf("Expected machine_id m-2, got %s", run2.MachineId)
	}
}

func TestCancelRunIdempotent(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	run, _, _ := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "")

	// First cancel should succeed
	canceledRun, err := s.CancelRun(run.RunId)
	if err != nil {
		t.Fatalf("First CancelRun failed: %v", err)
	}
	if canceledRun.Phase != pb.Run_CANCELED {
		t.Errorf("Expected CANCELED phase, got %v", canceledRun.Phase)
	}

	// Second cancel should also succeed (idempotent)
	canceledRun2, err := s.CancelRun(run.RunId)
	if err != nil {
		t.Fatalf("Second CancelRun failed: %v", err)
	}
	if canceledRun2.Phase != pb.Run_CANCELED {
		t.Errorf("Expected CANCELED phase on second cancel, got %v", canceledRun2.Phase)
	}

	// Verify machine state was cleared
	machine, _ := s.GetMachine("m-1")
	if machine.Status.ActiveRunId != "" {
		t.Errorf("Expected empty ActiveRunId after cancel, got %v", machine.Status.ActiveRunId)
	}
}

func TestCancelCompletedRunFails(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	run, _, _ := s.CreateRunIfNotExists("req-1", "m-1", "REPAVE", "")

	// Complete the run
	s.CompleteRun(run.RunId, pb.Run_SUCCEEDED)

	// Cancel should fail because run is already completed
	_, err := s.CancelRun(run.RunId)
	if err == nil {
		t.Error("Expected error when canceling completed run")
	}
}
