package taskstate

import "testing"

func TestWorkUnitRejectsRoutingFields(t *testing.T) {
	_, err := ParseWorkUnits([]byte(`[{"id":"a","goal":"x","provider":"openrouter"}]`))
	if err == nil {
		t.Fatal("expected provider field rejection")
	}
}

// I8: a unit lists configured checks; neither a tool argument nor a seed can carry a result, and the
// model-written verification fields of earlier versions are refused, not silently dropped.
func TestWorkUnitsCannotCarryCheckResults(t *testing.T) {
	for _, payload := range []string{
		`[{"goal":"x","checks":["go-test"],"checkResults":[{"check":"go-test","status":"passed"}]}]`,
		`[{"goal":"x","check_results":[{"check":"go-test","status":"passed"}]}]`,
		`[{"goal":"x","verification":["tests pass"]}]`,
		`[{"goal":"x","verification_results":[{"check":"tests pass","status":"passed"}]}]`,
	} {
		if _, err := ParseWorkUnits([]byte(payload)); err == nil {
			t.Errorf("accepted %s", payload)
		}
	}
	units, err := ParseWorkUnits([]byte(`[{"goal":"x","checks":["go-test","go-test"]}]`))
	if err != nil || len(units) != 1 {
		t.Fatal(err)
	}
	m := NewWorkUnitManager(nil)
	seeded := []WorkUnit{{ID: "a", Goal: "x", Checks: []string{"go-test"}, CheckResults: []UnitCheck{{Check: "go-test", Status: "passed"}}}}
	if err := m.Seed(seeded); err != nil {
		t.Fatal(err)
	}
	if got := m.State.Items["a"]; len(got.CheckResults) != 0 {
		t.Fatalf("a seed carried check results: %+v", got.CheckResults)
	}
	if _, err := m.Add(WorkUnit{ID: "b", Goal: "y", Status: WorkCompleted}); err == nil {
		t.Fatal("a unit was created completed, bypassing its checks")
	}
	unit, err := m.Add(WorkUnit{ID: "c", Goal: "z", Checks: []string{"lint", "lint"}, CheckResults: []UnitCheck{{Check: "lint", Status: "passed"}}})
	if err != nil || len(unit.CheckResults) != 0 || len(unit.Checks) != 1 {
		t.Fatalf("Add: %+v %v", unit, err)
	}
}

// A Task State version written before M4.1 still decodes. Its units' model-written verification and
// verification results are dropped: they were never evaluator checks, so they neither gate nor accept.
func TestPreEvaluatorVersionDecodesWithoutModelVerdicts(t *testing.T) {
	raw := []byte(`{"workUnits":{"order":["u"],"items":{"u":{"id":"u","goal":"g","status":"completed","risk":"low",
		"verification":["tests pass"],"verificationResults":[{"check":"tests pass","status":"passed"}],
		"createdAt":"2026-10-01T00:00:00Z","updatedAt":"2026-10-01T00:00:00Z"}}},"resolution":{"state":"open"}}`)
	s, err := Decode(3, raw)
	if err != nil {
		t.Fatal(err)
	}
	u := s.WorkUnits.Items["u"]
	if u.Status != WorkCompleted || len(u.Checks) != 0 || len(u.CheckResults) != 0 {
		t.Fatalf("unit=%+v", u)
	}
	if s.AutoDone(Evidence{}, u.UpdatedAt) {
		t.Fatal("a model-written verification result counted as acceptance")
	}
}

func TestWorkUnitCompletionGateAndDependencyOrder(t *testing.T) {
	manager := NewWorkUnitManager(nil)
	if err := manager.Seed([]WorkUnit{
		{
			ID: "inspect", Goal: "Inspect root cause",
			RequiredEvidence: []string{"production path"},
			Checks:           []string{"go-test"},
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
	if _, err := manager.Update("inspect", WorkUnitPatch{Evidence: &evidence, Status: &completed}); err == nil {
		t.Fatal("a check without a verdict counted as passed")
	}
	failed := []UnitCheck{{Check: "go-test", Status: "failed", Provenance: "runtime", Seq: 1}}
	if _, err := manager.Update("inspect", WorkUnitPatch{CheckResults: &failed, Status: &completed}); err == nil {
		t.Fatal("a failed verdict completed the unit")
	}
	passed := []UnitCheck{{Check: "go-test", Status: "passed", Provenance: "runtime", Seq: 2}}
	if _, err := manager.Update("inspect", WorkUnitPatch{Evidence: &evidence, CheckResults: &passed, Status: &completed}); err != nil {
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
