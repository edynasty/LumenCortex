package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// FFR-014. The owner of an Experience record is a session of the store: ListSessions returns it and
// DeleteSession deletes it like any thread. That deletion erased the record together with its owner,
// so no fenced session held the lineage of the identity (FFR-012) and the origin extracted the same
// identity again from the same episode: a procedure the host had invalidated, or a consumer had
// refuted, came back as a fresh validated record. The same held for the content-free owner a consumer
// deletion leaves behind, once the host deleted that too. The revocation of the owner hides the
// record and refuses that extraction; its deletion must not be weaker. The origin and the consumers
// are not withdrawn by it: the owner derives from them, not they from the owner.
func TestExperienceErasedWithItsOwnerIsNotExtractedAgain(t *testing.T) {
	validated := func(t *testing.T, f *cognitionFixture) (CognitiveNode, string, string) {
		t.Helper()
		n, origin := expValidated(t, f, "procedure the host only deletes")
		consumer := expNewSession(t, f, "consumer of a procedure the host only deletes")
		adoption, err := f.engine.AdoptExperience(context.Background(), consumer, n.ID, n.Version)
		if err != nil {
			t.Fatal(err)
		}
		expApply(t, f, consumer, n, adoption.ID)
		return n, origin, consumer
	}
	for _, tc := range []struct {
		name, pattern, withdrawal string
		record                    func(*testing.T, *cognitionFixture) (CognitiveNode, string, string)
	}{
		{"host invalidated then owner revoked", "procedure the host invalidates", "revoke", expHostInvalidated},
		{"host invalidated then owner deleted", "procedure the host invalidates", "delete", expHostInvalidated},
		{"refuted then owner deleted", "procedure a consumer refutes", "delete", expConsumerRefuted},
		{"validated then owner deleted", "procedure the host only deletes", "delete", validated},
		{"refuted, co-consumer deleted, then its emptied owner deleted", "procedure a consumer refutes", "delete after the consumer", expConsumerRefuted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCognitionFixture(t, true)
			ctx := context.Background()
			n, origin, consumer := tc.record(t, f)
			stored, err := f.engine.ReadExperience(ctx, origin, n.ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			owner := stored.SessionID
			if owner == origin || owner == consumer {
				t.Fatalf("fixture: the record is owned by %s", owner)
			}
			alive := []string{origin, consumer}
			if tc.withdrawal == "delete after the consumer" {
				if _, err := f.engine.DeleteSession(ctx, consumer, WorktreeCleanup{}); err != nil {
					t.Fatal(err)
				}
				alive = []string{origin}
			}
			if tc.withdrawal == "revoke" {
				_, err = f.engine.RevokeSource(ctx, SourceSession, owner, "user_withdrawal")
			} else {
				_, err = f.engine.DeleteSession(ctx, owner, WorktreeCleanup{})
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, id := range alive {
				if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, id); err != nil || withdrawn {
					t.Fatalf("withdrawing the owner withdrew %s: %v %v", id, withdrawn, err)
				}
			}

			extraction := ExperienceExtraction{ScopeKind: string(knowledge.ScopeProject), Proposal: expProposal(tc.pattern)}
			again, err := f.engine.ExtractExperience(ctx, origin, extraction)
			if !errors.Is(err, session.ErrFenced) {
				state := ""
				if again.Experience != nil {
					state = again.Experience.State
				}
				t.Fatalf("the withdrawn identity was extracted again as %s@%d (%s %s): %v", again.ID, again.Version, again.Status, state, err)
			}
			fresh := expNewSession(t, f, "consumer after the withdrawal")
			if matches, err := f.engine.RecallExperiences(ctx, fresh, tc.pattern, 8); err != nil || len(matches) != 0 {
				t.Fatalf("the withdrawn identity is recalled: %+v %v", matches, err)
			}
			if _, err := f.engine.ReadExperience(ctx, fresh, n.ID, 0); err == nil {
				t.Fatal("the withdrawn identity is readable")
			}

			// Another lesson of the same episode is another identity, with an owner of its own.
			other := expProposal("another lesson of the same episode")
			learned, err := f.engine.ExtractExperience(ctx, origin, ExperienceExtraction{ScopeKind: string(knowledge.ScopeProject), Proposal: other})
			if err != nil || learned.ID == n.ID || learned.SessionID == owner || learned.Experience.State != knowledge.ExperienceCandidate {
				t.Fatalf("the origin can no longer learn another lesson: %+v %v", learned, err)
			}
		})
	}
}
