package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

// FFR-003. The origin thread continues after a consumer adopted its experience: the record's source
// version no longer holds, so it can gain no new adoption. The consumer's open use must still reach a
// settled state; a use that can never be closed fails every later memory settlement of the consumer
// and keeps it queued for recovery at every Engine open.
func TestExperienceOpenUseSettlesAfterTheOriginThreadContinues(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, origin := expValidated(t, f, "build marker procedure")
	consumer := expNewSession(t, f, "adopt before the origin continues")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	// The origin thread gets one more ordinary turn. Nothing about the consumer changed.
	expMustRunSession(t, f, origin, cogAnswer("an unrelated follow-up answer in the origin thread"))
	if _, err := f.engine.AdoptExperience(ctx, expNewSession(t, f, "late adopter"), n.ID, adoption.Version); err == nil {
		t.Fatal("control: a record whose source version changed must not gain a new adoption")
	}
	for attempt := 1; attempt <= 2; attempt++ {
		next, err := f.engine.runAgentWith(ctx, consumer, cogAnswer("consumer finished without applying"), AgentOptions{MaxSteps: 6, Policy: "workspace"})
		if err != nil || next.Status != "completed" {
			t.Fatalf("consumer run %d = %+v %v", attempt, next, err)
		}
		if next.Cognition != nil && len(next.Cognition.Errors) != 0 {
			t.Fatalf("consumer run %d: the open use keeps failing memory settlement: %v", attempt, next.Cognition.Errors)
		}
	}
	pending, err := f.engine.store.PendingCognitiveSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range pending {
		if id == consumer {
			t.Fatal("the consumer stays queued for cognitive recovery forever")
		}
	}
	got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	fb := got.Experience.Uses[0].Feedback
	if fb == nil || fb.Used || fb.Outcome != "unknown" {
		t.Fatalf("the open use did not settle as unknown: %+v", fb)
	}
	if got.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("settling an unapplied use changed the experience state: %s", got.Experience.State)
	}
}

// FFR-003. An application admitted while the record was current is the consumer's own observation.
// The origin thread taking another turn before the consumer's run ends does not unsay it: the checked
// outcome is recorded once, and the record still gains no new adoption.
func TestExperienceCheckedOutcomeIsRecordedAfterTheOriginThreadContinues(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, origin := expValidated(t, f, "build marker procedure")
	consumer := expNewSession(t, f, "apply while the origin continues")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	p := &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			raw, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": adoption.ID, "actions": []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}})
			return cogCall("apply_experience", string(raw)), nil
		}
		// The application settled. The origin thread now takes one more ordinary turn.
		if result, err := f.engine.runAgentWith(ctx, origin, cogAnswer("an unrelated follow-up answer in the origin thread"), AgentOptions{MaxSteps: 6, Policy: "workspace"}); err != nil || result.Status != "completed" {
			t.Errorf("origin follow-up = %+v %v", result, err)
		}
		return cogFinal("procedure attempt finished"), nil
	}}
	result, err := f.engine.runAgentWith(ctx, consumer, p, AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	if result.Cognition == nil || result.Cognition.Feedback != 1 || len(result.Cognition.Errors) != 0 {
		t.Fatalf("the checked outcome was not recorded: %+v", result.Cognition)
	}
	got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	fb := got.Experience.Uses[0].Feedback
	if fb == nil || !fb.Used || fb.Outcome != "success" || fb.Observation == nil || fb.Observation.Evidence.Verification != "passed" {
		t.Fatalf("feedback = %+v", fb)
	}
	if _, err := f.engine.AdoptExperience(ctx, expNewSession(t, f, "late adopter"), n.ID, got.Version); err == nil {
		t.Fatal("recording feedback made a record with a changed source adoptable")
	}
	if matches, err := f.engine.RecallExperiences(ctx, expNewSession(t, f, "late reader"), "build marker procedure", 5); err != nil || len(matches) != 0 {
		t.Fatalf("a record with a changed source was recalled: %+v %v", matches, err)
	}
}
