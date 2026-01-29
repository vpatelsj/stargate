// Package store provides an in-memory thread-safe store for baremetal machines and runs.
package store

import (
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

// cloneMachine returns a deep copy of a machine.
func cloneMachine(m *pb.Machine) *pb.Machine {
	if m == nil {
		return nil
	}
	return proto.Clone(m).(*pb.Machine)
}

// cloneRun returns a deep copy of a run.
func cloneRun(r *pb.Run) *pb.Run {
	if r == nil {
		return nil
	}
	return proto.Clone(r).(*pb.Run)
}

// Store is a thread-safe in-memory store for machines and runs.
type Store struct {
	mu sync.RWMutex

	// Primary storage
	machines map[string]*pb.Machine
	runs     map[string]*pb.Run

	// Indexes
	requestIndex map[string]string // request_id -> run_id for idempotent StartRun

	// Per-machine locks to ensure only one active run per machine
	machineLocks   map[string]*sync.Mutex
	machineRunning map[string]bool // machine_id -> has active run
}

// New creates a new Store.
func New() *Store {
	return &Store{
		machines:       make(map[string]*pb.Machine),
		runs:           make(map[string]*pb.Run),
		requestIndex:   make(map[string]string),
		machineLocks:   make(map[string]*sync.Mutex),
		machineRunning: make(map[string]bool),
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

	if m.Spec != nil {
		existing.Spec = m.Spec
	}
	if m.Status != nil {
		existing.Status = m.Status
	}
	if m.Labels != nil {
		existing.Labels = m.Labels
	}

	s.machines[m.MachineId] = existing
	return cloneMachine(existing), nil
}

// CreateRunIfNotExists creates a new run if the request_id hasn't been seen before.
// Returns the run and whether it was newly created (true) or already existed (false).
// Enforces that only one run can be active per machine at a time.
// Idempotency is scoped to (machine_id, request_id) tuple.
func (s *Store) CreateRunIfNotExists(requestID, machineID, runType, planID string) (*pb.Run, bool, error) {
	s.mu.RLock()
	if requestID != "" {
		idempotencyKey := machineID + ":" + requestID
		if existingRunID, ok := s.requestIndex[idempotencyKey]; ok {
			run := s.runs[existingRunID]
			s.mu.RUnlock()
			return cloneRun(run), false, nil
		}
	}

	machine, ok := s.machines[machineID]
	if !ok {
		s.mu.RUnlock()
		return nil, false, fmt.Errorf("machine %q not found", machineID)
	}

	machineLock, ok := s.machineLocks[machineID]
	if !ok {
		s.mu.RUnlock()
		return nil, false, fmt.Errorf("machine %q lock not found", machineID)
	}
	s.mu.RUnlock()

	machineLock.Lock()
	defer machineLock.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()

	if requestID != "" {
		idempotencyKey := machineID + ":" + requestID
		if existingRunID, ok := s.requestIndex[idempotencyKey]; ok {
			return cloneRun(s.runs[existingRunID]), false, nil
		}
	}

	if s.machineRunning[machineID] {
		activeRunID := machine.Status.GetActiveRunId()
		return nil, false, fmt.Errorf("machine %q already has active run %q", machineID, activeRunID)
	}

	runID := fmt.Sprintf("run-%d", time.Now().UnixNano())
	run := &pb.Run{
		RunId:     runID,
		MachineId: machineID,
		Phase:     pb.Run_PENDING,
		RequestId: requestID,
		Type:      runType,
		PlanId:    planID,
		CreatedAt: timestamppb.Now(),
	}

	s.runs[runID] = run

	if requestID != "" {
		idempotencyKey := machineID + ":" + requestID
		s.requestIndex[idempotencyKey] = runID
	}

	s.machineRunning[machineID] = true

	if machine.Status == nil {
		machine.Status = &pb.MachineStatus{}
	}
	machine.Status.ActiveRunId = runID
	machine.Status.Phase = pb.MachineStatus_PROVISIONING

	return cloneRun(run), true, nil
}

// GetRun retrieves a run by ID.
func (s *Store) GetRun(runID string) (*pb.Run, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.runs[runID]
	if !ok {
		return nil, false
	}
	return cloneRun(r), true
}

// ListRuns returns all runs.
func (s *Store) ListRuns() []*pb.Run {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*pb.Run, 0, len(s.runs))
	for _, r := range s.runs {
		result = append(result, cloneRun(r))
	}
	return result
}

// UpdateRun updates a run's fields.
func (s *Store) UpdateRun(run *pb.Run) error {
	if run == nil {
		return fmt.Errorf("run is nil")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.runs[run.RunId]; !ok {
		return fmt.Errorf("run %q not found", run.RunId)
	}

	s.runs[run.RunId] = cloneRun(run)
	return nil
}

// CancelRun cancels a run if it's still active.
func (s *Store) CancelRun(runID string) (*pb.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("run %q not found", runID)
	}

	if run.Phase == pb.Run_SUCCEEDED || run.Phase == pb.Run_FAILED {
		return nil, fmt.Errorf("run %q already finished with phase %s", runID, run.Phase)
	}

	run.Phase = pb.Run_CANCELED
	run.FinishedAt = timestamppb.Now()

	s.clearMachineActiveRun(run.MachineId)

	return cloneRun(run), nil
}

// CompleteRun marks a run as completed and clears the machine's active run.
// NOTE: This method does NOT change machine.Status.Phase - the executor
// is responsible for phase transitions based on what steps completed.
func (s *Store) CompleteRun(runID string, phase pb.Run_Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}

	run.Phase = phase
	run.FinishedAt = timestamppb.Now()

	s.clearMachineActiveRun(run.MachineId)

	return nil
}

func (s *Store) clearMachineActiveRun(machineID string) {
	s.machineRunning[machineID] = false
	if machine, ok := s.machines[machineID]; ok {
		if machine.Status != nil {
			machine.Status.ActiveRunId = ""
		}
	}
}

// UpdateRunStep adds or updates a step status for a run.
func (s *Store) UpdateRunStep(runID string, step *pb.StepStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}

	found := false
	for i, existing := range run.Steps {
		if existing.Name == step.Name {
			run.Steps[i] = step
			found = true
			break
		}
	}
	if !found {
		run.Steps = append(run.Steps, step)
	}

	run.CurrentStep = step.Name
	return nil
}

// SetRunPhase updates just the phase of a run.
func (s *Store) SetRunPhase(runID string, phase pb.Run_Phase) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("run %q not found", runID)
	}

	run.Phase = phase
	if phase == pb.Run_RUNNING && run.StartedAt == nil {
		run.StartedAt = timestamppb.Now()
	}
	return nil
}
