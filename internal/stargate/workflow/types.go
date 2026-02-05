// Package workflow provides internal workflow engine types for plan/step execution.
// These types are NOT exposed in the public API or SDK.
package workflow

import (
	"time"
)

// Plan defines a workflow plan with a sequence of steps.
type Plan struct {
	PlanID      string
	DisplayName string
	Steps       []*Step
}

// Step defines a single step in a workflow plan.
type Step struct {
	Name           string
	Kind           StepKind
	TimeoutSeconds int32
	MaxRetries     int32
}

// StepKind is the type of step to execute.
type StepKind interface {
	stepKind()
}

// Step kind implementations
type (
	SSHCommand struct {
		ScriptRef string
		Args      map[string]string
	}
	Reboot          struct{ Force bool }
	SetNetboot      struct{ Profile string }
	RepaveImage     struct{ ImageRef, CloudInitRef string }
	KubeadmJoin     struct{ ClusterID, KubeconfigSecretRef string }
	VerifyInCluster struct{ ClusterID, KubeconfigSecretRef string }
	NetReconfig     struct{ Params map[string]string }
	RMAAction       struct{ Reason string }
)

func (SSHCommand) stepKind()      {}
func (Reboot) stepKind()          {}
func (SetNetboot) stepKind()      {}
func (RepaveImage) stepKind()     {}
func (KubeadmJoin) stepKind()     {}
func (VerifyInCluster) stepKind() {}
func (NetReconfig) stepKind()     {}
func (RMAAction) stepKind()       {}

// StepState represents the execution state of a step.
type StepState int

const (
	StepStateUnspecified StepState = iota
	StepStateWaiting
	StepStateRunning
	StepStateSucceeded
	StepStateFailed
)

func (s StepState) String() string {
	switch s {
	case StepStateWaiting:
		return "WAITING"
	case StepStateRunning:
		return "RUNNING"
	case StepStateSucceeded:
		return "SUCCEEDED"
	case StepStateFailed:
		return "FAILED"
	default:
		return "UNSPECIFIED"
	}
}

// StepStatus tracks the execution status of a step.
type StepStatus struct {
	Name       string
	State      StepState
	RetryCount int32
	StartedAt  time.Time
	FinishedAt time.Time
	Message    string
}

// OperationWorkflow holds internal workflow state for an operation.
// This is stored separately from the public Operation and never exposed.
type OperationWorkflow struct {
	OperationID string
	PlanID      string
	Steps       []*StepStatus
}

// Clone returns a deep copy of the workflow.
func (w *OperationWorkflow) Clone() *OperationWorkflow {
	if w == nil {
		return nil
	}
	clone := &OperationWorkflow{
		OperationID: w.OperationID,
		PlanID:      w.PlanID,
	}
	if w.Steps != nil {
		clone.Steps = make([]*StepStatus, len(w.Steps))
		for i, s := range w.Steps {
			stepClone := *s
			clone.Steps[i] = &stepClone
		}
	}
	return clone
}

// CurrentStep returns the name of the currently executing step.
func (w *OperationWorkflow) CurrentStep() string {
	if w == nil {
		return ""
	}
	for _, s := range w.Steps {
		if s.State == StepStateRunning {
			return s.Name
		}
	}
	// Return last step if none running
	if len(w.Steps) > 0 {
		return w.Steps[len(w.Steps)-1].Name
	}
	return ""
}
