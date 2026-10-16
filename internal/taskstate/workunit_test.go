package taskstate

import "testing"

func TestWorkUnitRejectsRoutingFields(t *testing.T) {
	_, err := ParseWorkUnits([]byte(`[{"id":"a","goal":"x","provider":"openrouter"}]`))
	if err == nil {
		t.Fatal("expected provider field rejection")
	}
}

func TestWorkUnitCompletionGateAndDependencyOrder(t *testing.T) {
	manager := NewWorkUnitManager(nil)
	if err := manager.Seed([]WorkUnit{
		{
			ID: "inspect", Goal: "Inspect root cause",
			RequiredEvidence: []string{"production path"},
			Verification:     []string{"reproduced"},
		},
		{
			ID: "repair", Goal: "Repair bug", DependsOn: []string{"inspect"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	active, err := manager.EnsureActive()
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != "inspect" {
		t.Fatalf("unexpected active unit %#v", active)
	}
	completed := WorkCompleted
	if _, err := manager.Update("inspect", WorkUnitPatch{Status: &completed}); err == nil {
		t.Fatal("expected evidence completion gate")
	}
	evidence := []EvidenceRef{{Requirement: "production path", Ref: "service.go:42"}}
	verify := []VerificationResult{{Check: "reproduced", Status: "passed"}}
	if _, err := manager.Update("inspect", WorkUnitPatch{
		Evidence: &evidence, VerificationResults: &verify, Status: &completed,
	}); err != nil {
		t.Fatal(err)
	}
	active, err = manager.EnsureActive()
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.ID != "repair" {
		t.Fatalf("expected repair to activate, got %#v", active)
	}
}

func TestWorkUnitDependencyCycle(t *testing.T) {
	manager := NewWorkUnitManager(nil)
	err := manager.Seed([]WorkUnit{
		{ID: "a", Goal: "A", DependsOn: []string{"b"}},
		{ID: "b", Goal: "B", DependsOn: []string{"a"}},
	})
	if err == nil {
		t.Fatal("expected dependency cycle")
	}
}
