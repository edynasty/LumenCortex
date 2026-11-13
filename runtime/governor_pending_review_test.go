package runtime

import (
	"context"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
)

// P0-REVIEW-F02: a fact that becomes valid tomorrow is not stale and is never proposed for archiving; an
// expired one still is.
func TestGovernorKeepsNotYetValidKnowledgeOutOfArchive(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "seed", cogAnswer("seed"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	now := time.Now().UTC()
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "pending", Kind: "belief", Title: "Valid from tomorrow", Scope: scope,
		Times: CognitiveTimes{Valid: CognitiveValidTime{From: now.Add(24 * time.Hour)}}}, 0)
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "expired", Kind: "belief", Title: "Expired yesterday", Scope: scope,
		Times: CognitiveTimes{Valid: CognitiveValidTime{From: now.Add(-48 * time.Hour), Until: now.Add(-24 * time.Hour)}}}, 0)

	graph, _, err := f.engine.CognitiveGraphSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := graph.Nodes["pending"]; n.Status != "active" || n.Metadata["useLimitation"] != "not_yet_valid" {
		t.Fatalf("not-yet-valid projection: status=%s limitation=%v", n.Status, n.Metadata["useLimitation"])
	}
	if n := graph.Nodes["expired"]; n.Status != "stale" || n.Metadata["useLimitation"] != "expired" {
		t.Fatalf("expired projection: status=%s limitation=%v", n.Status, n.Metadata["useLimitation"])
	}
	options := cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5}
	analysis, err := f.engine.GovernorAnalyze(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := cognitiveArchiveIDs(analysis.Candidates.Archive); !reflect.DeepEqual(got, []string{"expired"}) || analysis.Metrics.StaleCount != 1 {
		t.Fatalf("archive candidates = %v, stale = %d; want [expired], 1", got, analysis.Metrics.StaleCount)
	}
	plan, err := f.engine.GovernorPlan(ctx, nil, options)
	if err != nil || slices.Contains(plan.Plan.Archive, "pending") {
		t.Fatalf("plan archives not-yet-valid knowledge: %v, %v", plan.Plan.Archive, err)
	}
}
