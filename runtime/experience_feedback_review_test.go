package runtime

import (
	"context"
	"testing"
)

// P0-REVIEW-F01: the outcome A produced is still in the workspace. B adopts and its later run executes no
// tool, so nothing B did can have applied the strategy: the use is neither a success nor a
// counterexample.
func TestExperienceFeedbackWithoutExecutionIsNotSuccess(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	node, _ := expValidated(t, f, "outcome already present before the consumer runs")
	consumer := expNewSession(t, f, "B adopts and only answers")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, node.ID, node.Version)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	expMustRunSession(t, f, consumer, cogAnswer("B answered without executing anything"))
	if n := countRows(t, f.engine, `SELECT COUNT(*) FROM derived_origins WHERE session_id=? AND kind='execution'`, consumer); n != 0 {
		t.Fatalf("the consumer executed %d tools; the scenario needs none", n)
	}
	fed, err := f.engine.RecordExperienceFeedback(ctx, consumer, node.ID, adoption.ID)
	if err != nil {
		t.Fatalf("feedback: %v", err)
	}
	feedback := fed.Experience.Uses[0].Feedback
	if feedback == nil || feedback.Outcome != "unknown" || feedback.Used || feedback.Observation != nil ||
		feedback.ConditionsHeld || feedback.RevisionNeeded {
		t.Fatalf("feedback of a run that executed nothing = %+v", feedback)
	}
	if stats := fed.Experience.Statistics(); stats.Successes != 0 || stats.Failures != 0 || stats.Unknown != 1 {
		t.Fatalf("statistics = %+v, want one unknown use", stats)
	}
	if fed.Experience.State != node.Experience.State {
		t.Fatalf("state changed to %s without evidence", fed.Experience.State)
	}
	again, err := f.engine.RecordExperienceFeedback(ctx, consumer, node.ID, adoption.ID)
	if err != nil || again.Version != fed.Version {
		t.Fatalf("repeat feedback: version %d -> %d, err=%v", fed.Version, again.Version, err)
	}
}
