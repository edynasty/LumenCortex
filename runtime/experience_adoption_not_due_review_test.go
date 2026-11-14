package runtime

import (
	"context"
	"testing"
)

// FFR-006. A host adopts an Experience for a consumer whose previous run is terminal. Until that
// consumer runs again nothing can have been applied or observed: the use is open, not failed. The
// memory settlement of an Engine open must not report it as a recovery failure, and it must still
// settle the use once a later run has ended. A host that asks for feedback early is still refused.
func TestExperienceHostAdoptionAwaitingItsRunIsNotARecoveryFailure(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, _ := expValidated(t, f, "build marker procedure")
	consumer := expMustRun(t, f, "consumer that already finished a run", cogAnswer("done"))
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	events, cancel := f.engine.Events(64)
	defer cancel()
	for pass := 1; pass <= 2; pass++ {
		if err := f.engine.recoverCognitiveRuns(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for drained := false; !drained; {
		select {
		case event := <-events:
			if event.Type == "cognition.recovery.failed" {
				t.Fatalf("an adoption that is not due yet is reported as a recovery failure: %s", event.Data)
			}
		default:
			drained = true
		}
	}
	open, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(open.Experience.Uses) != 1 || open.Experience.Uses[0].Feedback != nil || open.Version != adoption.Version {
		t.Fatalf("the open use was changed by recovery: %+v", open.Experience.Uses)
	}
	if _, err := f.engine.RecordExperienceFeedback(ctx, consumer, n.ID, adoption.ID); err == nil {
		t.Fatal("a host recorded feedback before any later consumer run")
	}
	// The later run ends: the use is due and settles (nothing was applied, so the outcome is unknown).
	result, err := f.engine.runAgentWith(ctx, consumer, cogAnswer("finished without applying"), AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("later run %+v %v", result, err)
	}
	if result.Cognition == nil || result.Cognition.Feedback != 1 || len(result.Cognition.Errors) != 0 {
		t.Fatalf("the due use did not settle: %+v", result.Cognition)
	}
	settled, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fb := settled.Experience.Uses[0].Feedback; fb == nil || fb.Used || fb.Outcome != "unknown" {
		t.Fatalf("feedback = %+v", fb)
	}
}
