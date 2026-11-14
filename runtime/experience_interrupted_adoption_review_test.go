package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/protocol"
)

// FFR-002. An adoption made by a tool inside a run is bound to that run. When the run ends without a
// terminal status (here: its step limit) the adoption can never be applied again, so the next
// terminal run of the session must settle it as unknown. It must not stay unconfirmed forever and
// fail every later memory settlement of the session.
func TestExperienceAdoptionOfAnInterruptedRunSettlesUnknown(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, _ := expValidated(t, f, "build marker procedure")
	consumer := expNewSession(t, f, "adopt and get interrupted")
	adopt := &cognitionProvider{respond: func(int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		raw, _ := json.Marshal(map[string]any{"id": n.ID, "version": n.Version})
		return cogCall("adopt_experience", string(raw)), nil
	}}
	first, _ := f.engine.runAgentWith(ctx, consumer, adopt, AgentOptions{MaxSteps: 1, Policy: "workspace"})
	if first.Status != "interrupted" {
		t.Fatalf("first run = %+v, want interrupted at its step limit", first)
	}
	held, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil || len(held.Experience.Uses) != 1 || held.Experience.Uses[0].RunID == "" || held.Experience.Uses[0].Feedback != nil {
		t.Fatalf("the interrupted run did not leave a run-bound open adoption: %+v %v", held.Experience, err)
	}
	for attempt := 1; attempt <= 2; attempt++ {
		next, err := f.engine.runAgentWith(ctx, consumer, cogAnswer("continued without applying"), AgentOptions{MaxSteps: 6, Policy: "workspace"})
		if err != nil || next.Status != "completed" {
			t.Fatalf("later run %d = %+v %v", attempt, next, err)
		}
		if next.Cognition != nil && len(next.Cognition.Errors) != 0 {
			t.Fatalf("later run %d: a dead adoption keeps failing memory settlement: %v", attempt, next.Cognition.Errors)
		}
	}
	got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	fb := got.Experience.Uses[0].Feedback
	if fb == nil || fb.Used || fb.Outcome != "unknown" || fb.Observation != nil {
		t.Fatalf("the dead adoption did not settle as unknown: %+v", fb)
	}
	if got.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("an unapplied adoption changed the experience: %s", got.Experience.State)
	}
	if stats := got.Experience.Statistics(); stats.Unconfirmed != 0 || stats.Unknown != 1 || stats.Successes != 0 || stats.Failures != 0 {
		t.Fatalf("statistics = %+v", stats)
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
}
