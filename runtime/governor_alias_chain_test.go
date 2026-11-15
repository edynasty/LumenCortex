package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
)

// ICRA-V-003. The lifecycle store refuses to bind an alias to a target that is itself an alias, and
// to an inactive target. GovernorValidate checked neither direction of that invariant: a canonical
// node that already holds aliases could be consolidated into another node, leaving its aliases pinned
// to an alias (GovernorApply committed the chain), and an archived canonical was reported valid.
func TestGovernorRefusesAliasChainsAndInactiveCanonical(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "governor alias chain owner", cogAnswer("done"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	port := portFact("8080")
	for _, id := range []string{"chain-a", "chain-b", "chain-x", "chain-d", "chain-e"} {
		cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: id, Kind: "belief", Title: "Service Port", Body: "same", Scope: scope, Assertion: &port}, 0)
	}
	if _, err := f.engine.GovernorApply(ctx, owner, GovernorPlan{
		Canonicalize: []cognition.CanonicalizePlan{{Canonical: "chain-a", Aliases: []string{"chain-b"}}},
		Archive:      []string{"chain-d"},
	}, map[string]int64{"chain-a": 1, "chain-b": 1, "chain-d": 1}, "consolidate b into a, archive d"); err != nil {
		t.Fatal(err)
	}

	chain := GovernorPlan{Canonicalize: []cognition.CanonicalizePlan{{Canonical: "chain-x", Aliases: []string{"chain-a"}}}}
	validation, err := f.engine.GovernorValidate(ctx, chain)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Valid || !strings.Contains(strings.Join(validation.Errors, "; "), "alias is the canonical node of other aliases: chain-a") {
		t.Errorf("GovernorValidate accepted consolidating a canonical node that holds aliases: %+v", validation)
	}
	if _, err := f.engine.GovernorApply(ctx, owner, chain, map[string]int64{"chain-x": 1, "chain-a": 1}, "chain a into x"); err == nil {
		graph, _, _ := f.engine.CognitiveGraphSnapshot(ctx)
		t.Errorf("GovernorApply committed an alias chain: b -> %v, a -> %v", graph.Nodes["chain-b"].Metadata["canonicalNodeId"], graph.Nodes["chain-a"].Metadata["canonicalNodeId"])
	}

	inPlan, err := f.engine.GovernorValidate(ctx, GovernorPlan{Canonicalize: []cognition.CanonicalizePlan{
		{Canonical: "chain-x", Aliases: []string{"chain-e"}},
		{Canonical: "chain-e", Aliases: []string{"chain-d"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if inPlan.Valid {
		t.Errorf("GovernorValidate accepted a plan that consolidates one of its own canonical nodes: %+v", inPlan)
	}

	archived, err := f.engine.GovernorValidate(ctx, GovernorPlan{Canonicalize: []cognition.CanonicalizePlan{{Canonical: "chain-d", Aliases: []string{"chain-e"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if archived.Valid || !strings.Contains(strings.Join(archived.Errors, "; "), "canonical node is not active: chain-d") {
		t.Errorf("GovernorValidate accepted an archived canonical node: %+v", archived)
	}

	// Re-stating an existing consolidation and consolidating a fresh duplicate into the canonical stay valid.
	again, err := f.engine.GovernorValidate(ctx, GovernorPlan{Canonicalize: []cognition.CanonicalizePlan{{Canonical: "chain-a", Aliases: []string{"chain-b", "chain-x"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Valid {
		t.Errorf("GovernorValidate refused a legitimate consolidation into an existing canonical: %+v", again.Errors)
	}
}
