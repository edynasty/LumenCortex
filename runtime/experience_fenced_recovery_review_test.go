package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
)

// FFR-010. A consumer whose last run is terminal holds an open use of an Experience whose origin is
// then withdrawn. The consumer is fenced: nothing of it can be settled until one of its Starts
// projects the fence, and a thread that never runs again stays that way. The memory settlement of an
// Engine open must not report it as a recovery failure on every open; the fence itself still refuses
// the consumer.
func TestExperienceFencedConsumerIsNotARecoveryFailure(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	n, origin := expValidated(t, f, "build marker procedure of a withdrawn origin")
	consumer := expMustRun(t, f, "consumer of a withdrawn origin that already finished a run", cogAnswer("done"))
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.RevokeSource(ctx, SourceSession, origin, "user_withdrawal"); err != nil {
		t.Fatal(err)
	}
	if fenced, err := f.engine.store.SessionFenced(ctx, consumer); err != nil || !fenced {
		t.Fatalf("fixture: the consumer of a withdrawn origin is not fenced: %v %v", fenced, err)
	}
	if pending, err := f.engine.store.PendingCognitiveSessions(ctx); err != nil || !slices.Contains(pending, consumer) {
		t.Fatalf("fixture: the open use is not pending: %v %v", pending, err)
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
				t.Fatalf("a fenced consumer is reported as a recovery failure: %s", event.Data)
			}
		default:
			drained = true
		}
	}
	if _, err := f.engine.RecordExperienceFeedback(ctx, consumer, n.ID, adoption.ID); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("the fence no longer refuses the consumer: %v", err)
	}
}
