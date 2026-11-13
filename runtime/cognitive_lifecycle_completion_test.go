package runtime

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

func TestCompletionGovernorConsolidatesOnlyEquivalentOwnedMemory(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "maintain service beliefs", cogAnswer("observed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	a := portFact("8080")
	first := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "canonical", Kind: "belief", Title: "service", Body: "same assertion", Scope: scope, Assertion: &a}, 0)
	alias := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "alias", Kind: "belief", Title: "service", Body: "same assertion", Scope: scope, Assertion: &a}, 0)
	plan := GovernorPlan{Canonicalize: []cognition.CanonicalizePlan{{Canonical: first.ID, Aliases: []string{alias.ID}}}}
	applied, err := f.engine.GovernorApply(ctx, owner, plan, map[string]int64{first.ID: 1, alias.ID: 1}, "equivalent scoped observation")
	if err != nil || len(applied) != 1 || applied[0].Status != "archived" || applied[0].Memory.Canonical.ID != first.ID {
		t.Fatalf("consolidation %+v %v", applied, err)
	}
	historical, err := f.engine.ReadCognitiveNode(ctx, owner, alias.ID, 1)
	if err != nil || !reflect.DeepEqual(historical, alias) {
		t.Fatalf("history changed %+v %v", historical, err)
	}
	if _, err := f.engine.GovernorApply(ctx, owner, plan, map[string]int64{first.ID: 1, alias.ID: 1}, "stale plan"); !errors.Is(err, session.ErrCognitiveRevision) {
		t.Fatalf("stale plan %v", err)
	}
	other := f.run(t, "unrelated owner", cogAnswer("separate"))
	foreign := cognitiveMustPut(t, f.engine, other, CognitiveNode{ID: "foreign", Kind: "belief", Title: "other", Scope: scope}, 0)
	before, err := f.engine.ReadCognitiveNode(ctx, owner, first.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.GovernorApply(ctx, owner, GovernorPlan{Archive: []string{first.ID, foreign.ID}}, map[string]int64{first.ID: 1, foreign.ID: 1}, "mixed owners"); err == nil {
		t.Fatal("mixed owner batch accepted")
	}
	after, err := f.engine.ReadCognitiveNode(ctx, owner, first.ID, 0)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed batch partially committed")
	}
}

func TestCompletionDependencyWithdrawalHidesDerivedMemory(t *testing.T) {
	for _, operation := range []string{"revoke", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newCognitionFixture(t, false)
			ctx := context.Background()
			source := f.run(t, "source premise", cogAnswer("source"))
			dependent := f.run(t, "derived conclusion", cogAnswer("derived"))
			reader := expNewSession(t, f, "independent reader")
			scope := cognitiveMustScope(t, f.engine, source, "project")
			premise := cognitiveMustPut(t, f.engine, source, CognitiveNode{ID: "premise", Kind: "belief", Title: "source", Scope: scope}, 0)
			derived := cognitiveMustPut(t, f.engine, dependent, CognitiveNode{ID: "derived", Kind: "belief", Title: "derived", Scope: scope, Dependencies: []CognitiveNodeRef{{ID: premise.ID, Version: premise.Version}}}, 0)
			if operation == "revoke" {
				if _, err := f.engine.RevokeSource(ctx, SourceSession, source, "source_withdrawn"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.engine.DeleteSession(ctx, source, WorktreeCleanup{}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.engine.ReadCognitiveNode(ctx, reader, derived.ID, 0); !errors.Is(err, session.ErrFenced) && !errors.Is(err, session.ErrNotFound) {
				t.Fatalf("withdrawn derived record visible: %v", err)
			}
			if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, reader); err != nil || withdrawn {
				t.Fatalf("unexposed reader fenced %v %v", withdrawn, err)
			}
		})
	}
}

func TestCompletionAssessmentCannotPromoteUnknownOrHideConflicts(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	owner := f.run(t, "service facts", cogAnswer("observed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	f.write(t, "port.schema.json", `{"type":"object","properties":{"port":{"const":"8080"}},"required":["port"]}`)
	f.write(t, "port.data.json", `{"port":"8080"}`)
	a := knowledge.Assertion{Kind: knowledge.AssertConfigValue, Target: "port.data.json", Key: "port", Value: "8080"}
	f.setChecks(t, `{"schema":{"kind":"schema","schema":"port.schema.json","files":["port.data.json"],"required":false}}`, map[string]knowledge.Assertion{"schema": a})
	n := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "fact", Kind: "belief", Title: "port", Scope: scope, Assertion: &a}, 0)
	n, err := f.engine.VerifyCognitiveNode(ctx, owner, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	assessed, err := f.engine.AssessCognition(ctx, owner, "port", []string{n.ID})
	if err != nil || assessed.Recommendation != "continue" || assessed.Items[0].Applicability != knowledge.Current {
		t.Fatalf("verified static assertion %+v %v", assessed, err)
	}
	unknown := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "unknown", Kind: "belief", Title: "unknown premise", Scope: scope, Epistemic: "unknown"}, 0)
	assessed, err = f.engine.AssessCognition(ctx, owner, "port", []string{unknown.ID})
	if err != nil || assessed.Recommendation == "continue" || assessed.Items[0].Applicability == knowledge.Current {
		t.Fatalf("unknown became fact %+v %v", assessed, err)
	}
}

func TestCompletionGovernorRejectsChangingCanonicalTargetInBatch(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "consolidate equivalent beliefs", cogAnswer("observed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	a := portFact("8080")
	first := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "canonical-batch", Kind: "belief", Title: "service", Scope: scope, Assertion: &a}, 0)
	alias := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "alias-batch", Kind: "belief", Title: "service", Scope: scope, Assertion: &a}, 0)
	plan := GovernorPlan{Archive: []string{first.ID}, Canonicalize: []cognition.CanonicalizePlan{{Canonical: first.ID, Aliases: []string{alias.ID}}}}
	if _, err := f.engine.GovernorApply(ctx, owner, plan, map[string]int64{first.ID: first.Version, alias.ID: alias.Version}, "contradictory lifecycle batch"); err == nil {
		t.Fatal("archived target accepted as an active canonical premise")
	}
	for _, want := range []CognitiveNode{first, alias} {
		got, err := f.engine.ReadCognitiveNode(ctx, owner, want.ID, 0)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("invalid batch changed %s: %+v %v", want.ID, got, err)
		}
	}
}

func TestCompletionNodeRevisionCannotBypassProtectedRetention(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "retain safety knowledge", cogAnswer("observed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	n := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "safety", Kind: "belief", Title: "important safety constraint", Scope: scope, Memory: &CognitiveMemoryPolicy{Retention: "safety"}}, 0)
	change := n
	change.Status, change.Memory = "archived", nil
	if _, err := f.engine.PutCognitiveNode(ctx, owner, change, n.Version); err == nil {
		t.Fatal("generic node revision bypassed protected memory retention")
	}
	got, err := f.engine.ReadCognitiveNode(ctx, owner, n.ID, 0)
	if err != nil || !reflect.DeepEqual(got, n) {
		t.Fatalf("refused archival changed protected history: %+v %v", got, err)
	}
}
