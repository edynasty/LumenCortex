package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// FFR-016. The outcome of an application is what holds when its last action has settled. The
// outcome check ran when the consumer's run ended instead, or, for a settlement that was recovered,
// whenever it was made again, so whatever happened after the application was taken for a result of
// the procedure. A procedure that left its outcome unmet was recorded as a successful reuse once the
// consumer reached the outcome another way, and a procedure that worked was recorded as a
// counterexample, and withdrawn from every later consumer, once the consumer or the user cleaned up.

const expConditionalCommand = "if test -f build/block; then rm -f build/reused; else printf ready > build/reused; fi"

// expApplyThen applies the adopted procedure in the run's first step, makes the later calls and ends
// the run. applied receives the result of apply_experience as the model reads it.
func expApplyThen(n CognitiveNode, adoption string, actions []experienceAction, applied *string, later ...protocol.ProviderResponse) *cognitionProvider {
	return &cognitionProvider{respond: func(step int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			raw, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": adoption, "actions": actions})
			return cogCall("apply_experience", string(raw)), nil
		}
		if step == 1 {
			*applied = lastToolResult(req, "call_1")
		}
		if step <= len(later) {
			return later[step-1], nil
		}
		return cogFinal("done"), nil
	}}
}

func expSettledUse(t *testing.T, f *cognitionFixture, consumer, id string) (*knowledge.ExperienceFeedback, CognitiveNode) {
	t.Helper()
	got, err := f.engine.ReadExperience(context.Background(), consumer, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Experience.Uses) != 1 || got.Experience.Uses[0].Feedback == nil {
		t.Fatalf("the use was not settled: %+v", got.Experience.Uses)
	}
	return got.Experience.Uses[0].Feedback, got
}

// The procedure completes as it did in the origin but leaves the expected outcome unmet; the consumer
// then reaches the outcome with another tool. That is a counterexample of the procedure, not a reuse.
func TestExperienceOutcomeReachedAfterTheApplicationIsNotASuccess(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, _ := expConditionalValidated(t, f, "outcome reached another way")
	consumer := expNewSession(t, f, "consumer that reaches the outcome another way")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, "build/block"), "blocked")
	var applied string
	actions := []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": expConditionalCommand}}}
	result, err := f.engine.runAgentWith(ctx, consumer, expApplyThen(n, adoption.ID, actions, &applied, cogCall("write_file", `{"path":"build/reused","content":"ready"}`)), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "build/reused")); err != nil {
		t.Fatalf("fixture: the consumer's own step must leave the outcome in place: %v", err)
	}
	receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adoption.ID)
	if err != nil || receipt.State != "completed" || !receipt.Executed() {
		t.Fatalf("fixture: the application must repeat the origin's procedure: %+v %v", receipt, err)
	}
	fb, got := expSettledUse(t, f, consumer, n.ID)
	if fb.Outcome == "success" {
		t.Fatalf("an outcome the consumer produced after the application was credited to the procedure: %+v", fb)
	}
	if fb.Outcome != "failure" || fb.Reason != "outcome_counterexample" || !fb.RevisionNeeded || !fb.Used || fb.Observation == nil || fb.Observation.Evidence.Verification != "failed" ||
		got.Experience.State != knowledge.ExperienceNeedsRevision {
		t.Fatalf("the unmet outcome of the application is a counterexample: %+v state=%s", fb, got.Experience.State)
	}
	if stats := got.Experience.Statistics(); stats.Successes != 0 || stats.Failures != 1 {
		t.Fatalf("statistics = %+v", stats)
	}
	if !strings.Contains(applied, `"outcomeCheck":"not_held"`) {
		t.Fatalf("the run was not told that the procedure left its outcome unmet: %s", applied)
	}
}

// The procedure produces the expected outcome; a later step of the same run removes it. The procedure
// worked: the record is not contested and stays available to the next consumer.
func TestExperienceOutcomeRemovedAfterTheApplicationIsNotACounterexample(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, _ := expValidated(t, f, "outcome removed by a later step")
	consumer := expNewSession(t, f, "consumer that cleans up afterwards")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	var applied string
	actions := []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}
	result, err := f.engine.runAgentWith(ctx, consumer, expApplyThen(n, adoption.ID, actions, &applied, cogCall("bash", `{"command":"rm -f build/reused"}`)), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "build/reused")); !os.IsNotExist(err) {
		t.Fatalf("fixture: the consumer's later step must remove the outcome: %v", err)
	}
	fb, got := expSettledUse(t, f, consumer, n.ID)
	if fb.Outcome == "failure" || got.Experience.State == knowledge.ExperienceNeedsRevision {
		t.Fatalf("a procedure that produced its outcome was recorded as a counterexample: %+v state=%s", fb, got.Experience.State)
	}
	if fb.Outcome != "success" || fb.Reason != "expected_outcome_verified" || fb.RevisionNeeded || fb.Observation == nil || fb.Observation.Evidence.Verification != "passed" ||
		got.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("the outcome observed when the application ended is a success: %+v state=%s", fb, got.Experience.State)
	}
	if !strings.Contains(applied, `"outcomeCheck":"held"`) {
		t.Fatalf("the run was not told that the procedure produced its outcome: %s", applied)
	}
	next := expNewSession(t, f, "next consumer of the procedure")
	if _, err := f.engine.AdoptExperience(ctx, next, n.ID, got.Version); err != nil {
		t.Fatalf("the procedure was withdrawn from the next consumer: %v", err)
	}
}

// expHeldSettlement applies the procedure in a run whose own memory settlement cannot commit, as
// after a crash or a storage fault: the use stays open for a later settlement.
func expHeldSettlement(t *testing.T, f *cognitionFixture) (CognitiveNode, string, string) {
	t.Helper()
	ctx := context.Background()
	n, _ := expValidated(t, f, "outcome settled after the run")
	consumer := expNewSession(t, f, "consumer whose settlement is recovered")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	faultSQL(t, f.engine, `CREATE TRIGGER ffr016_hold BEFORE INSERT ON cognition_nodes WHEN NEW.kind='experience' AND json_type(NEW.json,'$.experience.uses[0].feedback')='object' BEGIN SELECT RAISE(ABORT,'feedback held by the test'); END`)
	actions := []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}
	result, err := f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, actions), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	faultSQL(t, f.engine, `DROP TRIGGER ffr016_hold`)
	if open, err := f.engine.readExperience(ctx, consumer, n.ID, 0); err != nil || open.Experience.Uses[0].Feedback != nil {
		t.Fatalf("fixture: the use must still be open: %+v %v", open.Experience, err)
	}
	return n, consumer, adoption.ID
}

// A settlement that is made after the run (the recovery pass of the next Engine open, a host call)
// records what the application left, not the workspace of that later moment.
func TestExperienceRecoveredSettlementKeepsTheOutcomeOfTheApplication(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, consumer, adoptionID := expHeldSettlement(t, f)
	if err := os.Remove(filepath.Join(f.root, "build/reused")); err != nil {
		t.Fatalf("fixture: the workspace is cleaned after the run: %v", err)
	}
	settled, err := f.engine.RecordExperienceFeedback(ctx, consumer, n.ID, adoptionID)
	if err != nil {
		t.Fatal(err)
	}
	fb := settled.Experience.Uses[0].Feedback
	if fb == nil || fb.Outcome == "failure" || settled.Experience.State == knowledge.ExperienceNeedsRevision {
		t.Fatalf("a workspace cleaned after the run was recorded as a counterexample: %+v state=%s", fb, settled.Experience.State)
	}
	if fb.Outcome != "success" || fb.Observation == nil || fb.Observation.Evidence.Verification != "passed" || settled.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("the recovered settlement lost the outcome of the application: %+v state=%s", fb, settled.Experience.State)
	}
}

// At the Store boundary: the outcome a used feedback cites is the observation of its application
// receipt. A check made later, whatever it shows, is refused as the outcome of the application.
func TestExperienceStoreRefusesALaterCheckAsTheOutcomeOfAnApplication(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, consumer, adoptionID := expHeldSettlement(t, f)
	node, err := f.engine.readExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	u := &node.Experience.Uses[0]
	source, err := f.engine.store.ExperienceOrigin(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adoptionID)
	if err != nil || receipt.State != "completed" || !receipt.Executed() {
		t.Fatalf("fixture: the application must repeat the origin's procedure: %+v %v", receipt, err)
	}
	later, err := f.engine.verifyAssertion(ctx, consumer, u.ExpectedOutcome, node.Scope, source.Source)
	if err != nil || later.Verification != "passed" {
		t.Fatalf("fixture: the later check must pass: %+v %v", later, err)
	}
	u.Feedback = &knowledge.ExperienceFeedback{Source: source.Source, Used: true, ConditionsHeld: true, Outcome: "success", Reason: "expected_outcome_verified", ApplicationID: receipt.ID,
		Observation: &knowledge.ExperienceObservation{Assertion: u.ExpectedOutcome, Evidence: later}, At: f.engine.now()}
	if _, err := f.engine.commitExperienceRevision(ctx, consumer, node, node.Version); !errors.Is(err, session.ErrNotQualified) {
		t.Fatalf("the Store accepted a later check as the outcome of the application: %v", err)
	}
}

// An observation whose approval was revoked before the settlement is no evidence any more, and a
// check made now cannot stand in for it: the use is settled as unknown instead of staying open.
func TestExperienceSettlementDoesNotReplaceAnOutcomeObservationThatLapsed(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, consumer, adoptionID := expHeldSettlement(t, f)
	binding := f.bind(t, expArtifactFact("present"), "reused")
	if _, err := f.engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeBinding, BindingDigest: binding.BindingDigest}); err != nil {
		t.Fatal(err)
	}
	settled, err := f.engine.RecordExperienceFeedback(ctx, consumer, n.ID, adoptionID)
	if err != nil {
		t.Fatalf("the use could not be settled after the revocation: %v", err)
	}
	fb := settled.Experience.Uses[0].Feedback
	if fb == nil || fb.Outcome != "unknown" || fb.Reason != "outcome_not_qualified" || !fb.Used || fb.Observation != nil || settled.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("a revoked observation was used, or replaced: %+v state=%s", fb, settled.Experience.State)
	}
}
