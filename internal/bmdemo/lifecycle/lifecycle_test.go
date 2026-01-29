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
			phase:   pb.MachineStatus_IN_SERVICE,
			want:    pb.MachineStatus_PHASE_UNSPECIFIED,
		},
		{
			name:    "nil status",
			machine: &pb.Machine{},
			phase:   pb.MachineStatus_IN_SERVICE,
			want:    pb.MachineStatus_IN_SERVICE,
		},
		{
			name:    "existing status",
			machine: &pb.Machine{Status: &pb.MachineStatus{Phase: pb.MachineStatus_FACTORY_READY}},
			phase:   pb.MachineStatus_PROVISIONING,
			want:    pb.MachineStatus_PROVISIONING,
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

func TestEffectiveState_ActiveRunTakesPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		status    *pb.MachineStatus
		activeRun *pb.Run
		want      pb.MachineStatus_Phase
	}{
		{
			name:      "pending run",
			status:    &pb.MachineStatus{Phase: pb.MachineStatus_IN_SERVICE},
			activeRun: &pb.Run{Phase: pb.Run_PENDING},
			want:      pb.MachineStatus_PROVISIONING,
		},
		{
			name:      "running run",
			status:    &pb.MachineStatus{Phase: pb.MachineStatus_IN_SERVICE},
			activeRun: &pb.Run{Phase: pb.Run_RUNNING},
			want:      pb.MachineStatus_PROVISIONING,
		},
		{
			name:      "succeeded run - not active",
			status:    &pb.MachineStatus{Phase: pb.MachineStatus_READY},
			activeRun: &pb.Run{Phase: pb.Run_SUCCEEDED},
			want:      pb.MachineStatus_READY,
		},
		{
			name:      "failed run - not active",
			status:    &pb.MachineStatus{Phase: pb.MachineStatus_READY},
			activeRun: &pb.Run{Phase: pb.Run_FAILED},
			want:      pb.MachineStatus_READY,
		},
		{
			name:      "no active run",
			status:    &pb.MachineStatus{Phase: pb.MachineStatus_READY},
			activeRun: nil,
			want:      pb.MachineStatus_READY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveState(tt.status, tt.activeRun)
			if got != tt.want {
				t.Errorf("EffectiveState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveState_ExplicitPhasesWin(t *testing.T) {
	// Even with InCustomerCluster=true, RMA/RETIRED/MAINTENANCE should win
	statusWithCondition := func(phase pb.MachineStatus_Phase) *pb.MachineStatus {
		return &pb.MachineStatus{
			Phase: phase,
			Conditions: []*pb.Condition{
				{Type: ConditionInCustomerCluster, Status: true},
			},
		}
	}

	tests := []struct {
		name   string
		status *pb.MachineStatus
		want   pb.MachineStatus_Phase
	}{
		{
			name:   "RMA wins over InCustomerCluster",
			status: statusWithCondition(pb.MachineStatus_RMA),
			want:   pb.MachineStatus_RMA,
		},
		{
			name:   "RETIRED wins over InCustomerCluster",
			status: statusWithCondition(pb.MachineStatus_RETIRED),
			want:   pb.MachineStatus_RETIRED,
		},
		{
			name:   "MAINTENANCE wins over InCustomerCluster",
			status: statusWithCondition(pb.MachineStatus_MAINTENANCE),
			want:   pb.MachineStatus_MAINTENANCE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveState(tt.status, nil)
			if got != tt.want {
				t.Errorf("EffectiveState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveState_InCustomerClusterCondition(t *testing.T) {
	tests := []struct {
		name   string
		status *pb.MachineStatus
		want   pb.MachineStatus_Phase
	}{
		{
			name: "InCustomerCluster=true => IN_SERVICE",
			status: &pb.MachineStatus{
				Phase: pb.MachineStatus_READY,
				Conditions: []*pb.Condition{
					{Type: ConditionInCustomerCluster, Status: true},
				},
			},
			want: pb.MachineStatus_IN_SERVICE,
		},
		{
			name: "InCustomerCluster=false => READY",
			status: &pb.MachineStatus{
				Phase: pb.MachineStatus_READY,
				Conditions: []*pb.Condition{
					{Type: ConditionInCustomerCluster, Status: false},
				},
			},
			want: pb.MachineStatus_READY,
		},
		{
			name: "Multiple conditions, InCustomerCluster=true",
			status: &pb.MachineStatus{
				Phase: pb.MachineStatus_READY,
				Conditions: []*pb.Condition{
					{Type: ConditionReachable, Status: true},
					{Type: ConditionInCustomerCluster, Status: true},
					{Type: ConditionHealthy, Status: true},
				},
			},
			want: pb.MachineStatus_IN_SERVICE,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveState(tt.status, nil)
			if got != tt.want {
				t.Errorf("EffectiveState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveState_FactoryReadyStays(t *testing.T) {
	tests := []struct {
		name   string
		status *pb.MachineStatus
		want   pb.MachineStatus_Phase
	}{
		{
			name:   "FACTORY_READY stays FACTORY_READY",
			status: &pb.MachineStatus{Phase: pb.MachineStatus_FACTORY_READY},
			want:   pb.MachineStatus_FACTORY_READY,
		},
		{
			name:   "IN_SERVICE without condition becomes READY",
			status: &pb.MachineStatus{Phase: pb.MachineStatus_IN_SERVICE},
			want:   pb.MachineStatus_READY,
		},
		{
			name:   "PROVISIONING without active run becomes READY",
			status: &pb.MachineStatus{Phase: pb.MachineStatus_PROVISIONING},
			want:   pb.MachineStatus_READY,
		},
		{
			name:   "PHASE_UNSPECIFIED becomes READY",
			status: &pb.MachineStatus{Phase: pb.MachineStatus_PHASE_UNSPECIFIED},
			want:   pb.MachineStatus_READY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := EffectiveState(tt.status, nil)
			if got != tt.want {
				t.Errorf("EffectiveState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEffectiveState_PrecedenceOrder(t *testing.T) {
	// Test full precedence: active run > RMA > InCustomerCluster > FACTORY_READY

	// Active run beats everything
	status := &pb.MachineStatus{
		Phase: pb.MachineStatus_RMA,
		Conditions: []*pb.Condition{
			{Type: ConditionInCustomerCluster, Status: true},
		},
	}
	activeRun := &pb.Run{Phase: pb.Run_RUNNING}

	got := EffectiveState(status, activeRun)
	if got != pb.MachineStatus_PROVISIONING {
		t.Errorf("active run should beat RMA+InCustomerCluster: got %v", got)
	}

	// RMA beats InCustomerCluster (no active run)
	got = EffectiveState(status, nil)
	if got != pb.MachineStatus_RMA {
		t.Errorf("RMA should beat InCustomerCluster: got %v", got)
	}

	// InCustomerCluster beats FACTORY_READY
	status2 := &pb.MachineStatus{
		Phase: pb.MachineStatus_FACTORY_READY,
		Conditions: []*pb.Condition{
			{Type: ConditionInCustomerCluster, Status: true},
		},
	}
	got = EffectiveState(status2, nil)
	if got != pb.MachineStatus_IN_SERVICE {
		t.Errorf("InCustomerCluster should beat FACTORY_READY: got %v", got)
	}
}

func TestEffectiveState_NilStatus(t *testing.T) {
	got := EffectiveState(nil, nil)
	if got != pb.MachineStatus_PHASE_UNSPECIFIED {
		t.Errorf("nil status should return PHASE_UNSPECIFIED, got %v", got)
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

func TestIsTerminalRunPhase(t *testing.T) {
	tests := []struct {
		phase pb.Run_Phase
		want  bool
	}{
		{pb.Run_RUN_PHASE_UNSPECIFIED, false},
		{pb.Run_PENDING, false},
		{pb.Run_RUNNING, false},
		{pb.Run_SUCCEEDED, true},
		{pb.Run_FAILED, true},
		{pb.Run_CANCELED, true},
	}

	for _, tt := range tests {
		if got := IsTerminalRunPhase(tt.phase); got != tt.want {
			t.Errorf("IsTerminalRunPhase(%v) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}

func TestIsActiveRunPhase(t *testing.T) {
	tests := []struct {
		phase pb.Run_Phase
		want  bool
	}{
		{pb.Run_RUN_PHASE_UNSPECIFIED, false},
		{pb.Run_PENDING, true},
		{pb.Run_RUNNING, true},
		{pb.Run_SUCCEEDED, false},
		{pb.Run_FAILED, false},
		{pb.Run_CANCELED, false},
	}

	for _, tt := range tests {
		if got := IsActiveRunPhase(tt.phase); got != tt.want {
			t.Errorf("IsActiveRunPhase(%v) = %v, want %v", tt.phase, got, tt.want)
		}
	}
}
