package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// FFR-012. An Experience identity names one origin episode and one definition. The privacy deletion
// of a consumer erases the whole combined record, and with it every invalidation and counterexample
// the record held. The origin could then extract the same identity again from the same episode and
// validate it: a procedure the host had invalidated, or a consumer had refuted, was recalled and
// adopted again as a fresh validated record. A deletion includes the revocation of the deleted
// session, and the revocation of a consumer hides the record and refuses that extraction; the
// deletion must not be weaker.
func TestExperienceErasedByConsumerWithdrawalIsNotExtractedAgain(t *testing.T) {
	hostInvalidates := func(t *testing.T, f *cognitionFixture) (CognitiveNode, string, string) {
		ctx := context.Background()
		n, origin := expValidated(t, f, "procedure the host invalidates")
		consumer := expNewSession(t, f, "consumer of a procedure the host invalidates")
		adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
		if err != nil {
			t.Fatal(err)
		}
		expApply(t, f, consumer, n, adoption.ID)
		current, err := f.engine.ReadExperience(ctx, origin, n.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		invalid, err := f.engine.SetCognitiveLifecycle(ctx, origin, n.ID, current.Version, "revoke", "the host found the procedure wrong")
		if err != nil || invalid.Status != "invalid" {
			t.Fatalf("fixture: host invalidation %+v %v", invalid, err)
		}
		return n, origin, consumer
	}
	consumerRefutes := func(t *testing.T, f *cognitionFixture) (CognitiveNode, string, string) {
		ctx := context.Background()
		n, origin := expConditionalValidated(t, f, "procedure a consumer refutes")
		if n.Experience.State != knowledge.ExperienceValidated {
			t.Fatalf("fixture: state %s", n.Experience.State)
		}
		// The withdrawn consumer only adopted; another consumer records the counterexample.
		bystander := expNewSession(t, f, "consumer that only adopted the refuted procedure")
		first, err := f.engine.AdoptExperience(ctx, bystander, n.ID, n.Version)
		if err != nil {
			t.Fatal(err)
		}
		refuter := expNewSession(t, f, "consumer that refutes the procedure")
		adoption, err := f.engine.AdoptExperience(ctx, refuter, n.ID, first.Version)
		if err != nil {
			t.Fatal(err)
		}
		if result, err := expApplyCounterexample(t, f, refuter, n, adoption.ID); err != nil || result.Status != "completed" {
			t.Fatalf("fixture: counterexample run %+v %v", result, err)
		}
		refuted, err := f.engine.ReadExperience(ctx, origin, n.ID, 0)
		if err != nil || refuted.Experience.State != knowledge.ExperienceNeedsRevision {
			t.Fatalf("fixture: the counterexample was not recorded: %+v %v", refuted.Experience, err)
		}
		return n, origin, bystander
	}
	for _, tc := range []struct {
		name, pattern, withdrawal string
		invalidate                func(*testing.T, *cognitionFixture) (CognitiveNode, string, string)
	}{
		{"host invalidated then consumer revoked", "procedure the host invalidates", "revoke", hostInvalidates},
		{"host invalidated then consumer deleted", "procedure the host invalidates", "delete", hostInvalidates},
		{"refuted then co-consumer deleted", "procedure a consumer refutes", "delete", consumerRefutes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCognitionFixture(t, true)
			ctx := context.Background()
			n, origin, withdrawn := tc.invalidate(t, f)
			extraction := ExperienceExtraction{ScopeKind: string(knowledge.ScopeProject), Proposal: expProposal(tc.pattern)}
			if same, err := f.engine.ExtractExperience(ctx, origin, extraction); err != nil || same.ID != n.ID || same.Version == 1 {
				t.Fatalf("fixture: the extraction does not name the stored identity: %s@%d %v", same.ID, same.Version, err)
			}
			var err error
			if tc.withdrawal == "revoke" {
				_, err = f.engine.RevokeSource(ctx, SourceSession, withdrawn, "user_withdrawal")
			} else {
				_, err = f.engine.DeleteSession(ctx, withdrawn, WorktreeCleanup{})
			}
			if err != nil {
				t.Fatal(err)
			}
			if fenced, err := f.engine.store.SessionWithdrawn(ctx, origin); err != nil || fenced {
				t.Fatalf("fixture: withdrawing a consumer withdrew the origin: %v %v", fenced, err)
			}

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

			// The origin is not withdrawn: a different lesson of the same episode is a different identity.
			other := expProposal("another lesson of the same episode")
			learned, err := f.engine.ExtractExperience(ctx, origin, ExperienceExtraction{ScopeKind: string(knowledge.ScopeProject), Proposal: other})
			if err != nil || learned.ID == n.ID || learned.Experience.State != knowledge.ExperienceCandidate {
				t.Fatalf("the origin can no longer learn another lesson: %+v %v", learned, err)
			}
		})
	}
}
