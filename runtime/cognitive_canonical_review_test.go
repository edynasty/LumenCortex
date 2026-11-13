package runtime

import (
	"context"
	"reflect"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
)

func TestReviewGenericNodeCannotForgeCanonicalBinding(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "maintain service beliefs", cogAnswer("observed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	assertion := portFact("8080")
	foreignOwner := f.run(t, "foreign observation", cogAnswer("separate"))
	foreign := cognitiveMustPut(t, f.engine, foreignOwner, CognitiveNode{ID: "foreign", Kind: "belief", Title: "service", Body: "same", Scope: scope, Assertion: &assertion}, 0)
	different := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "different", Kind: "belief", Title: "service", Body: "different", Scope: scope, Assertion: &assertion}, 0)
	for _, target := range []string{"missing", "self", foreign.ID, different.ID} {
		t.Run(target, func(t *testing.T) {
			original := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "alias-" + target, Kind: "belief", Title: "service", Body: "same", Scope: scope, Assertion: &assertion}, 0)
			if target == "self" {
				target = original.ID
			}
			n := original
			n.Status = "archived"
			n.Memory = &CognitiveMemoryPolicy{Canonical: &CognitiveNodeRef{ID: target, Version: 1}}
			if _, err := f.engine.PutCognitiveNode(ctx, owner, n, original.Version); err == nil {
				t.Fatal("generic proposal admitted an unchecked canonical binding")
			}
			got, err := f.engine.ReadCognitiveNode(ctx, owner, original.ID, 0)
			if err != nil || !reflect.DeepEqual(got, original) {
				t.Fatalf("refused binding changed history: %+v %v", got, err)
			}
		})
	}
}

func TestReviewCanonicalAliasCanBeRevokedAfterTargetChanges(t *testing.T) {
	for _, targetChange := range []string{"tier", "archive", "revoke"} {
		t.Run(targetChange, func(t *testing.T) {
			f := newCognitionFixture(t, false)
			ctx := context.Background()
			owner := f.run(t, "maintain service beliefs", cogAnswer("observed"))
			scope := cognitiveMustScope(t, f.engine, owner, "project")
			a := portFact("8080")
			canonical := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "canonical", Kind: "belief", Title: "service", Body: "same", Scope: scope, Assertion: &a}, 0)
			alias := cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "alias", Kind: "belief", Title: "service", Body: "same", Scope: scope, Assertion: &a}, 0)
			plan := GovernorPlan{Canonicalize: []cognition.CanonicalizePlan{{Canonical: canonical.ID, Aliases: []string{alias.ID}}}}
			applied, err := f.engine.GovernorApply(ctx, owner, plan, map[string]int64{canonical.ID: 1, alias.ID: 1}, "equivalent evidence")
			if err != nil || len(applied) != 1 {
				t.Fatalf("valid consolidation: %+v %v", applied, err)
			}
			archived := applied[0]
			changed := archived
			changed.Memory = nil
			changed.Body = "different claim"
			if _, err := f.engine.PutCognitiveNode(ctx, owner, changed, archived.Version); err == nil {
				t.Fatal("generic proposal rewrote governed alias")
			}
			if targetChange == "tier" {
				_, err = f.engine.GovernorApply(ctx, owner, GovernorPlan{Tiers: cognition.GovernorTiers{Cold: []string{canonical.ID}}}, map[string]int64{canonical.ID: 1}, "tier change")
			} else {
				_, err = f.engine.SetCognitiveLifecycle(ctx, owner, canonical.ID, 1, targetChange, "target changed")
			}
			if err != nil {
				t.Fatal(err)
			}
			revoked, err := f.engine.SetCognitiveLifecycle(ctx, owner, alias.ID, archived.Version, "revoke", "withdraw old alias")
			if err != nil || revoked.Status != "invalid" || !reflect.DeepEqual(revoked.Memory, archived.Memory) {
				t.Fatalf("historical canonical pin blocked revocation: %+v %v", revoked, err)
			}
			history, err := f.engine.ReadCognitiveNode(ctx, owner, alias.ID, archived.Version)
			if err != nil || !reflect.DeepEqual(history, archived) {
				t.Fatalf("old consolidation changed: %+v %v", history, err)
			}
			if _, err := f.engine.SetCognitiveLifecycle(ctx, owner, alias.ID, revoked.Version, "restore", "cannot revive"); err == nil {
				t.Fatal("revoked alias restored")
			}
		})
	}
}
