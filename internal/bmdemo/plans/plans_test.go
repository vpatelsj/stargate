package plans

import (
	"testing"

	"github.com/vpatelsj/stargate/internal/bmdemo/workflow"
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
		if tt.exists && plan.PlanID != tt.planID {
			t.Errorf("GetPlan(%q): got plan ID %q", tt.planID, plan.PlanID)
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
		if plan.PlanID == "" {
			t.Error("Plan has empty ID")
		}
		if plan.DisplayName == "" {
			t.Errorf("Plan %q has empty display name", plan.PlanID)
		}
		if len(plan.Steps) == 0 {
			t.Errorf("Plan %q has no steps", plan.PlanID)
		}
	}
}

func TestRegisterPlan(t *testing.T) {
	r := NewRegistry()

	customPlan := &workflow.Plan{
		PlanID:      "custom/my-plan",
		DisplayName: "My Custom Plan",
		Steps: []*workflow.Step{
			{Name: "step1", Kind: workflow.Reboot{}},
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
	r.RegisterPlan(&workflow.Plan{PlanID: ""})
	if len(r.ListPlans()) != initialCount {
		t.Error("Plan with empty ID should not be registered")
	}
}

// TestRegisterPlan_ClonesInput verifies that RegisterPlan clones the input plan,
// so mutations to the original do not affect the stored copy.
func TestRegisterPlan_ClonesInput(t *testing.T) {
	r := NewRegistry()

	// Create and register a custom plan
	original := &workflow.Plan{
		PlanID:      "custom/clone-test",
		DisplayName: "Original Name",
		Steps: []*workflow.Step{
			{Name: "step1", Kind: workflow.Reboot{}},
		},
	}
	r.RegisterPlan(original)

	// Mutate the original after registration
	original.DisplayName = "Mutated Name"

	// Verify the stored plan is not affected
	stored, ok := r.GetPlan("custom/clone-test")
	if !ok {
		t.Fatal("Plan not found")
	}
	if stored.DisplayName != "Original Name" {
		t.Errorf("Expected 'Original Name', got %q - registry should clone on insert", stored.DisplayName)
	}
}

// TestGetPlan_ReturnsClone verifies that GetPlan returns a clone,
// so mutations to the returned plan do not affect the stored copy.
func TestGetPlan_ReturnsClone(t *testing.T) {
	r := NewRegistry()

	// Get a built-in plan
	plan1, ok := r.GetPlan(PlanReboot)
	if !ok {
		t.Fatal("PlanReboot not found")
	}
	originalName := plan1.DisplayName

	// Mutate the returned plan
	plan1.DisplayName = "Mutated"

	// Get it again
	plan2, ok := r.GetPlan(PlanReboot)
	if !ok {
		t.Fatal("PlanReboot not found on second get")
	}

	if plan2.DisplayName != originalName {
		t.Errorf("Expected %q, got %q - GetPlan should return clone", originalName, plan2.DisplayName)
	}
}

func TestPlanIDs(t *testing.T) {
	r := NewRegistry()

	ids := r.PlanIDs()
	if len(ids) != len(BuiltinPlanIDs()) {
		t.Errorf("Expected %d plan IDs, got %d", len(BuiltinPlanIDs()), len(ids))
	}
}

func TestBuiltinPlanIDs(t *testing.T) {
	ids := BuiltinPlanIDs()

	expected := []string{
		PlanRepaveJoin,
		PlanRMA,
		PlanReboot,
		PlanUpgrade,
		PlanNetReconfig,
	}

	if len(ids) != len(expected) {
		t.Errorf("Expected %d builtin plan IDs, got %d", len(expected), len(ids))
	}

	for i, id := range expected {
		if ids[i] != id {
			t.Errorf("Expected ids[%d] = %q, got %q", i, id, ids[i])
		}
	}
}

func TestBuiltinPlanStepKinds(t *testing.T) {
	r := NewRegistry()

	// Verify PlanRepaveJoin has expected step kinds
	plan, ok := r.GetPlan(PlanRepaveJoin)
	if !ok {
		t.Fatal("PlanRepaveJoin not found")
	}

	if len(plan.Steps) < 5 {
		t.Errorf("Expected at least 5 steps in PlanRepaveJoin, got %d", len(plan.Steps))
	}

	// Verify step kinds
	for _, step := range plan.Steps {
		if step.Name == "" {
			t.Error("Step has empty name")
		}
		if step.Kind == nil {
			t.Errorf("Step %q has nil Kind", step.Name)
		}
	}
}
