// Package plans provides built-in plan definitions for baremetal provisioning workflows.
// These are internal types not exposed in the public API.
package plans

import (
	"github.com/vpatelsj/stargate/internal/bmdemo/workflow"
)

// Built-in plan IDs
const (
	PlanRepaveJoin  = "plan/repave-join"
	PlanRMA         = "plan/rma"
	PlanReboot      = "plan/reboot"
	PlanUpgrade     = "plan/upgrade"
	PlanNetReconfig = "plan/net-reconfig"
)

// builtinPlans contains all pre-defined plans.
var builtinPlans = map[string]*workflow.Plan{
	PlanRepaveJoin: {
		PlanID:      PlanRepaveJoin,
		DisplayName: "Repave and Join Cluster",
		Steps: []*workflow.Step{
			{
				Name:           "set-netboot",
				Kind:           workflow.SetNetboot{Profile: "pxe-ubuntu-22.04"},
				TimeoutSeconds: 60,
				MaxRetries:     2,
			},
			{
				Name:           "reboot-to-netboot",
				Kind:           workflow.Reboot{Force: false},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
			{
				Name: "repave-image",
				Kind: workflow.RepaveImage{
					ImageRef:     "ubuntu:22.04-k8s",
					CloudInitRef: "cloud-init/worker-node",
				},
				TimeoutSeconds: 600,
				MaxRetries:     1,
			},
			{
				Name:           "join-cluster",
				Kind:           workflow.KubeadmJoin{},
				TimeoutSeconds: 300,
				MaxRetries:     2,
			},
			{
				Name:           "verify-in-cluster",
				Kind:           workflow.VerifyInCluster{},
				TimeoutSeconds: 120,
				MaxRetries:     3,
			},
		},
	},

	PlanRMA: {
		PlanID:      PlanRMA,
		DisplayName: "Return Merchandise Authorization",
		Steps: []*workflow.Step{
			{
				Name: "drain-check",
				Kind: workflow.SSHCommand{
					ScriptRef: "drain_check.sh",
					Args:      map[string]string{"force": "false"},
				},
				TimeoutSeconds: 120,
				MaxRetries:     1,
			},
			{
				Name:           "graceful-shutdown",
				Kind:           workflow.Reboot{Force: false},
				TimeoutSeconds: 120,
				MaxRetries:     1,
			},
			{
				Name: "mark-rma",
				Kind: workflow.RMAAction{
					Reason: "hardware failure",
				},
				TimeoutSeconds: 60,
				MaxRetries:     1,
			},
		},
	},

	PlanReboot: {
		PlanID:      PlanReboot,
		DisplayName: "Simple Reboot",
		Steps: []*workflow.Step{
			{
				Name:           "reboot",
				Kind:           workflow.Reboot{Force: false},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
		},
	},

	PlanUpgrade: {
		PlanID:      PlanUpgrade,
		DisplayName: "Kubernetes Upgrade",
		Steps: []*workflow.Step{
			{
				Name: "cordon-node",
				Kind: workflow.SSHCommand{
					ScriptRef: "cordon_node.sh",
					Args:      map[string]string{},
				},
				TimeoutSeconds: 60,
				MaxRetries:     2,
			},
			{
				Name: "drain-node",
				Kind: workflow.SSHCommand{
					ScriptRef: "drain_node.sh",
					Args:      map[string]string{"timeout": "300"},
				},
				TimeoutSeconds: 600,
				MaxRetries:     1,
			},
			{
				Name: "upgrade-kubelet",
				Kind: workflow.SSHCommand{
					ScriptRef: "upgrade_k8s.sh",
					Args:      map[string]string{"version": "1.33.0"},
				},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
			{
				Name:           "restart-kubelet",
				Kind:           workflow.Reboot{Force: false},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
			{
				Name: "uncordon-node",
				Kind: workflow.SSHCommand{
					ScriptRef: "uncordon_node.sh",
					Args:      map[string]string{},
				},
				TimeoutSeconds: 60,
				MaxRetries:     2,
			},
			{
				Name:           "verify-upgrade",
				Kind:           workflow.VerifyInCluster{},
				TimeoutSeconds: 120,
				MaxRetries:     3,
			},
		},
	},

	PlanNetReconfig: {
		PlanID:      PlanNetReconfig,
		DisplayName: "Network Reconfiguration",
		Steps: []*workflow.Step{
			{
				Name: "apply-network-config",
				Kind: workflow.NetReconfig{
					Params: map[string]string{
						"action": "reconfigure",
					},
				},
				TimeoutSeconds: 120,
				MaxRetries:     2,
			},
			{
				Name: "verify-connectivity",
				Kind: workflow.SSHCommand{
					ScriptRef: "verify_network.sh",
					Args:      map[string]string{},
				},
				TimeoutSeconds: 60,
				MaxRetries:     3,
			},
		},
	},
}

// Registry holds plan definitions and provides lookup.
type Registry struct {
	plans map[string]*workflow.Plan
}

// NewRegistry creates a new plan registry with built-in plans.
func NewRegistry() *Registry {
	r := &Registry{
		plans: make(map[string]*workflow.Plan),
	}
	// Clone built-in plans so the registry is immutable from outside mutation
	for id, plan := range builtinPlans {
		r.plans[id] = clonePlan(plan)
	}
	return r
}

// clonePlan returns a deep copy of a plan to prevent callers from mutating shared state.
func clonePlan(p *workflow.Plan) *workflow.Plan {
	if p == nil {
		return nil
	}
	clone := &workflow.Plan{
		PlanID:      p.PlanID,
		DisplayName: p.DisplayName,
	}
	if p.Steps != nil {
		clone.Steps = make([]*workflow.Step, len(p.Steps))
		for i, s := range p.Steps {
			stepClone := *s
			clone.Steps[i] = &stepClone
		}
	}
	return clone
}

// GetPlan retrieves a plan by ID. Returns a deep clone so callers can't mutate registry state.
func (r *Registry) GetPlan(planID string) (*workflow.Plan, bool) {
	plan, ok := r.plans[planID]
	if !ok {
		return nil, false
	}
	return clonePlan(plan), true
}

// ListPlans returns all available plans. Returns deep clones so callers can't mutate registry state.
func (r *Registry) ListPlans() []*workflow.Plan {
	result := make([]*workflow.Plan, 0, len(r.plans))
	for _, plan := range r.plans {
		result = append(result, clonePlan(plan))
	}
	return result
}

// RegisterPlan adds a custom plan to the registry.
// The plan is cloned before storing so the registry is immutable from outside mutation.
func (r *Registry) RegisterPlan(plan *workflow.Plan) {
	if plan != nil && plan.PlanID != "" {
		r.plans[plan.PlanID] = clonePlan(plan)
	}
}

// PlanIDs returns all plan IDs.
func (r *Registry) PlanIDs() []string {
	ids := make([]string, 0, len(r.plans))
	for id := range r.plans {
		ids = append(ids, id)
	}
	return ids
}

// BuiltinPlanIDs returns the IDs of all built-in plans.
func BuiltinPlanIDs() []string {
	return []string{
		PlanRepaveJoin,
		PlanRMA,
		PlanReboot,
		PlanUpgrade,
		PlanNetReconfig,
	}
}
