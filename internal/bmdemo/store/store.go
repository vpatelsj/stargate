// Package store provides an in-memory thread-safe store for baremetal machines and operations.
package store

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
	"github.com/vpatelsj/stargate/internal/bmdemo/workflow"
)

// Sentinel errors for typed error handling
var (
	ErrMachineNotFound           = errors.New("machine not found")
	ErrMachineHasActiveOperation = errors.New("machine has active operation")
	ErrOperationNotFound         = errors.New("operation not found")
	ErrOperationAlreadyFinished  = errors.New("operation already finished")
)

// cloneMachine returns a deep copy of a machine.
func cloneMachine(m *pb.Machine) *pb.Machine {
	if m == nil {
		return nil
	}
	return proto.Clone(m).(*pb.Machine)
}

// cloneOperation returns a deep copy of an operation.
func cloneOperation(op *pb.Operation) *pb.Operation {
	if op == nil {
		return nil
	}
	return proto.Clone(op).(*pb.Operation)
}

// Store is a thread-safe in-memory store for machines and operations.
type Store struct {
	mu sync.RWMutex

	// Primary storage
	machines   map[string]*pb.Machine
	operations map[string]*pb.Operation

	// Internal workflow state - NOT exposed in public API
	workflows map[string]*workflow.OperationWorkflow

	// Indexes
	requestIndex map[string]string // (machine_id:request_id) -> operation_id for idempotent operations

	// Per-machine locks to ensure only one active operation per machine
	machineLocks     map[string]*sync.Mutex
	machineOperating map[string]bool // machine_id -> has active operation
}

// New creates a new Store.
func New() *Store {
	return &Store{
		machines:         make(map[string]*pb.Machine),
		operations:       make(map[string]*pb.Operation),
		workflows:        make(map[string]*workflow.OperationWorkflow),
		requestIndex:     make(map[string]string),
		machineLocks:     make(map[string]*sync.Mutex),
		machineOperating: make(map[string]bool),
	}
}

// UpsertMachine creates or updates a machine. If machine_id is empty, one is generated.
func (s *Store) UpsertMachine(m *pb.Machine) (*pb.Machine, error) {
	if m == nil {
		return nil, fmt.Errorf("machine is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if m.MachineId == "" {
		m.MachineId = fmt.Sprintf("m-%d", time.Now().UnixNano())
	}

	if m.Status == nil {
		m.Status = &pb.MachineStatus{Phase: pb.MachineStatus_FACTORY_READY}
	}

	if _, ok := s.machineLocks[m.MachineId]; !ok {
		s.machineLocks[m.MachineId] = &sync.Mutex{}
	}

	// Store a clone internally
	s.machines[m.MachineId] = cloneMachine(m)
	return cloneMachine(m), nil
}

// GetMachine retrieves a machine by ID.
func (s *Store) GetMachine(machineID string) (*pb.Machine, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.machines[machineID]
	if !ok {
		return nil, false
	}
	return cloneMachine(m), true
}

// ListMachines returns all machines.
func (s *Store) ListMachines() []*pb.Machine {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*pb.Machine, 0, len(s.machines))
	for _, m := range s.machines {
		result = append(result, cloneMachine(m))
	}
	return result
}

// UpdateMachine updates an existing machine. Returns error if not found.
// This method clones spec and labels to avoid pointer aliasing.
// Status is backend-owned; if the caller passes a status, it is applied
// (the server layer is responsible for ensuring clients don't pass status).
func (s *Store) UpdateMachine(m *pb.Machine) (*pb.Machine, error) {
	if m == nil {
		return nil, fmt.Errorf("machine is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.machines[m.MachineId]
	if !ok {
		return nil, fmt.Errorf("machine %q not found", m.MachineId)
	}

	// Clone spec and labels to avoid pointer aliasing (caller should not
	// be able to mutate the stored machine after this call returns)
	if m.Spec != nil {
		existing.Spec = proto.Clone(m.Spec).(*pb.MachineSpec)
	}
	if m.Status != nil {
		existing.Status = proto.Clone(m.Status).(*pb.MachineStatus)
	}
	if m.Labels != nil {
		// Clone the labels map
		clonedLabels := make(map[string]string, len(m.Labels))
		for k, v := range m.Labels {
			clonedLabels[k] = v
		}
		existing.Labels = clonedLabels
	}

	s.machines[m.MachineId] = existing
	return cloneMachine(existing), nil
}

// cloneParams returns a deep copy of a params map.
func cloneParams(params map[string]string) map[string]string {
	if params == nil {
		return nil
	}
	clone := make(map[string]string, len(params))
	for k, v := range params {
		clone[k] = v
	}
	return clone
}

// CreateOperationIfNotExists creates a new operation if the request_id hasn't been seen before.
// Returns the operation and whether it was newly created (true) or already existed (false).
// Enforces that only one operation can be active per machine at a time.
// Idempotency is scoped to (machine_id, request_id) tuple.
//
// Errors returned:
//   - ErrMachineNotFound if machine doesn't exist
//   - ErrMachineHasActiveOperation if machine already has an active operation
func (s *Store) CreateOperationIfNotExists(requestID, machineID string, opType pb.Operation_OperationType, params map[string]string) (*pb.Operation, bool, error) {
	s.mu.RLock()
	if requestID != "" {
		idempotencyKey := machineID + ":" + requestID
		if existingOpID, ok := s.requestIndex[idempotencyKey]; ok {
			op := s.operations[existingOpID]
			s.mu.RUnlock()
			return cloneOperation(op), false, nil
		}
	}

	if _, ok := s.machines[machineID]; !ok {
		s.mu.RUnlock()
		return nil, false, fmt.Errorf("%w: %q", ErrMachineNotFound, machineID)
	}

	machineLock, ok := s.machineLocks[machineID]
	if !ok {
		s.mu.RUnlock()
		return nil, false, fmt.Errorf("%w: %q lock not found", ErrMachineNotFound, machineID)
	}
	s.mu.RUnlock()

	machineLock.Lock()
	defer machineLock.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if requestID != "" {
		idempotencyKey := machineID + ":" + requestID
		if existingOpID, ok := s.requestIndex[idempotencyKey]; ok {
			return cloneOperation(s.operations[existingOpID]), false, nil
		}
	}

	// Re-fetch machine under write lock to avoid mutating stale pointer
	// that may have been replaced by concurrent UpsertMachine.
	machine, ok := s.machines[machineID]
	if !ok {
		return nil, false, fmt.Errorf("%w: %q", ErrMachineNotFound, machineID)
	}

	if s.machineOperating[machineID] {
		activeOpID := machine.Status.GetActiveOperationId()
		return nil, false, fmt.Errorf("%w: %q has active operation %q", ErrMachineHasActiveOperation, machineID, activeOpID)
	}

	opID := fmt.Sprintf("op-%d", time.Now().UnixNano())
	op := &pb.Operation{
		OperationId: opID,
		MachineId:   machineID,
		Phase:       pb.Operation_PENDING,
		RequestId:   requestID,
		Type:        opType,
		Params:      cloneParams(params), // Clone params to prevent caller mutation
		CreatedAt:   timestamppb.Now(),
	}

	s.operations[opID] = op

	// Create internal workflow state
	s.workflows[opID] = &workflow.OperationWorkflow{
		OperationID: opID,
	}

	if requestID != "" {
		idempotencyKey := machineID + ":" + requestID
		s.requestIndex[idempotencyKey] = opID
	}

	s.machineOperating[machineID] = true

	if machine.Status == nil {
		machine.Status = &pb.MachineStatus{}
	}
	machine.Status.ActiveOperationId = opID
	// NOTE: We do NOT set machine.Status.Phase here.
	// The executor is responsible for phase transitions.
	// This keeps lifecycle semantics out of the store.

	return cloneOperation(op), true, nil
}

// GetOperation retrieves an operation by ID.
func (s *Store) GetOperation(opID string) (*pb.Operation, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	op, ok := s.operations[opID]
	if !ok {
		return nil, false
	}
	return cloneOperation(op), true
}

// ListOperations returns all operations.
func (s *Store) ListOperations() []*pb.Operation {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*pb.Operation, 0, len(s.operations))
	for _, op := range s.operations {
		result = append(result, cloneOperation(op))
	}
	return result
}

// UpdateOperation updates an operation's fields.
func (s *Store) UpdateOperation(op *pb.Operation) error {
	if op == nil {
		return fmt.Errorf("operation is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.operations[op.OperationId]; !ok {
		return fmt.Errorf("operation %q not found", op.OperationId)
	}

	s.operations[op.OperationId] = cloneOperation(op)
	return nil
}

// CancelOperation cancels an operation if it's still active.
// Idempotent: canceling an already-canceled operation returns success.
// Returns:
//   - ErrOperationNotFound if operation doesn't exist
//   - ErrOperationAlreadyFinished if operation is in SUCCEEDED or FAILED phase
//   - nil (success) if operation is canceled or was already canceled
func (s *Store) CancelOperation(opID string) (*pb.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[opID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrOperationNotFound, opID)
	}

	// Already canceled - return success (idempotent)
	if op.Phase == pb.Operation_CANCELED {
		return cloneOperation(op), nil
	}

	// Already finished with success or failure - cannot cancel
	if op.Phase == pb.Operation_SUCCEEDED || op.Phase == pb.Operation_FAILED {
		return nil, fmt.Errorf("%w: %q (phase=%s)", ErrOperationAlreadyFinished, opID, op.Phase)
	}

	op.Phase = pb.Operation_CANCELED
	op.FinishedAt = timestamppb.Now()

	s.clearMachineActiveOperation(op.MachineId)

	return cloneOperation(op), nil
}

// CompleteOperation marks an operation as completed and clears the machine's active operation.
// NOTE: This method does NOT change machine.Status.Phase - the executor
// is responsible for phase transitions based on what steps completed.
func (s *Store) CompleteOperation(opID string, phase pb.Operation_Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[opID]
	if !ok {
		return fmt.Errorf("operation %q not found", opID)
	}

	op.Phase = phase
	op.FinishedAt = timestamppb.Now()

	s.clearMachineActiveOperation(op.MachineId)

	return nil
}

func (s *Store) clearMachineActiveOperation(machineID string) {
	s.machineOperating[machineID] = false
	if machine, ok := s.machines[machineID]; ok {
		if machine.Status != nil {
			machine.Status.ActiveOperationId = ""
		}
	}
}

// GetWorkflow retrieves the internal workflow state for an operation.
func (s *Store) GetWorkflow(opID string) (*workflow.OperationWorkflow, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w, ok := s.workflows[opID]
	if !ok {
		return nil, false
	}
	return w.Clone(), true
}

// UpdateWorkflowStep adds or updates a step status in the internal workflow.
func (s *Store) UpdateWorkflowStep(opID string, step *workflow.StepStatus) error {
	if step == nil {
		return fmt.Errorf("step is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	wf, ok := s.workflows[opID]
	if !ok {
		return fmt.Errorf("workflow for operation %q not found", opID)
	}

	// NOTE: We intentionally do NOT update op.CurrentStage with step.Name
	// because step names are internal workflow details that should not be
	// exposed in the public API.

	// Clone the step to prevent external mutation
	stepClone := *step

	found := false
	for i, existing := range wf.Steps {
		if existing.Name == stepClone.Name {
			wf.Steps[i] = &stepClone
			found = true
			break
		}
	}
	if !found {
		wf.Steps = append(wf.Steps, &stepClone)
	}

	return nil
}

// SetOperationPhase updates just the phase of an operation.
func (s *Store) SetOperationPhase(opID string, phase pb.Operation_Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[opID]
	if !ok {
		return fmt.Errorf("operation %q not found", opID)
	}

	op.Phase = phase
	if phase == pb.Operation_RUNNING && op.StartedAt == nil {
		op.StartedAt = timestamppb.Now()
	}
	return nil
}

// TryTransitionOperationPhase atomically transitions an operation from one phase to another.
// Returns (true, nil) if the transition succeeded.
// Returns (false, nil) if the operation is already in a different phase (including terminal phases).
// Returns (false, error) only if the operation doesn't exist.
func (s *Store) TryTransitionOperationPhase(opID string, from, to pb.Operation_Phase) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	op, ok := s.operations[opID]
	if !ok {
		return false, fmt.Errorf("operation %q not found", opID)
	}

	// If already in target phase, or different from expected, no transition
	if op.Phase != from {
		return false, nil
	}

	op.Phase = to
	if to == pb.Operation_RUNNING && op.StartedAt == nil {
		op.StartedAt = timestamppb.Now()
	}
	if to == pb.Operation_SUCCEEDED || to == pb.Operation_FAILED || to == pb.Operation_CANCELED {
		op.FinishedAt = timestamppb.Now()
	}

	return true, nil
}
