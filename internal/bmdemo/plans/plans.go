// Package plans provides built-in plan definitions for baremetal provisioning workflows.
package plans

import (
	"google.golang.org/protobuf/proto"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
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
var builtinPlans = map[string]*pb.Plan{
	PlanRepaveJoin: {
		PlanId:      PlanRepaveJoin,
		DisplayName: "Repave and Join Cluster",
		Steps: []*pb.Step{
			{
				Name:           "set-netboot",
				Kind:           &pb.Step_Netboot{Netboot: &pb.SetNetboot{Profile: "pxe-ubuntu-22.04"}},
				TimeoutSeconds: 60,
				MaxRetries:     2,
			},
			{
				Name:           "reboot-to-netboot",
				Kind:           &pb.Step_Reboot{Reboot: &pb.Reboot{Force: false}},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
			{
				Name: "repave-image",
				Kind: &pb.Step_Repave{Repave: &pb.RepaveImage{
					ImageRef:     "ubuntu:22.04-k8s",
					CloudInitRef: "cloud-init/worker-node",
				}},
				TimeoutSeconds: 600,
				MaxRetries:     1,
			},
			{
				Name:           "join-cluster",
				Kind:           &pb.Step_Join{Join: &pb.KubeadmJoin{}},
				TimeoutSeconds: 300,
				MaxRetries:     2,
			},
			{
				Name:           "verify-in-cluster",
				Kind:           &pb.Step_Verify{Verify: &pb.VerifyInCluster{}},
				TimeoutSeconds: 120,
				MaxRetries:     3,
			},
		},
	},

	PlanRMA: {
		PlanId:      PlanRMA,
		DisplayName: "Return Merchandise Authorization",
		Steps: []*pb.Step{
			{
				Name: "drain-check",
				Kind: &pb.Step_Ssh{Ssh: &pb.SshCommand{
					ScriptRef: "drain_check.sh",
					Args:      map[string]string{"force": "false"},
				}},
				TimeoutSeconds: 120,
				MaxRetries:     1,
			},
			{
				Name:           "graceful-shutdown",
				Kind:           &pb.Step_Reboot{Reboot: &pb.Reboot{Force: false}},
				TimeoutSeconds: 120,
				MaxRetries:     1,
			},
			{
				Name: "mark-rma",
				Kind: &pb.Step_Rma{Rma: &pb.RmaAction{
					Reason: "hardware failure",
				}},
				TimeoutSeconds: 60,
				MaxRetries:     1,
			},
		},
	},

	PlanReboot: {
		PlanId:      PlanReboot,
		DisplayName: "Simple Reboot",
		Steps: []*pb.Step{
			{
				Name:           "reboot",
				Kind:           &pb.Step_Reboot{Reboot: &pb.Reboot{Force: false}},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
		},
	},

	PlanUpgrade: {
		PlanId:      PlanUpgrade,
		DisplayName: "Kubernetes Upgrade",
		Steps: []*pb.Step{
			{
				Name: "cordon-node",
				Kind: &pb.Step_Ssh{Ssh: &pb.SshCommand{
					ScriptRef: "cordon_node.sh",
					Args:      map[string]string{},
				}},
				TimeoutSeconds: 60,
				MaxRetries:     2,
			},
			{
				Name: "drain-node",
				Kind: &pb.Step_Ssh{Ssh: &pb.SshCommand{
					ScriptRef: "drain_node.sh",
					Args:      map[string]string{"timeout": "300"},
				}},
				TimeoutSeconds: 600,
				MaxRetries:     1,
			},
			{
				Name: "upgrade-kubelet",
				Kind: &pb.Step_Ssh{Ssh: &pb.SshCommand{
					ScriptRef: "upgrade_k8s.sh",
					Args:      map[string]string{"version": "1.33.0"},
				}},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
			{
				Name:           "restart-kubelet",
				Kind:           &pb.Step_Reboot{Reboot: &pb.Reboot{Force: false}},
				TimeoutSeconds: 300,
				MaxRetries:     1,
			},
			{
				Name: "uncordon-node",
				Kind: &pb.Step_Ssh{Ssh: &pb.SshCommand{
					ScriptRef: "uncordon_node.sh",
					Args:      map[string]string{},
				}},
				TimeoutSeconds: 60,
				MaxRetries:     2,
			},
			{
				Name:           "verify-upgrade",
				Kind:           &pb.Step_Verify{Verify: &pb.VerifyInCluster{}},
				TimeoutSeconds: 120,
				MaxRetries:     3,
			},
		},
	},

	PlanNetReconfig: {
		PlanId:      PlanNetReconfig,
		DisplayName: "Network Reconfiguration",
		Steps: []*pb.Step{
			{
				Name: "apply-network-config",
				Kind: &pb.Step_Net{Net: &pb.NetReconfig{
					Params: map[string]string{
						"action": "reconfigure",
					},
				}},
				TimeoutSeconds: 120,
				MaxRetries:     2,
			},
			{
				Name: "verify-connectivity",
				Kind: &pb.Step_Ssh{Ssh: &pb.SshCommand{
					ScriptRef: "verify_network.sh",
					Args:      map[string]string{},
				}},
				TimeoutSeconds: 60,
				MaxRetries:     3,
			},
		},
	},
}

// Registry holds plan definitions and provides lookup.
type Registry struct {
	plans map[string]*pb.Plan
}

// NewRegistry creates a new plan registry with built-in plans.
func NewRegistry() *Registry {
	r := &Registry{
		plans: make(map[string]*pb.Plan),
	}
	// Copy built-in plans
	for id, plan := range builtinPlans {
		r.plans[id] = plan
	}
	return r
}

// clonePlan returns a deep copy of a plan to prevent callers from mutating shared state.
func clonePlan(p *pb.Plan) *pb.Plan {
	if p == nil {
		return nil
	}
	return proto.Clone(p).(*pb.Plan)
}

// GetPlan retrieves a plan by ID. Returns a deep clone so callers can't mutate registry state.
func (r *Registry) GetPlan(planID string) (*pb.Plan, bool) {
	plan, ok := r.plans[planID]
	if !ok {
		return nil, false
	}
	return clonePlan(plan), true
}

// ListPlans returns all available plans. Returns deep clones so callers can't mutate registry state.
func (r *Registry) ListPlans() []*pb.Plan {
	result := make([]*pb.Plan, 0, len(r.plans))
	for _, plan := range r.plans {
		result = append(result, clonePlan(plan))
	}
	return result
}

// RegisterPlan adds a custom plan to the registry.
func (r *Registry) RegisterPlan(plan *pb.Plan) {
	if plan != nil && plan.PlanId != "" {
		r.plans[plan.PlanId] = plan
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
