package runtime

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/session"
)

// Review of Governor reachability on the normal Go SQLite store. A host follows the whole path through
// the public Engine API: Analyze/Plan/Validate, an owned authorized lifecycle batch, restore, revoke
// and privacy deletion. The deterministic plan covers every owner of the project and carries advisory
// proposals, so the host selects the part one owner may authorize: ownership is the SessionID a host
// read returns, and an advisory proposal is refused rather than silently dropped.
func TestGovernorHostLifecyclePathIsReachableOnSQLite(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "first owner", cogAnswer("first"))
	other := f.run(t, "second owner", cogAnswer("second"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	now := time.Now().UTC()
	past := CognitiveTimes{Valid: CognitiveValidTime{From: now.Add(-48 * time.Hour), Until: now.Add(-24 * time.Hour)}}
	port := portFact("8080")
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "expired", Kind: "belief", Title: "Expired yesterday", Scope: scope, Times: past}, 0)
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "fresh", Kind: "belief", Title: "Fresh belief", Scope: scope}, 0)
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "warning", Kind: "negative", Title: "Expired warning", Scope: scope, Times: past}, 0)
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "dup-a", Kind: "belief", Title: "service port", Body: "same", Scope: scope, Assertion: &port}, 0)
	cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "dup-b", Kind: "belief", Title: "service port", Body: "same", Scope: scope, Assertion: &port}, 0)
	cognitiveMustPut(t, f.engine, other, CognitiveNode{ID: "other-expired", Kind: "belief", Title: "Other expired", Scope: cognitiveMustScope(t, f.engine, other, "project"), Times: past}, 0)

	options := cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5}
	analysis, err := f.engine.GovernorAnalyze(ctx, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := cognitiveArchiveIDs(analysis.Candidates.Archive); !reflect.DeepEqual(got, []string{"expired", "other-expired"}) {
		t.Fatalf("archive candidates = %v; a protected warning or a current belief was proposed", got)
	}
	if len(analysis.Candidates.Canonicalize) != 1 || !reflect.DeepEqual(analysis.Candidates.Canonicalize[0].NodeIDs, []string{"dup-a", "dup-b"}) {
		t.Fatalf("canonicalize candidates = %+v", analysis.Candidates.Canonicalize)
	}
	planned, err := f.engine.GovernorPlan(ctx, nil, options)
	if err != nil || !planned.Validation.Valid {
		t.Fatalf("plan %+v %v", planned.Validation, err)
	}
	version := func(id string) int64 {
		t.Helper()
		n, err := f.engine.ReadCognitiveNode(ctx, "", id, 0)
		if err != nil {
			t.Fatal(err)
		}
		return n.Version
	}
	// The plan as produced is not an authorization: it proposes an epoch and names both owners.
	if planned.Plan.Epoch == nil || !planned.Plan.Epoch.Proposed {
		t.Fatalf("fixture: no advisory epoch in %+v", planned.Plan)
	}
	all := map[string]int64{}
	for _, id := range slices.Concat(planned.Plan.Tiers.Hot, planned.Plan.Tiers.Warm, planned.Plan.Tiers.Cold) {
		all[id] = version(id)
	}
	if _, err := f.engine.GovernorApply(ctx, owner, planned.Plan, all, "plan as produced"); err == nil {
		t.Fatal("an advisory epoch proposal was accepted as an authorized change")
	}
	// The host selects what this owner may authorize.
	owned, expected := GovernorPlan{}, map[string]int64{}
	mine := func(ids []string) []string {
		var out []string
		for _, id := range ids {
			n, err := f.engine.ReadCognitiveNode(ctx, "", id, 0)
			if err != nil {
				t.Fatal(err)
			}
			if n.SessionID == owner {
				out, expected[id] = append(out, id), n.Version
			}
		}
		return out
	}
	owned.Archive = mine(planned.Plan.Archive)
	owned.Tiers = cognition.GovernorTiers{Hot: mine(planned.Plan.Tiers.Hot), Warm: mine(planned.Plan.Tiers.Warm), Cold: mine(planned.Plan.Tiers.Cold)}
	if validation, err := f.engine.GovernorValidate(ctx, owned); err != nil || !validation.Valid {
		t.Fatalf("owned plan %+v %v", validation, err)
	}
	applied, err := f.engine.GovernorApply(ctx, owner, owned, expected, "owned lifecycle batch")
	if err != nil || len(applied) != 5 {
		t.Fatalf("owned apply: %d revisions, %v", len(applied), err)
	}
	for _, n := range applied {
		wantStatus := "active"
		if n.ID == "expired" {
			wantStatus = "archived"
		}
		if n.Status != wantStatus || n.Version != 2 || n.Memory == nil || n.Memory.Tier == "" {
			t.Fatalf("applied %s: status=%s version=%d memory=%+v", n.ID, n.Status, n.Version, n.Memory)
		}
	}
	// Another owner's record is refused and the batch that names it commits nothing.
	if _, err := f.engine.GovernorApply(ctx, owner, GovernorPlan{Archive: []string{"other-expired"}, Tiers: cognition.GovernorTiers{Cold: []string{"dup-a"}}},
		map[string]int64{"other-expired": 1, "dup-a": 2}, "foreign record"); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("foreign record in an owned batch: %v", err)
	}
	if version("dup-a") != 2 || version("other-expired") != 1 {
		t.Fatal("a refused batch was partially committed")
	}
	// A protected warning is never archived, by a plan or by a direct host operation.
	if _, err := f.engine.GovernorApply(ctx, owner, GovernorPlan{Archive: []string{"warning"}}, map[string]int64{"warning": 2}, "protected"); err == nil {
		t.Fatal("a protected warning was archived by a plan")
	}
	if _, err := f.engine.SetCognitiveLifecycle(ctx, owner, "warning", 2, "archive", "protected"); err == nil {
		t.Fatal("a protected warning was archived by the host")
	}
	// Restore reassesses time: an expired record stays archived; a current one comes back.
	if _, err := f.engine.SetCognitiveLifecycle(ctx, owner, "expired", 2, "restore", "restore an expired record"); !errors.Is(err, session.ErrNotQualified) {
		t.Fatalf("an expired record was restored: %v", err)
	}
	archived, err := f.engine.SetCognitiveLifecycle(ctx, owner, "fresh", 2, "archive", "host archives")
	if err != nil || archived.Status != "archived" {
		t.Fatalf("archive %+v %v", archived.Status, err)
	}
	restored, err := f.engine.SetCognitiveLifecycle(ctx, owner, "fresh", archived.Version, "restore", "host restores")
	if err != nil || restored.Status != "active" {
		t.Fatalf("restore %+v %v", restored.Status, err)
	}
	// Revocation is stamped by Core and cannot be undone for the same identity.
	revoked, err := f.engine.SetCognitiveLifecycle(ctx, owner, "fresh", restored.Version, "revoke", "host revokes")
	if err != nil || revoked.Status != "invalid" || !revoked.Times.InvalidatedAt.Equal(revoked.Times.RecordedAt) {
		t.Fatalf("revoke %+v %v", revoked, err)
	}
	if _, err := f.engine.SetCognitiveLifecycle(ctx, owner, "fresh", revoked.Version, "restore", "revive"); err == nil {
		t.Fatal("a revoked identity was restored")
	}
	// Equivalent evidence is consolidated onto an exact revision; history stays readable.
	consolidated, err := f.engine.GovernorApply(ctx, owner, GovernorPlan{Canonicalize: []cognition.CanonicalizePlan{{Canonical: "dup-a", Aliases: []string{"dup-b"}}}},
		map[string]int64{"dup-a": 2, "dup-b": 2}, "equivalent scoped observation")
	if err != nil || len(consolidated) != 1 || consolidated[0].Status != "archived" || consolidated[0].Memory.Canonical == nil || consolidated[0].Memory.Canonical.Version != 2 {
		t.Fatalf("consolidation %+v %v", consolidated, err)
	}
	if first, err := f.engine.ReadCognitiveNode(ctx, owner, "dup-b", 1); err != nil || first.Status != "active" {
		t.Fatalf("consolidation rewrote history: %+v %v", first.Status, err)
	}
	// Privacy deletion of the owner removes its records from every Governor view; the other owner's stay.
	if _, err := f.engine.DeleteSession(ctx, owner, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	graph, _, err := f.engine.CognitiveGraphSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 1 || graph.Nodes["other-expired"].ID == "" {
		t.Fatalf("after privacy deletion the Governor sees %d nodes", len(graph.Nodes))
	}
	if after, err := f.engine.GovernorPlan(ctx, nil, options); err != nil || !reflect.DeepEqual(after.Plan.Archive, []string{"other-expired"}) {
		t.Fatalf("plan after deletion %+v %v", after.Plan.Archive, err)
	}
	if applied, err := f.engine.GovernorApply(ctx, other, GovernorPlan{Archive: []string{"other-expired"}}, map[string]int64{"other-expired": 1}, "the other owner archives"); err != nil || len(applied) != 1 {
		t.Fatalf("the remaining owner could not apply: %v", err)
	}
}
