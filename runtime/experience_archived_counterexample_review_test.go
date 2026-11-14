package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

// expArchivedCounterexample builds the FFR-004 counterexample. A consumer adopts a validated
// Experience and its run applies it; the action removes the expected artifact. After the application
// settled and before the run ends, the host archives the still unprotected record. It returns the
// record as the consumer reads it after the run, the origin, the consumer and the run's memory report.
func expArchivedCounterexample(t *testing.T, f *cognitionFixture) (CognitiveNode, string, string, *CognitionCompletion) {
	t.Helper()
	ctx := context.Background()
	n, origin := expConditionalValidated(t, f, "archived before its counterexample settles")
	consumer := expNewSession(t, f, "consumer whose record is archived mid-run")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.root+"/build/block", "blocked")
	command := "if test -f build/block; then rm -f build/reused; else printf ready > build/reused; fi"
	p := &cognitionProvider{respond: func(step int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if step == 0 {
			raw, _ := json.Marshal(map[string]any{"id": n.ID, "adoptionId": adoption.ID, "actions": []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": command}}}})
			return cogCall("apply_experience", string(raw)), nil
		}
		if _, err := f.engine.SetCognitiveLifecycle(ctx, origin, n.ID, adoption.Version, "archive", "host archived a quiet record"); err != nil {
			t.Errorf("host archive: %v", err)
		}
		return cogFinal("procedure attempt finished"), nil
	}}
	result, err := f.engine.runAgentWith(ctx, consumer, p, AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	return got, origin, consumer, result.Cognition
}

// FFR-004. A host archives an unprotected Experience while a consumer's run has already applied it.
// The run's checked counterexample is real evidence about that record: archiving first must not make
// it impossible to record, must not leave the use open forever, and must not undo the host's decision.
func TestExperienceCounterexampleSettlesOnARecordArchivedMeanwhile(t *testing.T) {
	f := newCognitionFixture(t, true)
	got, _, consumer, report := expArchivedCounterexample(t, f)
	if report != nil && len(report.Errors) != 0 {
		t.Fatalf("the checked counterexample could not be recorded: %v", report.Errors)
	}
	fb := got.Experience.Uses[0].Feedback
	if fb == nil || !fb.Used || fb.Outcome != "failure" || !fb.RevisionNeeded || got.Experience.State != knowledge.ExperienceNeedsRevision {
		t.Fatalf("the counterexample was lost: %+v state=%s", fb, got.Experience.State)
	}
	if got.Status != "archived" {
		t.Fatalf("recording feedback changed the host's lifecycle decision: %s", got.Status)
	}
	pending, err := f.engine.store.PendingCognitiveSessions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range pending {
		if id == consumer {
			t.Fatal("the consumer stays queued for cognitive recovery")
		}
	}
}

// FFR-004. The settled counterexample makes the archived record protected. Protection refuses the
// act of archiving; it must not refuse a later host revision that leaves the record archived, and an
// archived warning must not be offered for reuse.
func TestExperienceArchivedCounterexampleStaysManageable(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	got, origin, _, _ := expArchivedCounterexample(t, f)
	if got.Status != "archived" || got.Experience.State != knowledge.ExperienceNeedsRevision {
		t.Fatalf("fixture: status=%s state=%s", got.Status, got.Experience.State)
	}
	current, err := f.engine.ReadCognitiveNode(ctx, origin, got.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	plan := GovernorPlan{}
	plan.Tiers.Cold = []string{got.ID}
	applied, err := f.engine.GovernorApply(ctx, origin, plan, map[string]int64{got.ID: current.Version}, "host demotes the archived warning")
	if err != nil {
		t.Fatalf("re-tiering an archived protected record: %v", err)
	}
	if len(applied) != 1 || applied[0].Status != "archived" || applied[0].Memory == nil || applied[0].Memory.Tier != "cold" {
		t.Fatalf("applied = %+v", applied)
	}
	if matches, err := f.engine.RecallExperiences(ctx, expNewSession(t, f, "late reader"), "archived before its counterexample settles", 5); err != nil || len(matches) != 0 {
		t.Fatalf("an archived record was recalled: %+v %v", matches, err)
	}
	if _, err := f.engine.AdoptExperience(ctx, expNewSession(t, f, "late adopter"), got.ID, applied[0].Version); err == nil {
		t.Fatal("an archived counterexample was adopted")
	}
	// Control: protection still refuses the act of archiving. Restored, the warning cannot be archived.
	restored, err := f.engine.SetCognitiveLifecycle(ctx, origin, got.ID, applied[0].Version, "restore", "host restores the warning")
	if err != nil || restored.Status != "active" {
		t.Fatalf("restore %+v %v", restored, err)
	}
	if _, err := f.engine.SetCognitiveLifecycle(ctx, origin, got.ID, restored.Version, "archive", "host tries to archive a counterexample"); err == nil {
		t.Fatal("an active counterexample was archived")
	}
}
