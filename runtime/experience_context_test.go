package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
)

func TestExperienceContextKeepsLatestOwnerConsumerAndFences(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, false)
	n, origin := expValidated(t, f, "projected experience")
	consumer := expNewSession(t, f, "adopted reader")
	unrelated := expNewSession(t, f, "unrelated reader")
	adopted, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{origin, consumer} {
		view, err := f.engine.GetContextGraph(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		found, ok := expContextNode(view, "experience:"+n.ID)
		if !ok || found.SourceRef != "cognition:"+n.ID+"@"+itoa(adopted.Version) || found.Freshness != "requires_condition_check" {
			t.Fatalf("reader %s lost current experience locator: %+v present=%v", id, found, ok)
		}
		count := 0
		for _, node := range view.Nodes {
			if node.ID == found.ID {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("historical revisions produced %d duplicate locators", count)
		}
	}
	view, err := f.engine.GetContextGraph(ctx, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := expContextNode(view, "experience:"+n.ID); ok {
		t.Fatalf("unadopted reader exposed Experience: %+v", got)
	}
	if _, err := f.engine.RevokeSource(ctx, SourceSession, origin, "context_projection_test"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.GetContextGraph(ctx, consumer); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("revoked consumer read remained available: %v", err)
	}
	view, err = f.engine.GetContextGraph(ctx, unrelated)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := expContextNode(view, "experience:"+n.ID); ok {
		t.Fatalf("revoked Experience leaked to unrelated reader: %+v", got)
	}
}
