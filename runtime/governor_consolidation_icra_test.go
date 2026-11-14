package runtime

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
)

// ICRA-004. CognitiveGraphSnapshot projected n.Memory as a raw struct without setting the
// canonicalNodeId and storageTier metadata keys expected by ValidateGovernorPlan and the curator,
// while governorDuplicateTitleGroups continued counting archived/canonicalized aliases as unresolved
// duplicate groups. After GovernorApply consolidated duplicate pairs, GovernorAnalyze perpetually
// reported canonicalization-backlog (Epoch.Recommended=true), and GovernorValidate accepted plans
// promoting an alias to Canonical or re-aliasing it elsewhere.
func TestGovernorSnapshotProjectsCanonicalAndExcludesConsolidatedAliasesFromBacklog(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "governor consolidation owner", cogAnswer("done"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	port := portFact("8080")

	var canonicalize []cognition.CanonicalizePlan
	var hot []string
	tierExpected := map[string]int64{}
	aliasExpected := map[string]int64{}
	for i := 0; i < 5; i++ {
		canID := fmt.Sprintf("icra004-dup-%d-a", i)
		aliasID := fmt.Sprintf("icra004-dup-%d-b", i)
		title := fmt.Sprintf("Service Port Group %d", i)
		cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: canID, Kind: "belief", Title: title, Body: "same", Scope: scope, Assertion: &port}, 0)
		cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: aliasID, Kind: "belief", Title: strings.ToLower(title), Body: "same", Scope: scope, Assertion: &port}, 0)
		canonicalize = append(canonicalize, cognition.CanonicalizePlan{Canonical: canID, Aliases: []string{aliasID}})
		hot = append(hot, canID)
		tierExpected[canID] = 1
		aliasExpected[canID] = 2
		aliasExpected[aliasID] = 1
	}

	before, err := f.engine.GovernorAnalyze(ctx, cognition.GovernorAnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if before.Metrics.DuplicateGroupCount != 5 || !before.Epoch.Recommended {
		t.Fatalf("fixture before consolidation: duplicates=%d epoch=%+v", before.Metrics.DuplicateGroupCount, before.Epoch)
	}

	if _, err := f.engine.GovernorApply(ctx, owner, GovernorPlan{
		Tiers: cognition.GovernorTiers{Hot: hot},
	}, tierExpected, "tier canonical targets hot"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.engine.GovernorApply(ctx, owner, GovernorPlan{
		Canonicalize: canonicalize,
	}, aliasExpected, "consolidate duplicate beliefs"); err != nil {
		t.Fatal(err)
	}

	graph, storage, err := f.engine.CognitiveGraphSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := graph.Nodes["icra004-dup-0-b"].Metadata["canonicalNodeId"]; got != "icra004-dup-0-a" {
		t.Errorf("alias canonicalNodeId metadata = %#v, want \"icra004-dup-0-a\"", got)
	}
	if got := graph.Nodes["icra004-dup-0-a"].Metadata["storageTier"]; got != "hot" {
		t.Errorf("canonical storageTier metadata = %#v, want \"hot\"", got)
	}
	if got := storage["icra004-dup-0-a"].Tier; got != "hot" {
		t.Errorf("storage telemetry tier = %q, want \"hot\"", got)
	}

	after, err := f.engine.GovernorAnalyze(ctx, cognition.GovernorAnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Metrics.DuplicateGroupCount != 0 || len(after.Candidates.Canonicalize) != 0 {
		t.Errorf("consolidated aliases still counted as unresolved duplicates: count=%d candidates=%+v",
			after.Metrics.DuplicateGroupCount, after.Candidates.Canonicalize)
	}
	if after.Epoch.Recommended {
		t.Errorf("Epoch still recommended after resolving all duplicate groups: %+v", after.Epoch)
	}

	aliasAsCanonical, err := f.engine.GovernorValidate(ctx, GovernorPlan{
		Canonicalize: []cognition.CanonicalizePlan{{Canonical: "icra004-dup-0-b", Aliases: []string{"icra004-dup-1-a"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aliasAsCanonical.Valid || !strings.Contains(strings.Join(aliasAsCanonical.Errors, "; "), "canonical node is itself an alias") {
		t.Errorf("GovernorValidate accepted consolidated alias as Canonical: %+v", aliasAsCanonical)
	}

	reAliasElsewhere, err := f.engine.GovernorValidate(ctx, GovernorPlan{
		Canonicalize: []cognition.CanonicalizePlan{{Canonical: "icra004-dup-1-a", Aliases: []string{"icra004-dup-0-b"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reAliasElsewhere.Valid || !strings.Contains(strings.Join(reAliasElsewhere.Errors, "; "), "alias already canonicalized elsewhere") {
		t.Errorf("GovernorValidate accepted re-aliasing consolidated alias to another target: %+v", reAliasElsewhere)
	}
}
