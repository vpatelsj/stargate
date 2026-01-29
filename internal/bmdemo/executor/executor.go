// Package executor provides the run executor that processes provisioning runs.
package executor

import (
	"context"
	"fmt"
	"log"
	"math"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/lifecycle"
	"github.com/vpatelsj/stargate/internal/bmdemo/plans"
	"github.com/vpatelsj/stargate/internal/bmdemo/provider"
	"github.com/vpatelsj/stargate/internal/bmdemo/store"
)

// Condition types set by the executor
const (
	ConditionProvisioned       = "Provisioned"
	ConditionInCustomerCluster = "InCustomerCluster"
	ConditionNeedsIntervention = "NeedsIntervention"
)

// EventCallback is called on run state transitions.
type EventCallback func(event *pb.RunEvent)

// LogCallback is called to stream logs.
type LogCallback func(chunk *pb.LogChunk)

// Runner executes runs asynchronously.
type Runner struct {
	store    *store.Store
	provider provider.Provider
	plans    *plans.Registry

	mu            sync.RWMutex
	eventSubID    uint64                            // counter for unique subscriber IDs
	eventSubs     map[uint64]EventCallback          // subscriberID -> callback
	logSubID      map[string]uint64                 // runID -> counter for unique log subscriber IDs
	logSubs       map[string]map[uint64]LogCallback // runID -> subscriberID -> callback
	activeRuns    map[string]context.CancelFunc
	baseRetryWait time.Duration
	maxRetryWait  time.Duration
}

// NewRunner creates a new run executor.
func NewRunner(s *store.Store, p provider.Provider, pl *plans.Registry) *Runner {
	return &Runner{
		store:         s,
		provider:      p,
		plans:         pl,
		eventSubs:     make(map[uint64]EventCallback),
		logSubID:      make(map[string]uint64),
		logSubs:       make(map[string]map[uint64]LogCallback),
		activeRuns:    make(map[string]context.CancelFunc),
		baseRetryWait: 500 * time.Millisecond,
		maxRetryWait:  10 * time.Second,
	}
}

// SetProvider sets the provider (for deferred initialization).
func (r *Runner) SetProvider(p provider.Provider) {
	r.provider = p
}

// EmitLog is called by the provider to stream logs. This is the public API.
func (r *Runner) EmitLog(runID, stream string, data []byte) {
	r.emitLog(runID, stream, data)
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

// SubscribeLogs adds a log subscriber for a specific run. Returns unsubscribe function.
func (r *Runner) SubscribeLogs(runID string, cb LogCallback) func() {
	r.mu.Lock()
	if r.logSubs[runID] == nil {
		r.logSubs[runID] = make(map[uint64]LogCallback)
	}
	r.logSubID[runID]++
	id := r.logSubID[runID]
	r.logSubs[runID][id] = cb
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subs, ok := r.logSubs[runID]; ok {
			delete(subs, id)
			// Clean up empty map
			if len(subs) == 0 {
				delete(r.logSubs, runID)
				delete(r.logSubID, runID)
			}
		}
	}
}

// emitEvent sends an event to all subscribers with an immutable snapshot.
func (r *Runner) emitEvent(run *pb.Run, message string) {
	// Clone the run to ensure immutability
	snapshot := proto.Clone(run).(*pb.Run)
	event := &pb.RunEvent{
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

// emitLog sends a log chunk to run subscribers.
func (r *Runner) emitLog(runID, stream string, data []byte) {
	chunk := &pb.LogChunk{
		Ts:     timestamppb.Now(),
		RunId:  runID,
		Stream: stream,
		Data:   data,
	}

	r.mu.RLock()
	var subs []LogCallback
	if m, ok := r.logSubs[runID]; ok {
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

// StartRun begins executing a run asynchronously.
// Returns immediately; run proceeds in background.
func (r *Runner) StartRun(ctx context.Context, runID string) error {
	run, ok := r.store.GetRun(runID)
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}

	if run.Phase != pb.Run_PENDING {
		return fmt.Errorf("run %q is not pending (phase=%s)", runID, run.Phase)
	}

	// Create cancellable context for this run
	runCtx, cancel := context.WithCancel(context.Background())

	r.mu.Lock()
	r.activeRuns[runID] = cancel
	r.mu.Unlock()

	// Execute asynchronously
	go r.executeRun(runCtx, runID)

	return nil
}

// CancelRun cancels a run immediately.
// It marks the run as CANCELED in the store, clears machine state, and stops execution.
// Idempotent: canceling an already-canceled run returns success.
func (r *Runner) CancelRun(runID string) error {
	// First, get the run to access machine info
	run, ok := r.store.GetRun(runID)
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}

	// Always try to cancel in the store first (idempotent)
	_, err := r.store.CancelRun(runID)
	if err != nil {
		// If already finished, that's an error
		return err
	}

	// Update machine state for canceled runs
	machine, ok := r.store.GetMachine(run.MachineId)
	if ok {
		// Set machine to MAINTENANCE with NeedsIntervention
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_MAINTENANCE)
		lifecycle.SetCondition(machine, ConditionNeedsIntervention, true, "Canceled", "Run was canceled")
		machine.Status.ActiveRunId = ""
		r.store.UpdateMachine(machine)
	}

	// Cancel the active context if the run is still executing
	r.mu.Lock()
	cancel, active := r.activeRuns[runID]
	if active {
		cancel()
		delete(r.activeRuns, runID)
	}
	r.mu.Unlock()

	// Emit cancel event
	if updatedRun, ok := r.store.GetRun(runID); ok {
		r.emitEvent(updatedRun, "Run canceled")
		r.emitLog(runID, "stdout", []byte("\n=== Run CANCELED ===\n"))
	}

	return nil
}

// executeRun is the main run execution loop.
func (r *Runner) executeRun(ctx context.Context, runID string) {
	defer func() {
		r.mu.Lock()
		delete(r.activeRuns, runID)
		r.mu.Unlock()
	}()

	run, ok := r.store.GetRun(runID)
	if !ok {
		log.Printf("[executor] run %s not found", runID)
		return
	}

	machine, ok := r.store.GetMachine(run.MachineId)
	if !ok {
		log.Printf("[executor] machine %s not found for run %s", run.MachineId, runID)
		r.failRun(runID, "machine not found")
		return
	}

	// Get the plan
	plan := r.getPlanForRun(run)
	if plan == nil {
		log.Printf("[executor] no plan found for run %s", runID)
		r.failRun(runID, "plan not found")
		return
	}

	// Transition to RUNNING
	if err := r.store.SetRunPhase(runID, pb.Run_RUNNING); err != nil {
		log.Printf("[executor] failed to set run phase: %v", err)
		return
	}
	run.Phase = pb.Run_RUNNING
	run.StartedAt = timestamppb.Now()
	r.emitEvent(run, "Run started")
	r.emitLog(runID, "stdout", []byte(fmt.Sprintf("=== Starting run %s (type: %s) ===\n", runID, run.Type)))

	// Update machine phase
	lifecycle.SetMachinePhase(machine, pb.MachineStatus_PROVISIONING)
	machine.Status.ActiveRunId = runID

	// Execute steps
	var lastErr error
	completedJoin := false
	completedRepave := false
	completedVerify := false
	isRMA := plan.PlanId == plans.PlanRMA

	for i, step := range plan.Steps {
		select {
		case <-ctx.Done():
			r.cancelRun(runID)
			return
		default:
		}

		r.emitLog(runID, "stdout", []byte(fmt.Sprintf("\n--- Step %d/%d: %s ---\n", i+1, len(plan.Steps), step.Name)))

		// Set step to WAITING then RUNNING
		stepStatus := &pb.StepStatus{
			Name:      step.Name,
			State:     pb.StepStatus_WAITING,
			StartedAt: timestamppb.Now(),
		}
		r.store.UpdateRunStep(runID, stepStatus)

		stepStatus.State = pb.StepStatus_RUNNING
		r.store.UpdateRunStep(runID, stepStatus)
		run.CurrentStep = step.Name
		r.emitEvent(run, fmt.Sprintf("Step %s started", step.Name))

		// Execute with retries
		maxRetries := int(step.MaxRetries)
		if maxRetries < 1 {
			maxRetries = 1
		}

		var stepErr error
		for attempt := 0; attempt < maxRetries; attempt++ {
			if attempt > 0 {
				backoff := r.calculateBackoff(attempt)
				r.emitLog(runID, "stdout", []byte(fmt.Sprintf("Retry %d/%d after %v...\n", attempt+1, maxRetries, backoff)))
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					r.cancelRun(runID)
					return
				}
			}

			stepErr = r.executeStep(ctx, runID, machine, step)
			if stepErr == nil {
				break
			}

			if ctx.Err() != nil {
				r.cancelRun(runID)
				return
			}

			r.emitLog(runID, "stderr", []byte(fmt.Sprintf("Step failed: %v\n", stepErr)))
			stepStatus.RetryCount = int32(attempt + 1)
		}

		stepStatus.FinishedAt = timestamppb.Now()
		if stepErr != nil {
			stepStatus.State = pb.StepStatus_FAILED
			stepStatus.Message = stepErr.Error()
			r.store.UpdateRunStep(runID, stepStatus)
			r.emitEvent(run, fmt.Sprintf("Step %s failed: %v", step.Name, stepErr))
			lastErr = stepErr
			break
		}

		stepStatus.State = pb.StepStatus_SUCCEEDED
		r.store.UpdateRunStep(runID, stepStatus)
		r.emitEvent(run, fmt.Sprintf("Step %s succeeded", step.Name))

		// Track specific step completions
		switch step.Kind.(type) {
		case *pb.Step_Repave:
			completedRepave = true
		case *pb.Step_Join:
			completedJoin = true
		case *pb.Step_Verify:
			completedVerify = true
		}
	}

	// Finalize run
	if lastErr != nil {
		r.failRunWithMachine(runID, machine, lastErr.Error())
	} else {
		// Check if the plan contains a verify step
		planHasVerify := r.planContainsVerifyStep(plan)
		r.succeedRun(runID, machine, completedRepave, completedJoin, completedVerify, planHasVerify, isRMA)
	}
}

// getPlanForRun retrieves the plan for a run.
// Priority: plan_id > type-based lookup > fallback
func (r *Runner) getPlanForRun(run *pb.Run) *pb.Plan {
	// First, try explicit plan_id
	if run.PlanId != "" {
		if plan, ok := r.plans.GetPlan(run.PlanId); ok {
			return plan
		}
	}

	// Second, map type to default plan
	switch run.Type {
	case "REPAVE", "repave":
		if plan, ok := r.plans.GetPlan(plans.PlanRepaveJoin); ok {
			return plan
		}
	case "RMA", "rma":
		if plan, ok := r.plans.GetPlan(plans.PlanRMA); ok {
			return plan
		}
	case "REBOOT", "reboot":
		if plan, ok := r.plans.GetPlan(plans.PlanReboot); ok {
			return plan
		}
	case "UPGRADE", "upgrade":
		if plan, ok := r.plans.GetPlan(plans.PlanUpgrade); ok {
			return plan
		}
	case "NET_RECONFIG", "net-reconfig":
		if plan, ok := r.plans.GetPlan(plans.PlanNetReconfig); ok {
			return plan
		}
	}

	// Fallback to simple reboot plan
	plan, _ := r.plans.GetPlan(plans.PlanReboot)
	return plan
}

// executeStep executes a single step.
func (r *Runner) executeStep(ctx context.Context, runID string, machine *pb.Machine, step *pb.Step) error {
	timeout := time.Duration(step.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}

	stepCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch kind := step.Kind.(type) {
	case *pb.Step_Ssh:
		return r.provider.ExecuteSSHCommand(stepCtx, runID, machine, kind.Ssh.ScriptRef, kind.Ssh.Args)

	case *pb.Step_Reboot:
		return r.provider.Reboot(stepCtx, runID, machine, kind.Reboot.Force)

	case *pb.Step_Netboot:
		return r.provider.SetNetboot(stepCtx, runID, machine, kind.Netboot.Profile)

	case *pb.Step_Repave:
		return r.provider.Repave(stepCtx, runID, machine, kind.Repave.ImageRef, kind.Repave.CloudInitRef)

	case *pb.Step_Join:
		// Mint join material and join
		targetCluster := kind.Join.TargetCluster
		if targetCluster == nil {
			targetCluster = machine.Spec.GetTargetCluster()
		}
		if targetCluster == nil {
			targetCluster = &pb.TargetClusterRef{ClusterId: "default-cluster"}
		}

		material, err := r.provider.MintJoinMaterial(stepCtx, runID, targetCluster)
		if err != nil {
			return fmt.Errorf("mint join material: %w", err)
		}
		return r.provider.JoinNode(stepCtx, runID, machine, material)

	case *pb.Step_Verify:
		targetCluster := kind.Verify.TargetCluster
		if targetCluster == nil {
			targetCluster = machine.Spec.GetTargetCluster()
		}
		if targetCluster == nil {
			targetCluster = &pb.TargetClusterRef{ClusterId: "default-cluster"}
		}
		return r.provider.VerifyInCluster(stepCtx, runID, machine, targetCluster)

	case *pb.Step_Net:
		// Net reconfig is a stub - just log and succeed
		r.emitLog(runID, "stdout", []byte(fmt.Sprintf("[net-reconfig] Applying config: %v\n", kind.Net.Params)))
		time.Sleep(500 * time.Millisecond)
		r.emitLog(runID, "stdout", []byte("[net-reconfig] Network reconfiguration complete\n"))
		return nil

	case *pb.Step_Rma:
		reason := kind.Rma.Reason
		if reason == "" {
			reason = "no reason specified"
		}
		return r.provider.RMA(stepCtx, runID, machine, reason)

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

// failRun marks run as failed.
func (r *Runner) failRun(runID, message string) {
	r.store.CompleteRun(runID, pb.Run_FAILED)
	if run, ok := r.store.GetRun(runID); ok {
		run.Error = &pb.ErrorStatus{
			Code:      "EXECUTION_FAILED",
			Message:   message,
			Retryable: true,
		}
		r.store.UpdateRun(run)
		r.emitEvent(run, fmt.Sprintf("Run failed: %s", message))
		r.emitLog(runID, "stderr", []byte(fmt.Sprintf("\n=== Run FAILED: %s ===\n", message)))
	}
}

// failRunWithMachine marks run as failed and updates machine state.
func (r *Runner) failRunWithMachine(runID string, machine *pb.Machine, message string) {
	r.failRun(runID, message)

	// Set NeedsIntervention condition
	lifecycle.SetCondition(machine, ConditionNeedsIntervention, true, "RunFailed", message)
	machine.Status.ActiveRunId = ""
	lifecycle.SetMachinePhase(machine, pb.MachineStatus_MAINTENANCE)

	// Persist the updated machine
	r.store.UpdateMachine(machine)
}

// cancelRun is called when a run is canceled via context (internal path).
// It marks run as canceled and updates machine state.
func (r *Runner) cancelRun(runID string) {
	// Get run info before canceling
	run, ok := r.store.GetRun(runID)
	if !ok {
		return
	}

	// Cancel in store
	r.store.CancelRun(runID)

	// Update machine state
	machine, ok := r.store.GetMachine(run.MachineId)
	if ok {
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_MAINTENANCE)
		lifecycle.SetCondition(machine, ConditionNeedsIntervention, true, "Canceled", "Run was canceled")
		machine.Status.ActiveRunId = ""
		r.store.UpdateMachine(machine)
	}

	// Emit events
	if updatedRun, ok := r.store.GetRun(runID); ok {
		r.emitEvent(updatedRun, "Run canceled")
		r.emitLog(runID, "stdout", []byte("\n=== Run CANCELED ===\n"))
	}
}

// planContainsVerifyStep checks if a plan has a verify step.
func (r *Runner) planContainsVerifyStep(plan *pb.Plan) bool {
	if plan == nil {
		return false
	}
	for _, step := range plan.Steps {
		if _, ok := step.Kind.(*pb.Step_Verify); ok {
			return true
		}
	}
	return false
}

// succeedRun marks run as succeeded and updates machine state.
// completedVerify indicates if a verify step succeeded.
// planHasVerify indicates if the plan contains a verify step.
func (r *Runner) succeedRun(runID string, machine *pb.Machine, completedRepave, completedJoin, completedVerify, planHasVerify, isRMA bool) {
	r.store.CompleteRun(runID, pb.Run_SUCCEEDED)

	run, _ := r.store.GetRun(runID)
	r.emitEvent(run, "Run succeeded")
	r.emitLog(runID, "stdout", []byte("\n=== Run SUCCEEDED ===\n"))

	// Clear active run
	machine.Status.ActiveRunId = ""

	// Clear any intervention condition
	lifecycle.SetCondition(machine, ConditionNeedsIntervention, false, "RunSucceeded", "")

	// Update machine phase based on what completed
	if isRMA {
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_RMA)
		r.store.UpdateMachine(machine)
		return
	}

	if completedRepave {
		lifecycle.SetCondition(machine, ConditionProvisioned, true, "RepaveComplete", "Machine successfully reprovisioned")
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_READY)
	}

	// Only set InCustomerCluster if:
	// - Plan has verify step AND verify succeeded, OR
	// - Plan has no verify step AND join succeeded
	if completedJoin {
		shouldSetInCluster := false
		if planHasVerify {
			// Plan has verify - require verify success
			if completedVerify {
				shouldSetInCluster = true
			}
		} else {
			// Plan has no verify - join success is enough
			shouldSetInCluster = true
		}

		if shouldSetInCluster {
			lifecycle.SetCondition(machine, ConditionInCustomerCluster, true, "JoinVerified", "Node joined and verified in cluster")
			lifecycle.SetMachinePhase(machine, pb.MachineStatus_IN_SERVICE)
		}
	}

	// If neither repave nor join, just set to READY
	if !completedRepave && !completedJoin && !isRMA {
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_READY)
	}

	// Persist the updated machine
	r.store.UpdateMachine(machine)
}

// IsRunning returns true if the runner has an active run.
func (r *Runner) IsRunning(runID string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.activeRuns[runID]
	return ok
}

// ActiveRunCount returns the number of active runs.
func (r *Runner) ActiveRunCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.activeRuns)
}

// Shutdown cancels all active runs.
func (r *Runner) Shutdown() {
	r.mu.Lock()
	for runID, cancel := range r.activeRuns {
		log.Printf("[executor] canceling run %s on shutdown", runID)
		cancel()
	}
	r.mu.Unlock()
}
