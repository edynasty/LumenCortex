package runtime

import (
	"context"
	"testing"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// abortAutoStart leaves in the store exactly what a cancelled auto-effort Start leaves
// (TestCancelledAutoJudgeSettlesUnknownWithoutTouchingTheSession): one judge envelope of a new run
// id, settled unknown, and nothing else. No message, Task State, status or answer changes.
func abortAutoStart(t *testing.T, e *Engine, sessionID string) {
	t.Helper()
	runID, err := newRunID()
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := e.admitAutoJudge(context.Background(), sessionID, runID, autoTurn{})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.settleAutoJudge(requestID, session.RequestUnknown, agent.AutoReasonCancelled, nil); err != nil {
		t.Fatal(err)
	}
}

// A Start whose judge was cancelled before BeginRun ran nothing in the session: its source version
// is unchanged, so every source reference Core issued for it must still name the same run.
func TestCognitiveSourceRunIgnoresAnAbortedAutoStart(t *testing.T) {
	f := newCognitionFixture(t, false)
	n, origin := expValidated(t, f, "aborted judge keeps the origin run")
	ctx := context.Background()
	before, err := f.engine.store.CognitiveSource(ctx, origin)
	if err != nil {
		t.Fatal(err)
	}
	if before != n.Experience.Origin.Source {
		t.Fatalf("source references disagree before any change:\ngraph      %+v\nexperience %+v", before, n.Experience.Origin.Source)
	}

	abortAutoStart(t, f.engine, origin)

	after, err := f.engine.store.CognitiveSource(ctx, origin)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("a Start that ran nothing changed the source reference:\nbefore %+v\nafter  %+v", before, after)
	}
	consumer := expNewSession(t, f, "consume after the origin's aborted Start")
	matches, err := f.engine.RecallExperiences(ctx, consumer, "aborted judge keeps the origin run", 5)
	if err != nil || len(matches) != 1 || matches[0].ID != n.ID {
		t.Errorf("recall after an aborted Start of the origin = %+v, %v; want the validated experience", matches, err)
	}
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil || adoption.ExperienceID != n.ID {
		t.Fatalf("adoption after an aborted Start of the origin = %+v, %v", adoption, err)
	}
	again, err := f.engine.ValidateExperience(ctx, origin, n.ID, adoption.Version)
	if err != nil || again.Experience.State != knowledge.ExperienceValidated {
		t.Fatalf("revalidation after an aborted Start of the origin = %+v, %v", again.Experience, err)
	}
}

// The consumer baseline is issued by the same definition: an aborted Start before adoption must not
// move it, or the adoption would name a run that produced nothing.
func TestExperienceConsumerRunIgnoresAnAbortedAutoStart(t *testing.T) {
	f := newCognitionFixture(t, false)
	n, _ := expValidated(t, f, "aborted judge keeps the consumer baseline")
	ctx := context.Background()
	consumer := expMustRun(t, f, "consume around an aborted Start", cogAnswer("nothing to do yet"))
	before, err := f.engine.store.ExperienceConsumer(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := f.engine.store.CognitiveSource(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if graph != before {
		t.Fatalf("the two source references of one terminal session disagree:\ngraph      %+v\nexperience %+v", graph, before)
	}
	abortAutoStart(t, f.engine, consumer)
	after, err := f.engine.store.ExperienceConsumer(ctx, consumer)
	if err != nil {
		t.Fatal(err)
	}
	if after != before || after.RunID == "" {
		t.Fatalf("an aborted Start changed the consumer baseline:\nbefore %+v\nafter  %+v", before, after)
	}
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil || len(adoption.Conditions) != 1 {
		t.Fatalf("adoption after the consumer's aborted Start = %+v, %v", adoption, err)
	}
}

// The dirty row queued for a terminal source is the third issuer of the same reference, and the
// candidate a maintainer derives from it records that run as its origin. Revoking one source of a
// committed batch queues its co-batched sources again; a source whose last Start ran nothing must be
// queued under the run that produced its content.
func TestMaintenanceSourceRunIgnoresAnAbortedAutoStart(t *testing.T) {
	f, g, _, h, _ := requeueBatch(t)
	ctx := context.Background()
	want, err := f.engine.store.CognitiveSource(ctx, h)
	if err != nil || want.RunID == "" {
		t.Fatalf("terminal source reference = %+v, %v", want, err)
	}
	if f.pending(t, h) != 0 {
		t.Fatal("fixture: the committed source is still waiting")
	}

	abortAutoStart(t, f.engine, h)
	f.revoke(t, g)

	rows, err := f.engine.store.MaintenanceSources(ctx, 0, 1<<40, 64)
	if err != nil {
		t.Fatal(err)
	}
	var queued []knowledge.SourceRef
	for _, row := range rows {
		if row.Ref.SessionID == h {
			queued = append(queued, row.Ref)
		}
	}
	if len(queued) != 1 || queued[0] != want {
		t.Fatalf("the requeue after an aborted Start queued %+v, the store issues %+v", queued, want)
	}
}
