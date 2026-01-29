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

// EffectiveState computes the effective machine phase based on precedence rules:
//  1. Active run pending/running => PROVISIONING
//  2. Explicit phases RMA/RETIRED/MAINTENANCE take precedence
//  3. InCustomerCluster condition true => IN_SERVICE
//  4. FACTORY_READY stays FACTORY_READY, otherwise READY
func EffectiveState(status *pb.MachineStatus, activeRun *pb.Run) pb.MachineStatus_Phase {
	if status == nil {
		return pb.MachineStatus_PHASE_UNSPECIFIED
	}

	// Rule 1: Active run in progress => PROVISIONING
	if activeRun != nil {
		switch activeRun.Phase {
		case pb.Run_PENDING, pb.Run_RUNNING:
			return pb.MachineStatus_PROVISIONING
		}
	}

	// Rule 2: Explicit operational phases take precedence
	switch status.Phase {
	case pb.MachineStatus_RMA:
		return pb.MachineStatus_RMA
	case pb.MachineStatus_RETIRED:
		return pb.MachineStatus_RETIRED
	case pb.MachineStatus_MAINTENANCE:
		return pb.MachineStatus_MAINTENANCE
	}

	// Rule 3: InCustomerCluster condition => IN_SERVICE
	if HasCondition(status, ConditionInCustomerCluster, true) {
		return pb.MachineStatus_IN_SERVICE
	}

	// Rule 4: FACTORY_READY stays, otherwise READY
	if status.Phase == pb.MachineStatus_FACTORY_READY {
		return pb.MachineStatus_FACTORY_READY
	}

	return pb.MachineStatus_READY
}

// IsTerminalRunPhase returns true if the run phase is terminal (completed/failed/canceled).
func IsTerminalRunPhase(phase pb.Run_Phase) bool {
	switch phase {
	case pb.Run_SUCCEEDED, pb.Run_FAILED, pb.Run_CANCELED:
		return true
	}
	return false
}

// IsActiveRunPhase returns true if the run phase is active (pending/running).
func IsActiveRunPhase(phase pb.Run_Phase) bool {
	switch phase {
	case pb.Run_PENDING, pb.Run_RUNNING:
		return true
	}
	return false
}
