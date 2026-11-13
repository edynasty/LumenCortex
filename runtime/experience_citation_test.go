package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
)

func TestExperienceDeletedCitationDoesNotHideValidMatch(t *testing.T) {
	ctx := context.Background()
	f := newCognitionFixture(t, false)
	expInstallChecks(t, f)
	writeFile(t, filepath.Join(f.root, "rules.txt"), "Require the configured port before building.\n")
	origin := expOrigin(t, f, "citation origin")
	proposal := expProposal("recover the build marker")
	proposal.CitedPaths = []string{"rules.txt"}
	stale := expValidate(t, f, origin, expExtract(t, f, origin, proposal))
	valid := expValidate(t, f, origin, expExtract(t, f, origin, expProposal(proposal.ProblemPattern)))
	reader := expNewSession(t, f, "reader after citation deletion")
	if err := os.Remove(filepath.Join(f.root, "rules.txt")); err != nil {
		t.Fatal(err)
	}

	matches, err := f.engine.RecallExperiences(ctx, reader, proposal.ProblemPattern, 4)
	if err != nil || len(matches) != 1 || matches[0].ID != valid.ID {
		t.Fatalf("stale citation hid valid peer: matches=%+v err=%v", matches, err)
	}
	if _, err := f.engine.AdoptExperience(ctx, reader, stale.ID, stale.Version); !errors.Is(err, session.ErrSourceChanged) {
		t.Fatalf("adopting deleted citation: %v", err)
	}
	if _, err := f.engine.ValidateExperience(ctx, origin, stale.ID, stale.Version); !errors.Is(err, session.ErrSourceChanged) {
		t.Fatalf("validating deleted citation: %v", err)
	}
	latest, err := f.engine.ReadExperience(ctx, origin, stale.ID, 0)
	if err != nil || latest.Version != stale.Version || len(latest.Experience.Uses) != 0 {
		t.Fatalf("unavailable citation partially mutated history: %+v %v", latest, err)
	}
	if _, err := f.engine.ExtractExperience(ctx, origin, ExperienceExtraction{ScopeKind: "project", Proposal: proposal}); !errors.Is(err, session.ErrSourceChanged) {
		t.Fatalf("extraction accepted unavailable citation: %v", err)
	}
}
