// Package executor provides the operation executor that processes provisioning operations.
package executor

import (
	"context"
	"fmt"
	"log"
	"math"
	"runtime/debug"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/lifecycle"
	"github.com/vpatelsj/stargate/internal/bmdemo/plans"
	"github.com/vpatelsj/stargate/internal/bmdemo/provider"
	"github.com/vpatelsj/stargate/internal/bmdemo/store"
	"github.com/vpatelsj/stargate/internal/bmdemo/workflow"
)

// EventCallback is called on operation state transitions.
type EventCallback func(event *pb.OperationEvent)

// LogCallback is called to stream logs.
type LogCallback func(chunk *pb.LogChunk)

// Runner executes operations asynchronously.
type Runner struct {
	store    *store.Store
	provider provider.Provider
	plans    *plans.Registry

	mu               sync.RWMutex
	eventSubID       uint64                            // counter for unique subscriber IDs
	eventSubs        map[uint64]EventCallback          // subscriberID -> callback
	logSubID         map[string]uint64                 // operationID -> counter for unique log subscriber IDs
	logSubs          map[string]map[uint64]LogCallback // operationID -> subscriberID -> callback
	activeOperations map[string]context.CancelFunc
	baseRetryWait    time.Duration
	maxRetryWait     time.Duration
}

// NewRunner creates a new operation executor.
func NewRunner(s *store.Store, p provider.Provider, pl *plans.Registry) *Runner {
	return &Runner{
		store:            s,
		provider:         p,
		plans:            pl,
		eventSubs:        make(map[uint64]EventCallback),
		logSubID:         make(map[string]uint64),
		logSubs:          make(map[string]map[uint64]LogCallback),
		activeOperations: make(map[string]context.CancelFunc),
		baseRetryWait:    500 * time.Millisecond,
		maxRetryWait:     10 * time.Second,
	}
}

// SetProvider sets the provider (for deferred initialization).
func (r *Runner) SetProvider(p provider.Provider) {
	r.provider = p
}

// EmitLog is called by the provider to stream logs. This is the public API.
func (r *Runner) EmitLog(operationID, stream string, data []byte) {
	r.emitLog(operationID, stream, data)
}

// SubscribeEvents adds an event subscriber. Returns unsubscribe function.
func (r *Runner) SubscribeEvents(cb EventCallback) func() {
	r.mu.Lock()
	r.eventSubID++
	id := r.eventSubID
	r.eventSubs[id] = cb
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.eventSubs, id)
	}
}

// SubscribeLogs adds a log subscriber for a specific operation. Returns unsubscribe function.
func (r *Runner) SubscribeLogs(operationID string, cb LogCallback) func() {
	r.mu.Lock()
	if r.logSubs[operationID] == nil {
		r.logSubs[operationID] = make(map[uint64]LogCallback)
	}
	r.logSubID[operationID]++
	id := r.logSubID[operationID]
	r.logSubs[operationID][id] = cb
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subs, ok := r.logSubs[operationID]; ok {
			delete(subs, id)
			// Clean up empty map
			if len(subs) == 0 {
				delete(r.logSubs, operationID)
				delete(r.logSubID, operationID)
			}
		}
	}
}

// emitEvent sends an event to all subscribers with an immutable snapshot.
func (r *Runner) emitEvent(op *pb.Operation, message string) {
	// Clone the operation to ensure immutability
	snapshot := proto.Clone(op).(*pb.Operation)
	event := &pb.OperationEvent{
		Ts:       timestamppb.Now(),
		Snapshot: snapshot,
		Message:  message,
	}

	r.mu.RLock()
	subs := make([]EventCallback, 0, len(r.eventSubs))
	for _, cb := range r.eventSubs {
		subs = append(subs, cb)
	}
	r.mu.RUnlock()

	for _, cb := range subs {
		cb(event)
	}
}

// emitEventFromStore fetches the latest operation state from store and emits an event.
func (r *Runner) emitEventFromStore(operationID string, message string) {
	op, ok := r.store.GetOperation(operationID)
	if !ok {
		return
	}
	r.emitEvent(op, message)
}

// emitLog sends a log chunk to operation subscribers.
func (r *Runner) emitLog(operationID, stream string, data []byte) {
	chunk := &pb.LogChunk{
		Ts:          timestamppb.Now(),
		OperationId: operationID,
		Stream:      stream,
		Data:        data,
	}

	r.mu.RLock()
	var subs []LogCallback
	if m, ok := r.logSubs[operationID]; ok {
		subs = make([]LogCallback, 0, len(m))
		for _, cb := range m {
			subs = append(subs, cb)
		}
	}
	r.mu.RUnlock()

	for _, cb := range subs {
		cb(chunk)
	}
}

// StartOperation begins executing an operation asynchronously.
// Returns immediately; operation proceeds in background.
// Idempotent: calling StartOperation on an already-running or finished operation returns nil.
// parentCtx is used as the base context for the operation; canceling it will cancel the operation.
func (r *Runner) StartOperation(parentCtx context.Context, operationID string) error {
	// Atomically try to transition from PENDING to RUNNING
	ok, err := r.store.TryTransitionOperationPhase(operationID, pb.Operation_PENDING, pb.Operation_RUNNING)
	if err != nil {
		return err // operation not found
	}
	if !ok {
		// Operation is not PENDING - either already running, succeeded, failed, or canceled.
		// This is idempotent success - someone else started it or it's already done.
		return nil
	}

	// We are the winner - we transitioned PENDING -> RUNNING
	// Create cancellable context derived from parentCtx (so server shutdown cancels operations)
	opCtx, cancel := context.WithCancel(parentCtx)

	r.mu.Lock()
	r.activeOperations[operationID] = cancel
	r.mu.Unlock()

	// Execute asynchronously
	go r.executeOperation(opCtx, operationID)

	return nil
}

// CancelOperation cancels an operation immediately.
// It marks the operation as CANCELED in the store, clears machine state, and stops execution.
// Idempotent: canceling an already-canceled operation returns success.
// NOTE: Does NOT change machine.phase - phase is imperative intent, only
// EnterMaintenance/ExitMaintenance should modify it.
func (r *Runner) CancelOperation(operationID string) error {
	// First, get the operation to access machine info
	op, ok := r.store.GetOperation(operationID)
	if !ok {
		return fmt.Errorf("operation %q not found", operationID)
	}

	// Always try to cancel in the store first (idempotent)
	_, err := r.store.CancelOperation(operationID)
	if err != nil {
		// If already finished, that's an error
		return err
	}

	// Update machine state for canceled operations
	// Set OperationCanceled condition but do NOT change phase (phase is imperative intent)
	machine, ok := r.store.GetMachine(op.MachineId)
	if ok {
		lifecycle.SetCondition(machine, lifecycle.ConditionOperationCanceled, true, "UserCanceled", "Operation was canceled by user")
		machine.Status.ActiveOperationId = ""
		r.store.UpdateMachine(machine)
	}

	// Cancel the active context if the operation is still executing
	r.mu.Lock()
	cancel, active := r.activeOperations[operationID]
	if active {
		cancel()
		delete(r.activeOperations, operationID)
	}
	r.mu.Unlock()

	// Emit cancel event
	if updatedOp, ok := r.store.GetOperation(operationID); ok {
		r.emitEvent(updatedOp, "Operation canceled")
		r.emitLog(operationID, "stdout", []byte("\n=== Operation CANCELED ===\n"))
	}

	return nil
}

// executeOperation is the main operation execution loop.
// Called after StartOperation has already transitioned the operation to RUNNING.
func (r *Runner) executeOperation(ctx context.Context, operationID string) {
	defer func() {
		r.mu.Lock()
		delete(r.activeOperations, operationID)
		r.mu.Unlock()
	}()

	// Panic safety: recover from any panic in execution and clean up properly
	defer func() {
		if rec := recover(); rec != nil {
			r.handlePanic(operationID, rec)
		}
	}()

	op, ok := r.store.GetOperation(operationID)
	if !ok {
		log.Printf("[executor] operation %s not found", operationID)
		return
	}

	machine, ok := r.store.GetMachine(op.MachineId)
	if !ok {
		log.Printf("[executor] machine %s not found for operation %s", op.MachineId, operationID)
		r.failOperation(operationID, "machine not found")
		return
	}

	// Get the plan
	plan := r.getPlanForOperation(op)
	if plan == nil {
		log.Printf("[executor] no plan found for operation %s", operationID)
		r.failOperation(operationID, "plan not found")
		return
	}

	// Operation is already RUNNING (set by StartOperation via TryTransitionOperationPhase)
	// Emit the start event with the current operation state
	op, _ = r.store.GetOperation(operationID) // refresh to get StartedAt
	r.emitEvent(op, "Operation started")

	// Log operation start with params
	logMsg := fmt.Sprintf("=== Starting operation %s (type: %s) ===\n", operationID, op.Type)
	if len(op.Params) > 0 {
		logMsg += fmt.Sprintf("Parameters: %v\n", op.Params)
	}
	r.emitLog(operationID, "stdout", []byte(logMsg))

	// Update machine: set active operation ID
	machine.Status.ActiveOperationId = operationID
	r.store.UpdateMachine(machine)

	// Execute steps
	var lastErr error
	completedReimage := false

	for i, step := range plan.Steps {
		select {
		case <-ctx.Done():
			r.cancelOperation(operationID)
			return
		default:
		}

		r.emitLog(operationID, "stdout", []byte(fmt.Sprintf("\n--- Step %d/%d: %s ---\n", i+1, len(plan.Steps), step.Name)))

		// Set step to WAITING then RUNNING using internal workflow types
		stepStatus := &workflow.StepStatus{
			Name:      step.Name,
			State:     workflow.StepStateWaiting,
			StartedAt: time.Now(),
		}
		r.store.UpdateWorkflowStep(operationID, stepStatus)

		stepStatus.State = workflow.StepStateRunning
		r.store.UpdateWorkflowStep(operationID, stepStatus)
		// Emit event with fresh operation state from store
		r.emitEventFromStore(operationID, fmt.Sprintf("Step %s started", step.Name))

		// Execute with retries
		// MaxRetries means retries AFTER the first attempt, so total attempts = 1 + MaxRetries
		maxRetries := int(step.MaxRetries)
		if maxRetries < 0 {
			maxRetries = 0
		}
		totalAttempts := 1 + maxRetries

		var stepErr error
		for attempt := 0; attempt < totalAttempts; attempt++ {
			if attempt > 0 {
				backoff := r.calculateBackoff(attempt)
				r.emitLog(operationID, "stdout", []byte(fmt.Sprintf("Retry %d/%d after %v...\n", attempt, maxRetries, backoff)))
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					r.cancelOperation(operationID)
					return
				}
			}

			stepErr = r.executeStep(ctx, op, machine, step)
			if stepErr == nil {
				break
			}

			if ctx.Err() != nil {
				r.cancelOperation(operationID)
				return
			}

			r.emitLog(operationID, "stderr", []byte(fmt.Sprintf("Step failed: %v\n", stepErr)))
			stepStatus.RetryCount = int32(attempt + 1)
		}

		stepStatus.FinishedAt = time.Now()
		if stepErr != nil {
			stepStatus.State = workflow.StepStateFailed
			stepStatus.Message = stepErr.Error()
			r.store.UpdateWorkflowStep(operationID, stepStatus)
			r.emitEventFromStore(operationID, fmt.Sprintf("Step %s failed: %v", step.Name, stepErr))
			lastErr = stepErr
			break
		}

		stepStatus.State = workflow.StepStateSucceeded
		r.store.UpdateWorkflowStep(operationID, stepStatus)
		r.emitEventFromStore(operationID, fmt.Sprintf("Step %s succeeded", step.Name))

		// Track specific step completions
		switch step.Kind.(type) {
		case workflow.RepaveImage:
			completedReimage = true
		}
	}

	// Finalize operation
	if lastErr != nil {
		r.failOperationWithMachine(operationID, machine, lastErr.Error())
	} else {
		r.succeedOperation(operationID, machine, completedReimage, op.Type)
	}
}

// getPlanForOperation retrieves the plan for an operation.
// Uses internal workflow.Plan types, NOT proto types.
func (r *Runner) getPlanForOperation(op *pb.Operation) *workflow.Plan {
	// Map operation type to default plan
	switch op.Type {
	case pb.Operation_REIMAGE:
		if plan, ok := r.plans.GetPlan(plans.PlanRepaveJoin); ok {
			return plan
		}
	case pb.Operation_REBOOT:
		if plan, ok := r.plans.GetPlan(plans.PlanReboot); ok {
			return plan
		}
	case pb.Operation_ENTER_MAINTENANCE:
		// Simple no-op plan for entering maintenance
		return &workflow.Plan{
			PlanID:      "plan/enter-maintenance",
			DisplayName: "Enter Maintenance",
			Steps:       []*workflow.Step{}, // No steps - just a phase transition
		}
	case pb.Operation_EXIT_MAINTENANCE:
		// Simple no-op plan for exiting maintenance
		return &workflow.Plan{
			PlanID:      "plan/exit-maintenance",
			DisplayName: "Exit Maintenance",
			Steps:       []*workflow.Step{}, // No steps - just a phase transition
		}
	}

	// Fallback to simple reboot plan
	plan, _ := r.plans.GetPlan(plans.PlanReboot)
	return plan
}

// executeStep executes a single step using internal workflow types.
func (r *Runner) executeStep(ctx context.Context, op *pb.Operation, machine *pb.Machine, step *workflow.Step) error {
	timeout := time.Duration(step.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	operationID := op.OperationId

	switch kind := step.Kind.(type) {
	case workflow.SSHCommand:
		return r.provider.ExecuteSSHCommand(stepCtx, operationID, machine, kind.ScriptRef, kind.Args)

	case workflow.Reboot:
		return r.provider.Reboot(stepCtx, operationID, machine, kind.Force)

	case workflow.SetNetboot:
		return r.provider.SetNetboot(stepCtx, operationID, machine, kind.Profile)

	case workflow.RepaveImage:
		// Use image_ref from operation params if available, otherwise use plan default
		imageRef := kind.ImageRef
		if opImageRef := op.Params["image_ref"]; opImageRef != "" {
			imageRef = opImageRef
		}
		return r.provider.Repave(stepCtx, operationID, machine, imageRef, kind.CloudInitRef)

	case workflow.KubeadmJoin:
		// Mint join material and join
		targetCluster := machine.Spec.GetTargetCluster()
		if targetCluster == nil {
			targetCluster = &pb.TargetClusterRef{ClusterId: "default-cluster"}
		}

		material, err := r.provider.MintJoinMaterial(stepCtx, operationID, targetCluster)
		if err != nil {
			return fmt.Errorf("mint join material: %w", err)
		}
		return r.provider.JoinNode(stepCtx, operationID, machine, material)

	case workflow.VerifyInCluster:
		targetCluster := machine.Spec.GetTargetCluster()
		if targetCluster == nil {
			targetCluster = &pb.TargetClusterRef{ClusterId: "default-cluster"}
		}
		return r.provider.VerifyInCluster(stepCtx, operationID, machine, targetCluster)

	case workflow.NetReconfig:
		// Net reconfig is a stub - just log and succeed
		r.emitLog(operationID, "stdout", []byte(fmt.Sprintf("[net-reconfig] Applying config: %v\n", kind.Params)))
		time.Sleep(500 * time.Millisecond)
		r.emitLog(operationID, "stdout", []byte("[net-reconfig] Network reconfiguration complete\n"))
		return nil

	case workflow.RMAAction:
		reason := kind.Reason
		if reason == "" {
			reason = "no reason specified"
		}
		return r.provider.RMA(stepCtx, operationID, machine, reason)

	default:
		return fmt.Errorf("unknown step kind: %T", step.Kind)
	}
}

// calculateBackoff returns exponential backoff duration for retry attempt.
func (r *Runner) calculateBackoff(attempt int) time.Duration {
	backoff := r.baseRetryWait * time.Duration(math.Pow(2, float64(attempt)))
	if backoff > r.maxRetryWait {
		backoff = r.maxRetryWait
	}
	return backoff
}

// handlePanic handles a panic during operation execution, cleaning up state properly.
// NOTE: Does NOT change machine.phase - phase is imperative intent. Sets NeedsIntervention instead.
func (r *Runner) handlePanic(operationID string, recovered interface{}) {
	panicMsg := fmt.Sprintf("panic: %v", recovered)
	stackTrace := string(debug.Stack())

	log.Printf("[executor] PANIC in operation %s: %s\n%s", operationID, panicMsg, stackTrace)

	// Mark operation as failed with PANIC error code
	r.store.CompleteOperation(operationID, pb.Operation_FAILED)
	if op, ok := r.store.GetOperation(operationID); ok {
		op.Error = &pb.ErrorStatus{
			Code:      "PANIC",
			Message:   panicMsg,
			Retryable: false,
		}
		r.store.UpdateOperation(op)
		r.emitEvent(op, fmt.Sprintf("Operation panicked: %s", panicMsg))
		r.emitLog(operationID, "stderr", []byte(fmt.Sprintf("\n=== Operation PANICKED ===\n%s\n%s\n", panicMsg, stackTrace)))
	}

	// Get the operation to find the machine
	op, ok := r.store.GetOperation(operationID)
	if !ok {
		return
	}

	// Update machine state: set NeedsIntervention but do NOT change phase
	// Phase is imperative intent - only EnterMaintenance/ExitMaintenance should modify it
	machine, ok := r.store.GetMachine(op.MachineId)
	if ok {
		lifecycle.SetCondition(machine, lifecycle.ConditionNeedsIntervention, true, "Panic", panicMsg)
		machine.Status.ActiveOperationId = ""
		r.store.UpdateMachine(machine)
	}
}

// failOperation marks operation as failed.
func (r *Runner) failOperation(operationID, message string) {
	r.store.CompleteOperation(operationID, pb.Operation_FAILED)
	if op, ok := r.store.GetOperation(operationID); ok {
		op.Error = &pb.ErrorStatus{
			Code:      "EXECUTION_FAILED",
			Message:   message,
			Retryable: true,
		}
		r.store.UpdateOperation(op)
		r.emitEvent(op, fmt.Sprintf("Operation failed: %s", message))
		r.emitLog(operationID, "stderr", []byte(fmt.Sprintf("\n=== Operation FAILED: %s ===\n", message)))
	}
}

// failOperationWithMachine marks operation as failed and updates machine state.
// NOTE: Does NOT change machine.phase - phase is imperative intent. Sets NeedsIntervention instead.
func (r *Runner) failOperationWithMachine(operationID string, machine *pb.Machine, message string) {
	r.failOperation(operationID, message)

	// Set NeedsIntervention condition - failures require investigation
	// Do NOT change phase - phase is imperative intent
	lifecycle.SetCondition(machine, lifecycle.ConditionNeedsIntervention, true, "OperationFailed", message)
	machine.Status.ActiveOperationId = ""

	// Persist the updated machine
	r.store.UpdateMachine(machine)
}

// cancelOperation is called when an operation is canceled via context (internal path).
// It marks operation as canceled and updates machine state.
// NOTE: Does NOT change machine.phase - phase is imperative intent.
func (r *Runner) cancelOperation(operationID string) {
	// Get operation info before canceling
	op, ok := r.store.GetOperation(operationID)
	if !ok {
		return
	}

	// Cancel in store
	r.store.CancelOperation(operationID)

	// Update machine state - use OperationCanceled (not NeedsIntervention)
	// Cancellation is a normal operation, not a failure requiring intervention.
	// Do NOT change phase - phase is imperative intent
	machine, ok := r.store.GetMachine(op.MachineId)
	if ok {
		lifecycle.SetCondition(machine, lifecycle.ConditionOperationCanceled, true, "Canceled", "Operation was canceled")
		machine.Status.ActiveOperationId = ""
		r.store.UpdateMachine(machine)
	}

	// Emit events
	if updatedOp, ok := r.store.GetOperation(operationID); ok {
		r.emitEvent(updatedOp, "Operation canceled")
		r.emitLog(operationID, "stdout", []byte("\n=== Operation CANCELED ===\n"))
	}
}

// succeedOperation marks operation as succeeded and updates machine state.
func (r *Runner) succeedOperation(operationID string, machine *pb.Machine, completedReimage bool, opType pb.Operation_OperationType) {
	r.store.CompleteOperation(operationID, pb.Operation_SUCCEEDED)

	op, _ := r.store.GetOperation(operationID)
	r.emitEvent(op, "Operation succeeded")
	r.emitLog(operationID, "stdout", []byte("\n=== Operation SUCCEEDED ===\n"))

	// Clear active operation
	machine.Status.ActiveOperationId = ""

	// Clear any intervention/canceled conditions on success
	lifecycle.SetCondition(machine, lifecycle.ConditionNeedsIntervention, false, "OperationSucceeded", "")
	lifecycle.SetCondition(machine, lifecycle.ConditionOperationCanceled, false, "OperationSucceeded", "")

	// Update machine phase based on operation type
	// NOTE: Only ENTER_MAINTENANCE and EXIT_MAINTENANCE change phase.
	// Other operations do NOT change phase - phase is imperative intent.
	switch opType {
	case pb.Operation_ENTER_MAINTENANCE:
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_MAINTENANCE)
	case pb.Operation_EXIT_MAINTENANCE:
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_READY)
	case pb.Operation_REIMAGE:
		if completedReimage {
			lifecycle.SetCondition(machine, lifecycle.ConditionProvisioned, true, "ReimageComplete", "Machine successfully reimaged")
			lifecycle.SetCondition(machine, lifecycle.ConditionInCustomerCluster, true, "JoinVerified", "Node joined and verified in cluster")
		}
		// Stay in current phase after reimage - operator must explicitly exit maintenance
	case pb.Operation_REBOOT:
		// Keep current phase - reboot doesn't change phase
	default:
		// Default: keep current phase
	}

	// Persist the updated machine
	r.store.UpdateMachine(machine)
}

// IsRunning returns true if the runner has an active operation.
func (r *Runner) IsRunning(operationID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.activeOperations[operationID]
	return ok
}

// ActiveOperationCount returns the number of active operations.
func (r *Runner) ActiveOperationCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.activeOperations)
}

// Shutdown cancels all active operations.
func (r *Runner) Shutdown() {
	r.mu.Lock()
	for opID, cancel := range r.activeOperations {
		log.Printf("[executor] canceling operation %s on shutdown", opID)
		cancel()
	}
	r.mu.Unlock()
}
