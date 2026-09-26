package cognition

import (
	"errors"
	"testing"
	"time"
)

func TestRouterRaisesEffortForRepeatedFailure(t *testing.T) {
	router := Router{}
	plan := router.Route(Input{
		Goal: "Debug a cross-module transaction deadlock",
		Progress: Progress{MaxRepeatedFailure: 4, NoProgressSteps: 3},
	})
	if !plan.Think {
		t.Fatal("expected Think")
	}
	if plan.Effort != EffortMax {
		t.Fatalf("expected max effort, got %s", plan.Effort)
	}
	if plan.Category != "deep" && plan.Category != "ultrabrain" {
		t.Fatalf("unexpected category %q", plan.Category)
	}
}

func TestRouterCanUseAdvisoryCategorySignal(t *testing.T) {
	plan := (Router{}).Route(Input{
		Goal: "normal task",
		Signals: Signals{Category: "research", CategoryConfidence: 0.91},
	})
	if plan.Category != "research" {
		t.Fatalf("expected advisory research category, got %q", plan.Category)
	}
}

func TestResolveChainPreservesOrderAndHealth(t *testing.T) {
	profile := DefaultProfile()
	profile.Categories["deep"] = Category{Models: []string{"model-a", "model-b", "model-c"}}
	chain, err := ResolveChain(profile, "deep", func(model string) bool {
		return model != "model-a"
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[0] != "model-b" || chain[1] != "model-c" {
		t.Fatalf("unexpected chain %#v", chain)
	}
}

func TestHealthRegistryCircuitBreakerAndProbe(t *testing.T) {
	now := time.Unix(100, 0)
	registry := NewHealthRegistry(HealthConfig{FailureThreshold: 2, Cooldown: 5 * time.Second})
	registry.SetClock(func() time.Time { return now })
	registry.Failure("decision:jev", errors.New("down"))
	if !registry.Available("decision:jev") {
		t.Fatal("circuit opened too early")
	}
	registry.Failure("decision:jev", errors.New("down"))
	if registry.Available("decision:jev") {
		t.Fatal("expected open circuit")
	}
	now = now.Add(6 * time.Second)
	if !registry.Available("decision:jev") {
		t.Fatal("expected route eligible for cooldown probe")
	}
	registry.Success("decision:jev")
	if !registry.Available("decision:jev") {
		t.Fatal("expected healthy route after success")
	}
}

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
			Verification: []string{"reproduced"},
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


func TestRouterConstrainsModelRetrievalByConfidenceAndRelationApplicability(t *testing.T) {
	router := Router{
		RetrievalConfidenceThreshold: 0.68,
		ExpensiveRetrievalConfidenceThreshold: 0.82,
	}

	migration := router.Route(Input{
		Goal: "Run the database schema migration safely in production",
		Signals: Signals{Retrieval: "dependency", RetrievalConfidence: 0.99},
	})
	if migration.Retrieval != "lexical" || !containsReason(migration.Reasons, "retrieval-model-constrained") {
		t.Fatalf("migration=%#v", migration)
	}

	explicit := router.Route(Input{
		Goal: "Inspect this symbol ownership relation",
		Signals: Signals{Retrieval: "dependency", RetrievalConfidence: 0.95},
	})
	if explicit.Retrieval != "dependency" {
		t.Fatalf("explicit dependency=%#v", explicit)
	}

	mediumHybrid := router.Route(Input{
		Goal: "Inspect this behavior",
		Signals: Signals{Retrieval: "hybrid", RetrievalConfidence: 0.8},
	})
	if mediumHybrid.Retrieval != "lexical" {
		t.Fatalf("medium hybrid=%#v", mediumHybrid)
	}

	highHybrid := router.Route(Input{
		Goal: "Inspect this behavior",
		Signals: Signals{Retrieval: "hybrid", RetrievalConfidence: 0.9},
	})
	if highHybrid.Retrieval != "hybrid" {
		t.Fatalf("high hybrid=%#v", highHybrid)
	}
}

func TestRouterAllowsCausalModelRetrievalAfterRepeatedFailure(t *testing.T) {
	plan := (Router{}).Route(Input{
		Goal: "Try again",
		Progress: Progress{MaxRepeatedFailure: 3},
		Signals: Signals{Retrieval: "causal", RetrievalConfidence: 0.95},
	})
	if plan.Retrieval != "causal" {
		t.Fatalf("plan=%#v", plan)
	}
}
