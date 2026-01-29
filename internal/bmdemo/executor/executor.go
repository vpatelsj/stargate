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
	"github.com/vpatelsj/stargate/internal/bmdemo/provider/fake"
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
	provider *fake.Provider
	plans    *plans.Registry

	mu            sync.RWMutex
	eventSubs     []EventCallback
	logSubs       map[string][]LogCallback // runID -> subscribers
	activeRuns    map[string]context.CancelFunc
	baseRetryWait time.Duration
	maxRetryWait  time.Duration
}

// NewRunner creates a new run executor.
func NewRunner(s *store.Store, p *fake.Provider, pl *plans.Registry) *Runner {
	return &Runner{
		store:         s,
		provider:      p,
		plans:         pl,
		logSubs:       make(map[string][]LogCallback),
		activeRuns:    make(map[string]context.CancelFunc),
		baseRetryWait: 500 * time.Millisecond,
		maxRetryWait:  10 * time.Second,
	}
}

// SetProvider sets the provider (for deferred initialization).
func (r *Runner) SetProvider(p *fake.Provider) {
	r.provider = p
}

// EmitLog is called by the provider to stream logs. This is the public API.
func (r *Runner) EmitLog(runID, stream string, data []byte) {
	r.emitLog(runID, stream, data)
}

// SubscribeEvents adds an event subscriber.
func (r *Runner) SubscribeEvents(cb EventCallback) func() {
	r.mu.Lock()
	r.eventSubs = append(r.eventSubs, cb)
	idx := len(r.eventSubs) - 1
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		// Mark as nil instead of removing to avoid index issues
		if idx < len(r.eventSubs) {
			r.eventSubs[idx] = nil
		}
	}
}

// SubscribeLogs adds a log subscriber for a specific run.
func (r *Runner) SubscribeLogs(runID string, cb LogCallback) func() {
	r.mu.Lock()
	r.logSubs[runID] = append(r.logSubs[runID], cb)
	idx := len(r.logSubs[runID]) - 1
	r.mu.Unlock()

	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if subs, ok := r.logSubs[runID]; ok && idx < len(subs) {
			r.logSubs[runID][idx] = nil
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
	subs := make([]EventCallback, len(r.eventSubs))
	copy(subs, r.eventSubs)
	r.mu.RUnlock()

	for _, cb := range subs {
		if cb != nil {
			cb(event)
		}
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
	subs := r.logSubs[runID]
	subsCopy := make([]LogCallback, len(subs))
	copy(subsCopy, subs)
	r.mu.RUnlock()

	for _, cb := range subsCopy {
		if cb != nil {
			cb(chunk)
		}
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

// CancelRun cancels an active run.
func (r *Runner) CancelRun(runID string) error {
	r.mu.Lock()
	cancel, ok := r.activeRuns[runID]
	r.mu.Unlock()

	if !ok {
		// Try to cancel via store (might already be done)
		_, err := r.store.CancelRun(runID)
		return err
	}

	cancel()
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
	isRMA := run.Type == "RMA" || run.Type == plans.PlanRMA

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
			if completedJoin {
				completedJoin = true // Mark verify after join as success
			}
		}
	}

	// Finalize run
	if lastErr != nil {
		r.failRunWithMachine(runID, machine, lastErr.Error())
	} else {
		r.succeedRun(runID, machine, completedRepave, completedJoin, isRMA)
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

// cancelRun marks run as canceled.
func (r *Runner) cancelRun(runID string) {
	r.store.CancelRun(runID)
	if run, ok := r.store.GetRun(runID); ok {
		r.emitEvent(run, "Run canceled")
		r.emitLog(runID, "stdout", []byte("\n=== Run CANCELED ===\n"))
	}
}

// succeedRun marks run as succeeded and updates machine state.
func (r *Runner) succeedRun(runID string, machine *pb.Machine, completedRepave, completedJoin, isRMA bool) {
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

	if completedJoin {
		lifecycle.SetCondition(machine, ConditionInCustomerCluster, true, "JoinComplete", "Node joined and verified in cluster")
		lifecycle.SetMachinePhase(machine, pb.MachineStatus_IN_SERVICE)
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
