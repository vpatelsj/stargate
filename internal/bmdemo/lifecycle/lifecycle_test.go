package lifecycle

import (
	"testing"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

func TestSetMachinePhase(t *testing.T) {
	tests := []struct {
		name    string
		machine *pb.Machine
		phase   pb.MachineStatus_Phase
		want    pb.MachineStatus_Phase
	}{
		{
			name:    "nil machine",
			machine: nil,
			phase:   pb.MachineStatus_READY,
			want:    pb.MachineStatus_PHASE_UNSPECIFIED,
		},
		{
			name:    "nil status",
			machine: &pb.Machine{},
			phase:   pb.MachineStatus_READY,
			want:    pb.MachineStatus_READY,
		},
		{
			name:    "existing status",
			machine: &pb.Machine{Status: &pb.MachineStatus{Phase: pb.MachineStatus_FACTORY_READY}},
			phase:   pb.MachineStatus_MAINTENANCE,
			want:    pb.MachineStatus_MAINTENANCE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			SetMachinePhase(tt.machine, tt.phase)
			if tt.machine == nil {
				return
			}
			if tt.machine.Status.Phase != tt.want {
				t.Errorf("got %v, want %v", tt.machine.Status.Phase, tt.want)
			}
		})
	}
}

func TestSetCondition(t *testing.T) {
	machine := &pb.Machine{}

	// Add new condition
	SetCondition(machine, ConditionReachable, true, "Connected", "SSH connection successful")

	if len(machine.Status.Conditions) != 1 {
		t.Fatalf("expected 1 condition, got %d", len(machine.Status.Conditions))
	}

	c := machine.Status.Conditions[0]
	if c.Type != ConditionReachable || !c.Status || c.Reason != "Connected" {
		t.Errorf("unexpected condition: %+v", c)
	}

	// Update existing condition
	SetCondition(machine, ConditionReachable, false, "Disconnected", "SSH timeout")

	if len(machine.Status.Conditions) != 1 {
		t.Fatalf("expected still 1 condition, got %d", len(machine.Status.Conditions))
	}

	c = machine.Status.Conditions[0]
	if c.Status || c.Reason != "Disconnected" {
		t.Errorf("condition not updated: %+v", c)
	}

	// Add second condition
	SetCondition(machine, ConditionInCustomerCluster, true, "Joined", "Node registered")

	if len(machine.Status.Conditions) != 2 {
		t.Fatalf("expected 2 conditions, got %d", len(machine.Status.Conditions))
	}
}

func TestIsBusy(t *testing.T) {
	tests := []struct {
		name    string
		machine *pb.Machine
		want    bool
	}{
		{
			name:    "nil machine",
			machine: nil,
			want:    false,
		},
		{
			name:    "nil status",
			machine: &pb.Machine{},
			want:    false,
		},
		{
			name: "no active operation",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{},
			},
			want: false,
		},
		{
			name: "has active operation",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{ActiveOperationId: "op-123"},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsBusy(tt.machine)
			if got != tt.want {
				t.Errorf("IsBusy() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsBusyWithOperation(t *testing.T) {
	tests := []struct {
		name    string
		machine *pb.Machine
		op      *pb.Operation
		want    bool
	}{
		{
			name:    "nil machine",
			machine: nil,
			op:      nil,
			want:    false,
		},
		{
			name:    "no active operation id",
			machine: &pb.Machine{Status: &pb.MachineStatus{}},
			op:      nil,
			want:    false,
		},
		{
			name:    "has active operation id, no op provided",
			machine: &pb.Machine{Status: &pb.MachineStatus{ActiveOperationId: "op-123"}},
			op:      nil,
			want:    true,
		},
		{
			name:    "has active operation id, pending op",
			machine: &pb.Machine{Status: &pb.MachineStatus{ActiveOperationId: "op-123"}},
			op:      &pb.Operation{Phase: pb.Operation_PENDING},
			want:    true,
		},
		{
			name:    "has active operation id, running op",
			machine: &pb.Machine{Status: &pb.MachineStatus{ActiveOperationId: "op-123"}},
			op:      &pb.Operation{Phase: pb.Operation_RUNNING},
			want:    true,
		},
		{
			name:    "has active operation id, succeeded op",
			machine: &pb.Machine{Status: &pb.MachineStatus{ActiveOperationId: "op-123"}},
			op:      &pb.Operation{Phase: pb.Operation_SUCCEEDED},
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsBusyWithOperation(tt.machine, tt.op)
			if got != tt.want {
				t.Errorf("IsBusyWithOperation() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestHasCondition(t *testing.T) {
	status := &pb.MachineStatus{
		Conditions: []*pb.Condition{
			{Type: ConditionReachable, Status: true},
			{Type: ConditionInCustomerCluster, Status: false},
		},
	}

	if !HasCondition(status, ConditionReachable, true) {
		t.Error("expected Reachable=true")
	}
	if HasCondition(status, ConditionReachable, false) {
		t.Error("Reachable should not be false")
	}
	if HasCondition(status, ConditionInCustomerCluster, true) {
		t.Error("InCustomerCluster should be false")
	}
	if !HasCondition(status, ConditionInCustomerCluster, false) {
		t.Error("expected InCustomerCluster=false")
	}
	if HasCondition(status, "NonExistent", true) {
		t.Error("NonExistent condition should not exist")
	}
}

func TestIsTerminalOperationPhase(t *testing.T) {
	tests := []struct {
		phase pb.Operation_Phase
		want  bool
	}{
		{pb.Operation_PHASE_UNSPECIFIED, false},
		{pb.Operation_PENDING, false},
		{pb.Operation_RUNNING, false},
		{pb.Operation_SUCCEEDED, true},
		{pb.Operation_FAILED, true},
		{pb.Operation_CANCELED, true},
	}

	for _, tt := range tests {
		if got := IsTerminalOperationPhase(tt.phase); got != tt.want {
			t.Errorf("IsTerminalOperationPhase(%v) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}

func TestIsActiveOperationPhase(t *testing.T) {
	tests := []struct {
		phase pb.Operation_Phase
		want  bool
	}{
		{pb.Operation_PHASE_UNSPECIFIED, false},
		{pb.Operation_PENDING, true},
		{pb.Operation_RUNNING, true},
		{pb.Operation_SUCCEEDED, false},
		{pb.Operation_FAILED, false},
		{pb.Operation_CANCELED, false},
	}

	for _, tt := range tests {
		if got := IsActiveOperationPhase(tt.phase); got != tt.want {
			t.Errorf("IsActiveOperationPhase(%v) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}

// TestComputeEffectiveState tests the precedence rules for effective state computation.
func TestComputeEffectiveState(t *testing.T) {
	tests := []struct {
		name     string
		machine  *pb.Machine
		activeOp *pb.Operation
		want     pb.MachineStatus_EffectiveState
	}{
		{
			name:    "nil machine",
			machine: nil,
			want:    pb.MachineStatus_EFFECTIVE_UNSPECIFIED,
		},
		{
			name:    "nil status",
			machine: &pb.Machine{},
			want:    pb.MachineStatus_EFFECTIVE_UNSPECIFIED,
		},
		{
			name: "BLOCKED - Retired condition true (highest precedence)",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_READY,
					ActiveOperationId: "op-1",
					Conditions: []*pb.Condition{
						{Type: ConditionRetired, Status: true},
						{Type: ConditionNeedsIntervention, Status: true},
					},
				},
			},
			activeOp: &pb.Operation{Phase: pb.Operation_RUNNING},
			want:     pb.MachineStatus_BLOCKED,
		},
		{
			name: "BLOCKED - RMA condition true",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase: pb.MachineStatus_MAINTENANCE,
					Conditions: []*pb.Condition{
						{Type: ConditionRMA, Status: true},
					},
				},
			},
			want: pb.MachineStatus_BLOCKED,
		},
		{
			name: "ATTENTION - NeedsIntervention overrides BUSY",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_READY,
					ActiveOperationId: "op-1",
					Conditions: []*pb.Condition{
						{Type: ConditionNeedsIntervention, Status: true},
					},
				},
			},
			activeOp: &pb.Operation{Phase: pb.Operation_RUNNING},
			want:     pb.MachineStatus_ATTENTION,
		},
		{
			name: "BUSY - active RUNNING operation",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_READY,
					ActiveOperationId: "op-1",
				},
			},
			activeOp: &pb.Operation{Phase: pb.Operation_RUNNING},
			want:     pb.MachineStatus_BUSY,
		},
		{
			name: "BUSY - active PENDING operation",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_MAINTENANCE,
					ActiveOperationId: "op-1",
				},
			},
			activeOp: &pb.Operation{Phase: pb.Operation_PENDING},
			want:     pb.MachineStatus_BUSY,
		},
		{
			name: "BUSY - active_operation_id set, no op provided (conservative)",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_READY,
					ActiveOperationId: "op-1",
				},
			},
			activeOp: nil,
			want:     pb.MachineStatus_BUSY,
		},
		{
			name: "BUSY overrides MAINTENANCE - machine in MAINTENANCE but op RUNNING",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_MAINTENANCE,
					ActiveOperationId: "op-1",
				},
			},
			activeOp: &pb.Operation{Phase: pb.Operation_RUNNING},
			want:     pb.MachineStatus_BUSY,
		},
		{
			name: "MAINTENANCE_IDLE - phase MAINTENANCE, no active operation",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase: pb.MachineStatus_MAINTENANCE,
				},
			},
			want: pb.MachineStatus_MAINTENANCE_IDLE,
		},
		{
			name: "MAINTENANCE_IDLE - phase MAINTENANCE, operation completed",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_MAINTENANCE,
					ActiveOperationId: "op-1",
				},
			},
			activeOp: &pb.Operation{Phase: pb.Operation_SUCCEEDED},
			want:     pb.MachineStatus_MAINTENANCE_IDLE,
		},
		{
			name: "NEW - phase FACTORY_READY",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase: pb.MachineStatus_FACTORY_READY,
				},
			},
			want: pb.MachineStatus_NEW,
		},
		{
			name: "IDLE - phase READY, no active operation",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase: pb.MachineStatus_READY,
				},
			},
			want: pb.MachineStatus_IDLE,
		},
		{
			name: "IDLE - phase READY, operation completed",
			machine: &pb.Machine{
				Status: &pb.MachineStatus{
					Phase:             pb.MachineStatus_READY,
					ActiveOperationId: "op-1",
				},
			},
			activeOp: &pb.Operation{Phase: pb.Operation_SUCCEEDED},
			want:     pb.MachineStatus_IDLE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ComputeEffectiveState(tt.machine, tt.activeOp)
			if got != tt.want {
				t.Errorf("ComputeEffectiveState() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestComputeEffectiveState_PrecedenceOrder verifies that precedence is correctly ordered.
func TestComputeEffectiveState_PrecedenceOrder(t *testing.T) {
	// Test: BLOCKED > ATTENTION > BUSY > MAINTENANCE > NEW > IDLE

	// BLOCKED beats everything
	m := &pb.Machine{
		Status: &pb.MachineStatus{
			Phase:             pb.MachineStatus_MAINTENANCE,
			ActiveOperationId: "op-1",
			Conditions: []*pb.Condition{
				{Type: ConditionRetired, Status: true},
				{Type: ConditionNeedsIntervention, Status: true},
			},
		},
	}
	op := &pb.Operation{Phase: pb.Operation_RUNNING}
	if got := ComputeEffectiveState(m, op); got != pb.MachineStatus_BLOCKED {
		t.Errorf("BLOCKED should beat everything, got %v", got)
	}

	// ATTENTION beats BUSY
	m.Status.Conditions = []*pb.Condition{
		{Type: ConditionNeedsIntervention, Status: true},
	}
	if got := ComputeEffectiveState(m, op); got != pb.MachineStatus_ATTENTION {
		t.Errorf("ATTENTION should beat BUSY, got %v", got)
	}

	// BUSY beats MAINTENANCE
	m.Status.Conditions = nil
	if got := ComputeEffectiveState(m, op); got != pb.MachineStatus_BUSY {
		t.Errorf("BUSY should beat MAINTENANCE_IDLE, got %v", got)
	}

	// MAINTENANCE_IDLE when no active op
	m.Status.ActiveOperationId = ""
	if got := ComputeEffectiveState(m, nil); got != pb.MachineStatus_MAINTENANCE_IDLE {
		t.Errorf("Expected MAINTENANCE_IDLE when in MAINTENANCE phase with no active op, got %v", got)
	}
}
