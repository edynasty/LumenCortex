package runtime

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
)

// A consumer derives from the origin whose strategy it adopted, not from the other consumers of the
// same Experience. Withdrawing one consumer must hide the record that holds its observations and
// nothing else: a co-consumer never received anything of it, so it is neither fenced nor reported as
// exposed, and an Experience it also uses stays available. Withdrawing an origin still reaches every
// consumer of its strategy.
func TestExperienceConsumerWithdrawalDoesNotFenceCoConsumers(t *testing.T) {
	for _, op := range []string{"revoke", "delete"} {
		t.Run(op, func(t *testing.T) {
			f := newCognitionFixture(t, false)
			ctx := context.Background()
			// One approval of the checks: approving them again would retire x's validation.
			expInstallChecks(t, f)
			validated := func(pattern string) (CognitiveNode, string) {
				t.Helper()
				origin := expOrigin(t, f, pattern)
				return expValidate(t, f, origin, expExtract(t, f, origin, expProposal(pattern))), origin
			}
			x, originX := validated("reach of experience x")
			y, originY := validated("reach of experience y")
			adopt := func(consumer string, n CognitiveNode) ExperienceAdoption {
				t.Helper()
				current, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, current.Version)
				if err != nil {
					t.Fatalf("adopt %s by %s: %v", n.ID, consumer, err)
				}
				return adoption
			}
			// shared adopts both before late exists; other only ever touches y.
			shared := expNewSession(t, f, "consumer of x and y")
			adopt(shared, x)
			adopt(shared, y)
			other := expNewSession(t, f, "consumer of y")
			adopt(other, y)
			late := expNewSession(t, f, "late consumer of x")
			adopt(late, x)

			var reached []string
			if op == "revoke" {
				report, err := f.engine.RevokeSource(ctx, SourceSession, late, "user_withdrawal")
				if err != nil {
					t.Fatal(err)
				}
				reached = report.Sessions
			} else if _, err := f.engine.DeleteSession(ctx, late, WorktreeCleanup{}); err != nil {
				t.Fatal(err)
			}

			for name, id := range map[string]string{"co-consumer": shared, "consumer of another experience": other, "origin x": originX, "origin y": originY} {
				if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, id); err != nil || withdrawn {
					t.Errorf("withdrawing one consumer fenced the %s: %v, %v", name, withdrawn, err)
				}
				if slices.Contains(reached, id) {
					t.Errorf("the exposure report names the %s, which received nothing of the withdrawn consumer", name)
				}
				if _, err := f.engine.GetContextGraph(ctx, id); err != nil {
					t.Errorf("context of the %s after the withdrawal: %v", name, err)
				}
			}
			// The record that holds the withdrawn consumer's observations is gone for every reader.
			for name, id := range map[string]string{"co-consumer": shared, "origin x": originX} {
				if _, err := f.engine.ReadExperience(ctx, id, x.ID, 0); !errors.Is(err, session.ErrFenced) && !errors.Is(err, session.ErrNotFound) {
					t.Errorf("experience x read by the %s after its consumer was withdrawn = %v", name, err)
				}
			}
			if matches, err := f.engine.RecallExperiences(ctx, shared, x.Experience.Definition.ProblemPattern, 4); err != nil || slices.ContainsFunc(matches, func(m ExperienceMatch) bool { return m.ID == x.ID }) {
				t.Errorf("experience x still recalled: %+v, %v", matches, err)
			}
			// The Experience the withdrawn consumer never touched keeps its record, uses and readers.
			for _, consumer := range []string{shared, other} {
				still, err := f.engine.ReadExperience(ctx, consumer, y.ID, 0)
				if err != nil || still.Experience == nil || len(still.Experience.Uses) != 1 || still.Experience.Uses[0].ConsumerID != consumer {
					t.Fatalf("experience y lost its private use after an unrelated withdrawal: %+v, %v", still.Experience, err)
				}
			}
			fresh := expNewSession(t, f, "later reader of y")
			if matches, err := f.engine.RecallExperiences(ctx, fresh, y.Experience.Definition.ProblemPattern, 4); err != nil || len(matches) != 1 || matches[0].ID != y.ID {
				t.Errorf("experience y recall after an unrelated withdrawal = %+v, %v", matches, err)
			}

			// Control: the origin's strategy did reach its consumers. Withdrawing it fences both.
			report, err := f.engine.RevokeSource(ctx, SourceSession, originY, "user_withdrawal")
			if err != nil {
				t.Fatal(err)
			}
			for name, id := range map[string]string{"shared": shared, "other": other} {
				if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, id); err != nil || !withdrawn || !slices.Contains(report.Sessions, id) {
					t.Errorf("withdrawing the origin of y did not reach its consumer %s: fenced=%v reported=%v err=%v", name, withdrawn, slices.Contains(report.Sessions, id), err)
				}
			}
			if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, originX); err != nil || withdrawn {
				t.Errorf("withdrawing the origin of y fenced the origin of x: %v, %v", withdrawn, err)
			}
			if _, err := f.engine.ReadExperience(ctx, fresh, y.ID, 0); !errors.Is(err, session.ErrFenced) {
				t.Errorf("experience y read after its origin was withdrawn = %v, want ErrFenced", err)
			}
		})
	}
}
