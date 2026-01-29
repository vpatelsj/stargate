package baremetal

import "time"

type MachinePhase string

const (
	PhaseUnspecified  MachinePhase = "PHASE_UNSPECIFIED"
	PhaseFactoryReady MachinePhase = "FACTORY_READY"
	PhaseReady        MachinePhase = "READY"
	PhaseProvisioning MachinePhase = "PROVISIONING"
	PhaseInService    MachinePhase = "IN_SERVICE"
	PhaseMaintenance  MachinePhase = "MAINTENANCE"
	PhaseRMA          MachinePhase = "RMA"
	PhaseRetired      MachinePhase = "RETIRED"
)

type RunPhase string

const (
	RunPending   RunPhase = "PENDING"
	RunRunning   RunPhase = "RUNNING"
	RunSucceeded RunPhase = "SUCCEEDED"
	RunFailed    RunPhase = "FAILED"
	RunCanceled  RunPhase = "CANCELED"
)

type ConditionType string

const (
	CondReachable         ConditionType = "Reachable"
	CondBMCReachable      ConditionType = "BMCReachable"
	CondProvisioned       ConditionType = "Provisioned"
	CondInCustomerCluster ConditionType = "InCustomerCluster"
	CondDrained           ConditionType = "Drained"
	CondDegraded          ConditionType = "Degraded"
	CondNeedsIntervention ConditionType = "NeedsIntervention"
)

type Condition struct {
	Type   ConditionType
	Status bool
	Reason string
	Msg    string
	At     time.Time
}

type MachineStatus struct {
	Phase      MachinePhase
	Conditions []Condition
	ActiveRun  string
	LastSeen   time.Time
}

type Run struct {
	ID        string
	MachineID string
	Phase     RunPhase
	Type      string

	CurrentStep string
	Steps       []StepStatus

	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time
}

type StepState string

const (
	StepWaiting   StepState = "WAITING"
	StepRunning   StepState = "RUNNING"
	StepSucceeded StepState = "SUCCEEDED"
	StepFailed    StepState = "FAILED"
)

type StepStatus struct {
	Name       string
	State      StepState
	RetryCount int
	StartedAt  time.Time
	FinishedAt time.Time
	Message    string
}
