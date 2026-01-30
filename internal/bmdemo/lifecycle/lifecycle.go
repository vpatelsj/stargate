// Package lifecycle provides helpers for managing baremetal machine lifecycle state.
package lifecycle

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

// Condition type constants - all condition types should be defined here
// and referenced from other packages to avoid string literal duplication.
const (
	ConditionReachable         = "Reachable"
	ConditionInCustomerCluster = "InCustomerCluster"
	ConditionNeedsIntervention = "NeedsIntervention"
	ConditionHealthy           = "Healthy"
	ConditionProvisioned       = "Provisioned"
	ConditionOperationCanceled = "OperationCanceled"
	ConditionRMA               = "RMA"     // Machine flagged for RMA
	ConditionRetired           = "Retired" // Machine retired from service
)

// SetMachinePhase sets the phase on a machine's status.
func SetMachinePhase(machine *pb.Machine, phase pb.MachineStatus_Phase) {
	if machine == nil {
		return
	}
	if machine.Status == nil {
		machine.Status = &pb.MachineStatus{}
	}
	machine.Status.Phase = phase
}

// SetCondition sets or updates a condition on a machine's status.
func SetCondition(machine *pb.Machine, condType string, status bool, reason, message string) {
	if machine == nil {
		return
	}
	if machine.Status == nil {
		machine.Status = &pb.MachineStatus{}
	}

	now := timestamppb.Now()

	// Find existing condition
	for i, c := range machine.Status.Conditions {
		if c.Type == condType {
			// Only update transition time if status changed
			if c.Status != status {
				c.LastTransitionTime = now
			}
			c.Status = status
			c.Reason = reason
			c.Message = message
			machine.Status.Conditions[i] = c
			return
		}
	}

	// Add new condition
	machine.Status.Conditions = append(machine.Status.Conditions, &pb.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	})
}

// GetCondition retrieves a condition by type. Returns nil if not found.
func GetCondition(status *pb.MachineStatus, condType string) *pb.Condition {
	if status == nil {
		return nil
	}
	for _, c := range status.Conditions {
		if c.Type == condType {
			return c
		}
	}
	return nil
}

// HasCondition checks if a condition exists and has the specified status.
func HasCondition(status *pb.MachineStatus, condType string, wantStatus bool) bool {
	c := GetCondition(status, condType)
	return c != nil && c.Status == wantStatus
}

// IsBusy returns true if the machine has an active operation.
// This is determined by checking active_operation_id.
func IsBusy(machine *pb.Machine) bool {
	if machine == nil || machine.Status == nil {
		return false
	}
	return machine.Status.ActiveOperationId != ""
}

// IsBusyWithOperation returns true if the machine has an active operation
// and checks the operation's phase if provided.
func IsBusyWithOperation(machine *pb.Machine, op *pb.Operation) bool {
	if machine == nil || machine.Status == nil {
		return false
	}
	if machine.Status.ActiveOperationId == "" {
		return false
	}
	if op == nil {
		return true // has active_operation_id but no operation provided to check
	}
	return IsActiveOperationPhase(op.Phase)
}

// IsTerminalOperationPhase returns true if the operation phase is terminal (completed/failed/canceled).
func IsTerminalOperationPhase(phase pb.Operation_Phase) bool {
	switch phase {
	case pb.Operation_SUCCEEDED, pb.Operation_FAILED, pb.Operation_CANCELED:
		return true
	}
	return false
}

// IsActiveOperationPhase returns true if the operation phase is active (pending/running).
func IsActiveOperationPhase(phase pb.Operation_Phase) bool {
	switch phase {
	case pb.Operation_PENDING, pb.Operation_RUNNING:
		return true
	}
	return false
}

// ComputeEffectiveState computes the derived effective state for a machine.
// This implements deterministic precedence rules:
//  1. If condition Retired==true OR condition RMA==true => BLOCKED
//  2. Else if condition NeedsIntervention==true => ATTENTION
//  3. Else if active operation exists AND operation.phase in {PENDING,RUNNING} => BUSY
//  4. Else if phase == MAINTENANCE => MAINTENANCE_IDLE
//  5. Else if phase == FACTORY_READY => NEW
//  6. Else => IDLE
//
// The activeOp parameter should be the current active operation if known,
// or nil if no operation is active or the operation's phase is unknown.
func ComputeEffectiveState(machine *pb.Machine, activeOp *pb.Operation) pb.MachineStatus_EffectiveState {
	if machine == nil || machine.Status == nil {
		return pb.MachineStatus_EFFECTIVE_UNSPECIFIED
	}

	status := machine.Status

	// Rule 1: BLOCKED if RMA or Retired condition is true
	if HasCondition(status, ConditionRetired, true) || HasCondition(status, ConditionRMA, true) {
		return pb.MachineStatus_BLOCKED
	}

	// Rule 2: ATTENTION if NeedsIntervention condition is true
	if HasCondition(status, ConditionNeedsIntervention, true) {
		return pb.MachineStatus_ATTENTION
	}

	// Rule 3: BUSY if active operation in PENDING/RUNNING phase
	if status.ActiveOperationId != "" {
		if activeOp != nil && IsActiveOperationPhase(activeOp.Phase) {
			return pb.MachineStatus_BUSY
		}
		// If we have an active_operation_id but no operation provided,
		// we assume it's still active (conservative)
		if activeOp == nil {
			return pb.MachineStatus_BUSY
		}
	}

	// Rule 4: MAINTENANCE_IDLE if phase is MAINTENANCE
	if status.Phase == pb.MachineStatus_MAINTENANCE {
		return pb.MachineStatus_MAINTENANCE_IDLE
	}

	// Rule 5: NEW if phase is FACTORY_READY
	if status.Phase == pb.MachineStatus_FACTORY_READY {
		return pb.MachineStatus_NEW
	}

	// Rule 6: IDLE otherwise (including READY phase)
	return pb.MachineStatus_IDLE
}

// PopulateEffectiveState populates the effective_state field on a machine's status.
// This should be called before returning machines to clients.
func PopulateEffectiveState(machine *pb.Machine, activeOp *pb.Operation) {
	if machine == nil || machine.Status == nil {
		return
	}
	machine.Status.EffectiveState = ComputeEffectiveState(machine, activeOp)
}
