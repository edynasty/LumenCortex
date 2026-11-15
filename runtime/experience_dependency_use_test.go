package runtime

import (
	"context"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// ICRA-V-002. A belief that cites a validated Experience pins its revision. Every adoption and
// settlement of the shared record by any consumer commits a new revision, so an unrelated consumer's
// adoption made the dependent stale (dependency_changed), with nothing the dependent relied on changed.
// A counterexample still does.
func TestUnrelatedExperienceUseKeepsDependentCurrent(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	exp, _ := expConditionalValidated(t, f, "shared procedure cited by a belief")
	reader := expMustRun(t, f, "reader citing the procedure", cogAnswer("ok"))
	cognitiveMustPut(t, f.engine, reader, CognitiveNode{
		ID:           "icrav002-dependent",
		Kind:         "belief",
		Title:        "deployment note citing the shared procedure",
		Scope:        cognitiveMustScope(t, f.engine, reader, "project"),
		Dependencies: []knowledge.NodeRef{{ID: exp.ID, Version: exp.Version}},
	}, 0)
	dependent := func(when string) CognitiveAssessmentItem {
		t.Helper()
		got, err := f.engine.AssessCognition(ctx, reader, "", []string{"icrav002-dependent"})
		if err != nil {
			t.Fatalf("%s: %v", when, err)
		}
		for _, item := range got.Items {
			if item.ID == "icrav002-dependent" {
				return item
			}
		}
		t.Fatalf("%s: dependent missing from %+v", when, got.Items)
		return CognitiveAssessmentItem{}
	}
	if item := dependent("before any use"); item.Applicability == knowledge.Stale {
		t.Fatalf("fixture: dependent stale before any use: %+v", item)
	}

	bystander := expNewSession(t, f, "unrelated consumer that adopts and applies")
	adoption, err := f.engine.AdoptExperience(ctx, bystander, exp.ID, exp.Version)
	if err != nil {
		t.Fatal(err)
	}
	if item := dependent("after an unrelated adoption"); item.Applicability == knowledge.Stale {
		t.Errorf("an unrelated consumer's adoption made the dependent stale: %+v", item)
	}
	expApply(t, f, bystander, exp, adoption.ID)
	if item := dependent("after an unrelated settled application"); item.Applicability == knowledge.Stale {
		t.Errorf("an unrelated consumer's settled application made the dependent stale: %+v", item)
	}

	current, err := f.engine.ReadExperience(ctx, reader, exp.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	refuter := expNewSession(t, f, "consumer that refutes the procedure")
	refuting, err := f.engine.AdoptExperience(ctx, refuter, exp.ID, current.Version)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := expApplyCounterexample(t, f, refuter, exp, refuting.ID); err != nil || result.Status != "completed" {
		t.Fatalf("fixture: counterexample run %+v %v", result, err)
	}
	if item := dependent("after a counterexample"); item.Applicability != knowledge.Stale || item.Reason != "dependency_changed" {
		t.Errorf("a checked counterexample left the dependent %+v, want stale/dependency_changed", item)
	}
}

// ICRA-003 ownership (verification coverage). A belief that cites a validated Experience derives from
// the origin, not from the shared record's owner, which absorbs every consumer's use. Revoking an
// unrelated consumer fences the owner; it must not fence the dependent's session.
func TestUnrelatedConsumerRevocationDoesNotFenceExperienceDependent(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	exp, _ := expValidated(t, f, "shared procedure cited by a belief")
	reader := expMustRun(t, f, "reader citing the procedure", cogAnswer("ok"))
	cognitiveMustPut(t, f.engine, reader, CognitiveNode{
		ID:           "icra003-owner-dependent",
		Kind:         "belief",
		Title:        "deployment note citing the shared procedure",
		Scope:        cognitiveMustScope(t, f.engine, reader, "project"),
		Dependencies: []knowledge.NodeRef{{ID: exp.ID, Version: exp.Version}},
	}, 0)
	unrelated := expNewSession(t, f, "unrelated consumer later withdrawn")
	if _, err := f.engine.AdoptExperience(ctx, unrelated, exp.ID, exp.Version); err != nil {
		t.Fatal(err)
	}
	rev, err := f.engine.RevokeSource(ctx, SourceSession, unrelated, "withdrawn_consumer")
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rev.Sessions, reader) {
		t.Fatalf("revoking an unrelated consumer fenced the dependent's session: %+v", rev.Sessions)
	}
	if _, err := f.engine.ReadCognitiveNode(ctx, reader, "icra003-owner-dependent", 0); err != nil {
		t.Fatalf("dependent's own belief unreadable after an unrelated revocation: %v", err)
	}
}
