package plans

import (
	"testing"

	pb "github.com/vpatelsj/stargate/gen/baremetal/v1"
)

func TestNewRegistry(t *testing.T) {
	r := NewRegistry()

	// Should have all built-in plans
	builtinIDs := BuiltinPlanIDs()
	for _, id := range builtinIDs {
		if _, ok := r.GetPlan(id); !ok {
			t.Errorf("Expected built-in plan %q to exist", id)
		}
	}
}

func TestGetPlan(t *testing.T) {
	r := NewRegistry()

	tests := []struct {
		planID string
		exists bool
	}{
		{PlanRepaveJoin, true},
		{PlanRMA, true},
		{PlanReboot, true},
		{PlanUpgrade, true},
		{PlanNetReconfig, true},
		{"nonexistent", false},
		{"", false},
	}

	for _, tt := range tests {
		plan, ok := r.GetPlan(tt.planID)
		if ok != tt.exists {
			t.Errorf("GetPlan(%q): got exists=%v, want %v", tt.planID, ok, tt.exists)
		}
		if tt.exists && plan.PlanId != tt.planID {
			t.Errorf("GetPlan(%q): got plan ID %q", tt.planID, plan.PlanId)
		}
	}
}

func TestListPlans(t *testing.T) {
	r := NewRegistry()

	plans := r.ListPlans()
	if len(plans) != len(BuiltinPlanIDs()) {
		t.Errorf("Expected %d plans, got %d", len(BuiltinPlanIDs()), len(plans))
	}

	// Verify all plans have IDs and display names
	for _, plan := range plans {
		if plan.PlanId == "" {
			t.Error("Plan has empty ID")
		}
		if plan.DisplayName == "" {
			t.Errorf("Plan %q has empty display name", plan.PlanId)
		}
		if len(plan.Steps) == 0 {
			t.Errorf("Plan %q has no steps", plan.PlanId)
		}
	}
}

func TestRegisterPlan(t *testing.T) {
	r := NewRegistry()

	customPlan := &pb.Plan{
		PlanId:      "custom/my-plan",
		DisplayName: "My Custom Plan",
		Steps: []*pb.Step{
			{Name: "step1", Kind: &pb.Step_Reboot{Reboot: &pb.Reboot{}}},
		},
	}

	r.RegisterPlan(customPlan)

	got, ok := r.GetPlan("custom/my-plan")
	if !ok {
		t.Fatal("Custom plan not found after registration")
	}
	if got.DisplayName != "My Custom Plan" {
		t.Errorf("Expected 'My Custom Plan', got %q", got.DisplayName)
	}

	// Verify list includes custom plan
	plans := r.ListPlans()
	if len(plans) != len(BuiltinPlanIDs())+1 {
		t.Errorf("Expected %d plans after adding custom, got %d", len(BuiltinPlanIDs())+1, len(plans))
	}
}

func TestRegisterPlan_NilAndEmpty(t *testing.T) {
	r := NewRegistry()
	initialCount := len(r.ListPlans())

	// Nil plan should be ignored
	r.RegisterPlan(nil)
	if len(r.ListPlans()) != initialCount {
		t.Error("Nil plan should not be registered")
	}

	// Empty ID plan should be ignored
	r.RegisterPlan(&pb.Plan{PlanId: ""})
	if len(r.ListPlans()) != initialCount {
		t.Error("Plan with empty ID should not be registered")
	}
}

func TestPlanRepaveJoin_Steps(t *testing.T) {
	r := NewRegistry()
	plan, ok := r.GetPlan(PlanRepaveJoin)
	if !ok {
		t.Fatal("PlanRepaveJoin not found")
	}

	expectedSteps := []string{
		"set-netboot",
		"reboot-to-netboot",
		"repave-image",
		"join-cluster",
		"verify-in-cluster",
	}

	if len(plan.Steps) != len(expectedSteps) {
		t.Fatalf("Expected %d steps, got %d", len(expectedSteps), len(plan.Steps))
	}

	for i, expected := range expectedSteps {
		if plan.Steps[i].Name != expected {
			t.Errorf("Step %d: expected %q, got %q", i, expected, plan.Steps[i].Name)
		}
	}

	// Verify step types
	if _, ok := plan.Steps[0].Kind.(*pb.Step_Netboot); !ok {
		t.Error("Step 0 should be SetNetboot")
	}
	if _, ok := plan.Steps[1].Kind.(*pb.Step_Reboot); !ok {
		t.Error("Step 1 should be Reboot")
	}
	if _, ok := plan.Steps[2].Kind.(*pb.Step_Repave); !ok {
		t.Error("Step 2 should be RepaveImage")
	}
	if _, ok := plan.Steps[3].Kind.(*pb.Step_Join); !ok {
		t.Error("Step 3 should be KubeadmJoin")
	}
	if _, ok := plan.Steps[4].Kind.(*pb.Step_Verify); !ok {
		t.Error("Step 4 should be VerifyInCluster")
	}
}

func TestPlanRMA_Steps(t *testing.T) {
	r := NewRegistry()
	plan, ok := r.GetPlan(PlanRMA)
	if !ok {
		t.Fatal("PlanRMA not found")
	}

	if len(plan.Steps) != 3 {
		t.Fatalf("Expected 3 steps, got %d", len(plan.Steps))
	}

	// First step should be SSH drain check
	if ssh, ok := plan.Steps[0].Kind.(*pb.Step_Ssh); !ok {
		t.Error("Step 0 should be SSH command")
	} else if ssh.Ssh.ScriptRef != "drain_check.sh" {
		t.Errorf("Expected drain_check.sh, got %q", ssh.Ssh.ScriptRef)
	}

	// Second step should be Reboot
	if _, ok := plan.Steps[1].Kind.(*pb.Step_Reboot); !ok {
		t.Error("Step 1 should be Reboot")
	}
}

func TestPlanReboot_Steps(t *testing.T) {
	r := NewRegistry()
	plan, ok := r.GetPlan(PlanReboot)
	if !ok {
		t.Fatal("PlanReboot not found")
	}

	if len(plan.Steps) != 1 {
		t.Fatalf("Expected 1 step, got %d", len(plan.Steps))
	}

	if _, ok := plan.Steps[0].Kind.(*pb.Step_Reboot); !ok {
		t.Error("Step should be Reboot")
	}
}

func TestPlanUpgrade_Steps(t *testing.T) {
	r := NewRegistry()
	plan, ok := r.GetPlan(PlanUpgrade)
	if !ok {
		t.Fatal("PlanUpgrade not found")
	}

	// Should have: cordon, drain, upgrade, restart, uncordon, verify
	if len(plan.Steps) < 4 {
		t.Errorf("Expected at least 4 steps for upgrade, got %d", len(plan.Steps))
	}

	// Find upgrade step
	found := false
	for _, step := range plan.Steps {
		if ssh, ok := step.Kind.(*pb.Step_Ssh); ok {
			if ssh.Ssh.ScriptRef == "upgrade_k8s.sh" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("Expected upgrade_k8s.sh script in upgrade plan")
	}
}

func TestPlanNetReconfig_Steps(t *testing.T) {
	r := NewRegistry()
	plan, ok := r.GetPlan(PlanNetReconfig)
	if !ok {
		t.Fatal("PlanNetReconfig not found")
	}

	if len(plan.Steps) < 1 {
		t.Fatal("Expected at least 1 step")
	}

	// First step should be NetReconfig
	if _, ok := plan.Steps[0].Kind.(*pb.Step_Net); !ok {
		t.Error("Step 0 should be NetReconfig")
	}
}

func TestPlanIDs(t *testing.T) {
	r := NewRegistry()
	ids := r.PlanIDs()

	if len(ids) != len(BuiltinPlanIDs()) {
		t.Errorf("Expected %d IDs, got %d", len(BuiltinPlanIDs()), len(ids))
	}

	// Verify all built-in IDs are present
	idSet := make(map[string]bool)
	for _, id := range ids {
		idSet[id] = true
	}
	for _, id := range BuiltinPlanIDs() {
		if !idSet[id] {
			t.Errorf("Missing plan ID: %q", id)
		}
	}
}

func TestAllPlansHaveTimeouts(t *testing.T) {
	r := NewRegistry()

	for _, plan := range r.ListPlans() {
		for _, step := range plan.Steps {
			if step.TimeoutSeconds <= 0 {
				t.Errorf("Plan %q step %q has no timeout", plan.PlanId, step.Name)
			}
		}
	}
}

// TestGetPlanReturnsClone verifies that GetPlan returns a deep clone,
// so callers cannot mutate the registry's shared global plans.
func TestGetPlanReturnsClone(t *testing.T) {
	r := NewRegistry()

	// Get a plan
	plan1, ok := r.GetPlan(PlanRepaveJoin)
	if !ok {
		t.Fatal("PlanRepaveJoin not found")
	}

	// Remember original step name
	originalName := plan1.Steps[0].Name

	// Mutate the returned plan
	plan1.DisplayName = "MUTATED"
	plan1.Steps[0].Name = "MUTATED-STEP"

	// Get the plan again - it should have original values
	plan2, ok := r.GetPlan(PlanRepaveJoin)
	if !ok {
		t.Fatal("PlanRepaveJoin not found on second get")
	}

	if plan2.DisplayName == "MUTATED" {
		t.Error("Caller was able to mutate registry's plan DisplayName - GetPlan should return a clone")
	}

	if plan2.Steps[0].Name != originalName {
		t.Errorf("Caller was able to mutate registry's plan Steps - GetPlan should return a clone. Expected %q, got %q",
			originalName, plan2.Steps[0].Name)
	}
}

// TestListPlansReturnsClones verifies that ListPlans returns deep clones,
// so callers cannot mutate the registry's shared global plans.
func TestListPlansReturnsClones(t *testing.T) {
	r := NewRegistry()

	plans1 := r.ListPlans()
	if len(plans1) == 0 {
		t.Fatal("Expected at least one plan")
	}

	// Remember original values
	originalDisplayName := plans1[0].DisplayName
	originalPlanId := plans1[0].PlanId

	// Mutate the returned plans
	plans1[0].DisplayName = "MUTATED"

	// Get plans again - they should have original values
	plans2 := r.ListPlans()

	// Find the same plan
	var found *pb.Plan
	for _, p := range plans2 {
		if p.PlanId == originalPlanId {
			found = p
			break
		}
	}

	if found == nil {
		t.Fatal("Could not find original plan")
	}

	if found.DisplayName != originalDisplayName {
		t.Errorf("Caller was able to mutate registry's plan via ListPlans - should return clones. Expected %q, got %q",
			originalDisplayName, found.DisplayName)
	}
}
