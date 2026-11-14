package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// FFR-011. A record holds at most MaxExperienceUses uses for its whole life. Once they are spent it
// accepts no adoption: recall must not present it as adoptable after a condition check, and the
// refusal must name the bound instead of a condition that could change.
func TestExperienceWithSpentUsesIsNotRecalledAsAdoptable(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	const pattern = "build marker procedure with spent uses"
	n, _ := expValidated(t, f, pattern)
	fresh := expNewSession(t, f, "consumer before the first use")
	matches, err := f.engine.RecallExperiences(ctx, fresh, pattern, 8)
	if err != nil || len(matches) != 1 || matches[0].ID != n.ID || matches[0].Applicability != "requires_condition_check" {
		t.Fatalf("fixture: a record with free uses is not recalled as adoptable: %+v %v", matches, err)
	}
	version := n.Version
	for i := 0; i < knowledge.MaxExperienceUses; i++ {
		consumer := expNewSession(t, f, "consumer that spends a use")
		adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, version)
		if err != nil {
			t.Fatalf("adoption %d: %v", i+1, err)
		}
		version = adoption.Version
	}
	late := expNewSession(t, f, "consumer after the last use")
	matches, err = f.engine.RecallExperiences(ctx, late, pattern, 8)
	if err != nil || len(matches) != 1 || matches[0].ID != n.ID {
		t.Fatalf("recall after the last use: %+v %v", matches, err)
	}
	if matches[0].Applicability != "use_limit_reached" {
		t.Fatalf("a record that accepts no adoption is recalled as %q", matches[0].Applicability)
	}
	_, err = f.engine.AdoptExperience(ctx, late, n.ID, matches[0].Version)
	if !errors.Is(err, ErrExperienceNotApplicable) {
		t.Fatalf("adoption beyond the bound: %v", err)
	}
	if !strings.Contains(err.Error(), "uses") {
		t.Fatalf("the refusal does not name the use bound: %v", err)
	}
}
