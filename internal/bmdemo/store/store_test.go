package store

import (
	"sync"
	"testing"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/workflow"
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

func TestIdempotentCreateOperation(t *testing.T) {
	s := New()

	// Create a machine first
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// First create with request_id
	op1, created, err := s.CreateOperationIfNotExists("req-123", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created")
	}
	if op1.RequestId != "req-123" {
		t.Errorf("Expected request_id req-123, got %s", op1.RequestId)
	}

	// Complete the first operation so we can test idempotency cleanly
	s.CompleteOperation(op1.OperationId, pb.Operation_SUCCEEDED)

	// Second create with same request_id should return same operation
	op2, created, err := s.CreateOperationIfNotExists("req-123", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists failed: %v", err)
	}
	if created {
		t.Error("Expected operation NOT to be created (idempotent)")
	}
	if op2.OperationId != op1.OperationId {
		t.Errorf("Expected same operation ID %s, got %s", op1.OperationId, op2.OperationId)
	}

	// Different request_id should create new operation
	op3, created, err := s.CreateOperationIfNotExists("req-456", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created with different request_id")
	}
	if op3.OperationId == op1.OperationId {
		t.Error("Expected different operation ID for different request_id")
	}
}

func TestNoTwoActiveOperationsPerMachine(t *testing.T) {
	s := New()

	// Create a machine
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Start first operation
	op1, created, err := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created")
	}

	// Try to start second operation - should fail
	_, _, err = s.CreateOperationIfNotExists("req-2", "m-1", pb.Operation_REIMAGE, nil)
	if err == nil {
		t.Error("Expected error when creating second operation for same machine")
	}

	// Complete first operation
	err = s.CompleteOperation(op1.OperationId, pb.Operation_SUCCEEDED)
	if err != nil {
		t.Fatalf("CompleteOperation failed: %v", err)
	}

	// Now we should be able to start a new operation
	op3, created, err := s.CreateOperationIfNotExists("req-3", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists failed after completing first operation: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created after first operation completed")
	}
	if op3.OperationId == op1.OperationId {
		t.Error("Expected different operation ID")
	}
}

func TestConcurrentCreateOperations(t *testing.T) {
	s := New()

	// Create machine
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Try to create 10 operations concurrently with different request_ids
	var wg sync.WaitGroup
	results := make(chan error, 10)
	successCount := make(chan bool, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, created, err := s.CreateOperationIfNotExists("", "m-1", pb.Operation_REIMAGE, nil)
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

	// Only 1 operation should succeed, the rest should error
	if successes != 1 {
		t.Errorf("Expected exactly 1 successful operation creation, got %d", successes)
	}
	if errors != 9 {
		t.Errorf("Expected 9 errors, got %d", errors)
	}
}

func TestConcurrentIdempotentRequests(t *testing.T) {
	s := New()

	// Create machine
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Try to create operations concurrently with SAME request_id
	var wg sync.WaitGroup
	opIDs := make(chan string, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, _, err := s.CreateOperationIfNotExists("idempotent-req", "m-1", pb.Operation_REIMAGE, nil)
			if err != nil {
				// May get "already has active operation" error, which is fine
				return
			}
			opIDs <- op.OperationId
		}()
	}

	wg.Wait()
	close(opIDs)

	// All successful requests should return the same operation ID
	var firstID string
	for id := range opIDs {
		if firstID == "" {
			firstID = id
		} else if id != firstID {
			t.Errorf("Expected all operations to have same ID %s, got %s", firstID, id)
		}
	}
}

func TestCancelOperation(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	op, _, _ := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)

	// Cancel the operation
	cancelled, err := s.CancelOperation(op.OperationId)
	if err != nil {
		t.Fatalf("CancelOperation failed: %v", err)
	}
	if cancelled.Phase != pb.Operation_CANCELED {
		t.Errorf("Expected phase CANCELED, got %v", cancelled.Phase)
	}

	// Machine should no longer have active operation
	machine, _ := s.GetMachine("m-1")
	if machine.Status.ActiveOperationId != "" {
		t.Errorf("Expected empty active_operation_id, got %s", machine.Status.ActiveOperationId)
	}

	// Should be able to start new operation now
	_, created, err := s.CreateOperationIfNotExists("req-2", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists after cancel failed: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created after cancel")
	}
}

func TestUpdateWorkflowStep(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	op, _, _ := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)

	// Add step using internal workflow types
	err := s.UpdateWorkflowStep(op.OperationId, &workflow.StepStatus{
		Name:  "step-1",
		State: workflow.StepStateRunning,
	})
	if err != nil {
		t.Fatalf("UpdateWorkflowStep failed: %v", err)
	}

	// Check step was added to workflow (not to public Operation)
	wf, ok := s.GetWorkflow(op.OperationId)
	if !ok {
		t.Fatal("Workflow not found")
	}
	if len(wf.Steps) != 1 {
		t.Errorf("Expected 1 step in workflow, got %d", len(wf.Steps))
	}

	// current_stage should NOT be set to step name (step names are internal)
	o, _ := s.GetOperation(op.OperationId)
	if o.CurrentStage != "" {
		t.Errorf("Expected current stage to be empty (step names are internal), got %s", o.CurrentStage)
	}

	// Update same step
	err = s.UpdateWorkflowStep(op.OperationId, &workflow.StepStatus{
		Name:  "step-1",
		State: workflow.StepStateSucceeded,
	})
	if err != nil {
		t.Fatalf("UpdateWorkflowStep update failed: %v", err)
	}

	wf, _ = s.GetWorkflow(op.OperationId)
	if len(wf.Steps) != 1 {
		t.Errorf("Expected still 1 step, got %d", len(wf.Steps))
	}
	if wf.Steps[0].State != workflow.StepStateSucceeded {
		t.Errorf("Expected state SUCCEEDED, got %v", wf.Steps[0].State)
	}
}

// TestUpdateWorkflowStep_ClonesInput verifies that UpdateWorkflowStep clones the input
// step, so mutations to the original do not affect the stored copy.
func TestUpdateWorkflowStep_ClonesInput(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})
	op, _, _ := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)

	// Create a step and store it
	originalStep := &workflow.StepStatus{
		Name:       "test-step",
		State:      workflow.StepStateRunning,
		Message:    "initial message",
		RetryCount: 0,
	}
	err := s.UpdateWorkflowStep(op.OperationId, originalStep)
	if err != nil {
		t.Fatalf("UpdateWorkflowStep failed: %v", err)
	}

	// Mutate the original step after storing
	originalStep.State = workflow.StepStateFailed
	originalStep.Message = "mutated message"
	originalStep.RetryCount = 99

	// Fetch the workflow and verify the stored step was NOT affected
	wf, ok := s.GetWorkflow(op.OperationId)
	if !ok {
		t.Fatal("Workflow not found")
	}
	if len(wf.Steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(wf.Steps))
	}
	storedStep := wf.Steps[0]
	if storedStep.State != workflow.StepStateRunning {
		t.Errorf("Expected stored step state RUNNING, got %v (mutation leaked)", storedStep.State)
	}
	if storedStep.Message != "initial message" {
		t.Errorf("Expected stored message 'initial message', got %q (mutation leaked)", storedStep.Message)
	}
	if storedStep.RetryCount != 0 {
		t.Errorf("Expected retry count 0, got %d (mutation leaked)", storedStep.RetryCount)
	}
}

func TestMachineStatusUpdatedOnOperationCompletion(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	op, _, _ := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)

	// Machine should still be FACTORY_READY (store no longer sets phase)
	// The executor is responsible for phase transitions
	machine, _ := s.GetMachine("m-1")
	if machine.Status.Phase != pb.MachineStatus_FACTORY_READY {
		t.Errorf("Expected FACTORY_READY, got %v", machine.Status.Phase)
	}
	// But ActiveOperationId should be set
	if machine.Status.ActiveOperationId != op.OperationId {
		t.Errorf("Expected ActiveOperationId %s, got %s", op.OperationId, machine.Status.ActiveOperationId)
	}

	// Complete operation successfully
	s.CompleteOperation(op.OperationId, pb.Operation_SUCCEEDED)

	// Machine phase should NOT be changed by CompleteOperation - that's the executor's job
	// CompleteOperation should only clear active_operation_id
	machine, _ = s.GetMachine("m-1")
	if machine.Status.ActiveOperationId != "" {
		t.Errorf("Expected empty ActiveOperationId, got %v", machine.Status.ActiveOperationId)
	}
	// Phase should still be FACTORY_READY (executor updates it, not store)
	if machine.Status.Phase != pb.MachineStatus_FACTORY_READY {
		t.Errorf("Expected phase to remain FACTORY_READY, got %v", machine.Status.Phase)
	}
}

func TestIdempotencyForInFlightOperation(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	// Create an operation (which becomes the active operation)
	op1, created, err := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created")
	}

	// Verify machine has active operation
	machine, _ := s.GetMachine("m-1")
	if machine.Status.ActiveOperationId != op1.OperationId {
		t.Errorf("Expected active operation %s, got %s", op1.OperationId, machine.Status.ActiveOperationId)
	}

	// Same request_id should return the existing operation (even though machine has active operation)
	op2, created, err := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists for same request_id failed: %v", err)
	}
	if created {
		t.Error("Expected operation NOT to be created (idempotent)")
	}
	if op2.OperationId != op1.OperationId {
		t.Errorf("Expected same operation ID %s, got %s", op1.OperationId, op2.OperationId)
	}

	// Different request_id should fail because machine has active operation
	_, _, err = s.CreateOperationIfNotExists("req-2", "m-1", pb.Operation_REIMAGE, nil)
	if err == nil {
		t.Error("Expected error when creating operation with different request_id while machine has active operation")
	}
}

func TestIdempotencyScopedToMachine(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})
	s.UpsertMachine(&pb.Machine{MachineId: "m-2"})

	// Create operation on m-1 with request_id "req-1"
	op1, created, err := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists failed: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created for m-1")
	}

	// Complete op1 so m-1 doesn't have an active operation
	s.CompleteOperation(op1.OperationId, pb.Operation_SUCCEEDED)

	// Same request_id on DIFFERENT machine should create a NEW operation
	op2, created, err := s.CreateOperationIfNotExists("req-1", "m-2", pb.Operation_REIMAGE, nil)
	if err != nil {
		t.Fatalf("CreateOperationIfNotExists for m-2 failed: %v", err)
	}
	if !created {
		t.Error("Expected operation to be created for m-2 (different machine, same request_id)")
	}
	if op2.OperationId == op1.OperationId {
		t.Error("Expected different operation ID for different machine")
	}
	if op2.MachineId != "m-2" {
		t.Errorf("Expected machine_id m-2, got %s", op2.MachineId)
	}
}

func TestCancelOperationIdempotent(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	op, _, _ := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)

	// First cancel should succeed
	canceledOp, err := s.CancelOperation(op.OperationId)
	if err != nil {
		t.Fatalf("First CancelOperation failed: %v", err)
	}
	if canceledOp.Phase != pb.Operation_CANCELED {
		t.Errorf("Expected CANCELED phase, got %v", canceledOp.Phase)
	}

	// Second cancel should also succeed (idempotent)
	canceledOp2, err := s.CancelOperation(op.OperationId)
	if err != nil {
		t.Fatalf("Second CancelOperation failed: %v", err)
	}
	if canceledOp2.Phase != pb.Operation_CANCELED {
		t.Errorf("Expected CANCELED phase on second cancel, got %v", canceledOp2.Phase)
	}

	// Verify machine state was cleared
	machine, _ := s.GetMachine("m-1")
	if machine.Status.ActiveOperationId != "" {
		t.Errorf("Expected empty ActiveOperationId after cancel, got %v", machine.Status.ActiveOperationId)
	}
}

func TestCancelCompletedOperationFails(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	op, _, _ := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)

	// Complete the operation
	s.CompleteOperation(op.OperationId, pb.Operation_SUCCEEDED)

	// Cancel should fail because operation is already completed
	_, err := s.CancelOperation(op.OperationId)
	if err == nil {
		t.Error("Expected error when canceling completed operation")
	}
}

func TestTryTransitionOperationPhase(t *testing.T) {
	s := New()
	s.UpsertMachine(&pb.Machine{MachineId: "m-1"})

	op, _, _ := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)

	// Operation starts as PENDING
	if op.Phase != pb.Operation_PENDING {
		t.Fatalf("Expected PENDING, got %v", op.Phase)
	}

	// Transition PENDING -> RUNNING should succeed
	ok, err := s.TryTransitionOperationPhase(op.OperationId, pb.Operation_PENDING, pb.Operation_RUNNING)
	if err != nil {
		t.Fatalf("TryTransitionOperationPhase failed: %v", err)
	}
	if !ok {
		t.Error("Expected transition to succeed")
	}

	// Verify the operation is now RUNNING with StartedAt set
	updatedOp, _ := s.GetOperation(op.OperationId)
	if updatedOp.Phase != pb.Operation_RUNNING {
		t.Errorf("Expected RUNNING, got %v", updatedOp.Phase)
	}
	if updatedOp.StartedAt == nil {
		t.Error("Expected StartedAt to be set")
	}

	// Second transition attempt PENDING -> RUNNING should fail (already RUNNING)
	ok, err = s.TryTransitionOperationPhase(op.OperationId, pb.Operation_PENDING, pb.Operation_RUNNING)
	if err != nil {
		t.Fatalf("TryTransitionOperationPhase should not error: %v", err)
	}
	if ok {
		t.Error("Expected transition to fail (operation is no longer PENDING)")
	}

	// Transition for non-existent operation should error
	_, err = s.TryTransitionOperationPhase("nonexistent", pb.Operation_PENDING, pb.Operation_RUNNING)
	if err == nil {
		t.Error("Expected error for non-existent operation")
	}
}

// TestCreateOperationConcurrentUpsert verifies that CreateOperationIfNotExists correctly
// re-fetches the machine under write lock, preventing mutation of stale pointers
// that could be replaced by concurrent UpsertMachine calls.
func TestCreateOperationConcurrentUpsert(t *testing.T) {
	s := New()

	// Create initial machine
	s.UpsertMachine(&pb.Machine{
		MachineId: "m-1",
		Spec:      &pb.MachineSpec{SshEndpoint: "old-endpoint"},
	})

	// Simulate concurrent scenario: between the read lock and write lock in
	// CreateOperationIfNotExists, another goroutine calls UpsertMachine replacing the machine.
	// Without the fix, the stale machine pointer would be mutated.

	// Channel to synchronize the test
	done := make(chan bool)

	// Start CreateOperationIfNotExists in a goroutine
	go func() {
		op, created, err := s.CreateOperationIfNotExists("req-1", "m-1", pb.Operation_REIMAGE, nil)
		if err != nil {
			t.Errorf("CreateOperationIfNotExists failed: %v", err)
			done <- false
			return
		}
		if !created {
			t.Error("Expected operation to be created")
			done <- false
			return
		}
		done <- op != nil
	}()

	// Immediately try to replace the machine (simulating concurrent update)
	// This happens very quickly, so it may or may not race with the above call
	newMachine := &pb.Machine{
		MachineId: "m-1",
		Spec:      &pb.MachineSpec{SshEndpoint: "new-endpoint"},
		Status:    &pb.MachineStatus{Phase: pb.MachineStatus_READY},
	}
	s.UpsertMachine(newMachine)

	// Wait for CreateOperation to complete
	<-done

	// Verify: the machine in the store should have ActiveOperationId set on the
	// correct (current) machine, not a stale copy
	machine, _ := s.GetMachine("m-1")
	if machine.Status.ActiveOperationId == "" {
		t.Error("Expected ActiveOperationId to be set on the machine")
	}
}
