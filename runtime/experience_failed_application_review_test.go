package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// expDivergedApplication builds the FFR-001 counterexample. The origin's command writes build/reused
// and completes; its artifact stays in the shared workspace. The consumer adopts the validated
// Experience, then the identical command exits non-zero without touching the artifact, and the
// approved outcome check still passes on the stale file. holdFeedback keeps the run's own memory
// settlement from recording feedback, so a test can drive the Store boundary itself.
func expDivergedApplication(t *testing.T, f *cognitionFixture, holdFeedback bool) (CognitiveNode, string, string) {
	t.Helper()
	ctx := context.Background()
	expInstallChecks(t, f)
	command := "if test -f build/block; then exit 1; else printf ready > build/reused; fi"
	origin := expNewSession(t, f, "procedure that leaves its artifact behind")
	writeFile(t, filepath.Join(f.root, "build/fixture"), "procedure workspace")
	result, err := f.engine.runAgentWith(ctx, origin, expCommandThenFinal(command, "observed"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("origin %+v %v", result, err)
	}
	n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("artifact left behind")))
	if n.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("origin experience not validated: %+v", n.Experience.Validation)
	}
	consumer := expNewSession(t, f, "reuse where the action fails")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, "build/block"), "blocked")
	if _, err := os.Stat(filepath.Join(f.root, "build/reused")); err != nil {
		t.Fatalf("the origin artifact must pre-exist for this counterexample: %v", err)
	}
	if holdFeedback {
		faultSQL(t, f.engine, `CREATE TRIGGER ffr001_hold BEFORE INSERT ON cognition_nodes WHEN NEW.kind='experience' AND json_type(NEW.json,'$.experience.uses[0].feedback')='object' BEGIN SELECT RAISE(ABORT,'feedback held by the test'); END`)
	}
	result, err = f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": command}}}), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	if holdFeedback {
		faultSQL(t, f.engine, `DROP TRIGGER ffr001_hold`)
	}
	receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adoption.ID)
	if err != nil || receipt.State != "failed" || receipt.Actions[0].Outcome != "failed" {
		t.Fatalf("the consumer action did not settle as a failed execution: %+v %v", receipt, err)
	}
	return n, consumer, adoption.ID
}

// FFR-001. A procedure whose action failed in the consumer cannot be credited with an outcome that
// already held before it ran. A passing outcome check on a pre-existing artifact is not a successful
// reuse; the use is recorded as applied with an unknown outcome and the Experience keeps its state.
func TestExperienceFailedApplicationIsNotCreditedWithPreexistingOutcome(t *testing.T) {
	f := newCognitionFixture(t, true)
	n, consumer, _ := expDivergedApplication(t, f, false)
	got, err := f.engine.ReadExperience(context.Background(), consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	fb := got.Experience.Uses[0].Feedback
	if fb == nil {
		t.Fatal("no feedback settled")
	}
	if fb.Outcome == "success" {
		t.Fatalf("a failed action was recorded as a successful reuse: %+v", fb)
	}
	if fb.Outcome != "unknown" || fb.RevisionNeeded || !fb.Used || fb.Observation == nil || fb.Reason != "application_diverged_from_origin" || got.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("a failed action with an unattributable outcome must stay unknown: %+v state=%s", fb, got.Experience.State)
	}
	if stats := got.Experience.Statistics(); stats.Successes != 0 || stats.Failures != 0 || stats.Unknown != 1 {
		t.Fatalf("statistics credited the failed action: %+v", stats)
	}
}

// FFR-001 at the Store boundary: the commit itself refuses a success whose receipt does not show the
// origin's procedure, whatever the caller decided.
func TestExperienceStoreRefusesSuccessOfADivergedApplication(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, consumer, adoptionID := expDivergedApplication(t, f, true)
	node, err := f.engine.readExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	u := &node.Experience.Uses[0]
	if u.ID != adoptionID || u.Feedback != nil {
		t.Fatalf("the held use is not open: %+v", u)
	}
	source, err := f.engine.store.ExperienceOrigin(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adoptionID)
	if err != nil || receipt.Outcome == nil || receipt.Outcome.Evidence.Verification != "passed" {
		t.Fatalf("the stale artifact must pass the outcome check of the application: %+v %v", receipt, err)
	}
	u.Feedback = &knowledge.ExperienceFeedback{Source: source.Source, Used: true, ConditionsHeld: true, Outcome: "success", Reason: "expected_outcome_verified", ApplicationID: receipt.ID,
		Observation: receipt.Outcome, At: f.engine.now()}
	if _, err := f.engine.commitExperienceRevision(ctx, consumer, node, node.Version); !errors.Is(err, session.ErrNotQualified) {
		t.Fatalf("the Store accepted a success for a failed action: %v", err)
	}
	settled, err := f.engine.RecordExperienceFeedback(ctx, consumer, n.ID, adoptionID)
	if err != nil {
		t.Fatal(err)
	}
	if fb := settled.Experience.Uses[0].Feedback; fb == nil || fb.Outcome != "unknown" || settled.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("the use did not settle as unknown: %+v state=%s", fb, settled.Experience.State)
	}
}
