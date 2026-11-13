package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/migrate"
	"github.com/edynasty/LumenCortex/internal/session"
	_ "github.com/mattn/go-sqlite3"
)

// These tests exercise the native Go cognition graph through the host APIs a consumer can reach:
// Engine.Open, a scripted run, CognitiveScopeFor, Put/Verify/ReadCognitiveNode, PutCognitiveEdge and
// the three Governor reads. They never create the legacy Node graph tables. The only direct SQL is
// fault injection into rows first written by those APIs.

func TestCognitiveGraphUpgradeReopenAndRestart(t *testing.T) {
	ctx := context.Background()
	engine, root := cognitiveOpenUpgradeProject(t)
	current, closed := engine, false
	t.Cleanup(func() {
		if !closed {
			_ = current.Close()
		}
	})
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "inspect port"})
	if err != nil {
		t.Fatal(err)
	}
	owner := handle.ID
	result, err := engine.runAgentWith(ctx, owner, cogAnswer("checked 8080"), AgentOptions{MaxSteps: 6, Policy: "workspace"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("seed run = %+v, %v", result, err)
	}

	path := engine.store.Path()
	stateDir := filepath.Dir(path)
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	set, err := migrate.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	from, now, err := migrate.Downgrade(ctx, stateDir, set, 15)
	if err != nil {
		t.Fatalf("downgrade before upgrade: %v", err)
	}
	if from != set.Latest() || now != 15 {
		t.Fatalf("downgrade moved %d -> %d, want %d -> 15", from, now, set.Latest())
	}

	engine, err = Open(Options{Workspace: root})
	if err != nil {
		t.Fatalf("open existing Go database for upgrade: %v", err)
	}
	current, closed = engine, false
	if got := cognitiveSchemaVersion(t, path); got != set.Latest() {
		t.Fatalf("schema after upgrade = %d, want %d", got, set.Latest())
	}

	scope := cognitiveMustScope(t, engine, owner, "project")
	first := CognitiveNode{ID: "restart-a", Kind: "entity", Title: "Restart A", Body: "body a", Tags: []string{"restart"}, Scope: scope}
	cognitiveMustPut(t, engine, owner, first, 0)
	second := CognitiveNode{ID: "restart-b", Kind: "belief", Title: "Restart B", Body: "body b", Scope: scope}
	cognitiveMustPut(t, engine, owner, second, 0)
	cognitiveMustPut(t, engine, owner, CognitiveNode{ID: "restart-b", Kind: "belief", Title: "Restart B revised", Body: "body b2", Scope: scope}, 1)
	if err := engine.PutCognitiveEdge(ctx, owner, CognitiveEdge{ID: "restart-edge", From: "restart-a", To: "restart-b", Type: "relates_to", Weight: 0.25}); err != nil {
		t.Fatal(err)
	}

	beforeNodes, beforeEdges, err := engine.nativeGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeNodes) != 2 || len(beforeEdges) != 1 {
		t.Fatalf("seeded graph nodes=%d edges=%d", len(beforeNodes), len(beforeEdges))
	}
	wantA := beforeNodes[0]
	wantB := beforeNodes[1]
	wantEdge := beforeEdges[0]
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true

	reopened, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatalf("reopen existing Go database: %v", err)
	}
	current, closed = reopened, false
	afterNodes, afterEdges, err := reopened.nativeGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterNodes, beforeNodes) || !reflect.DeepEqual(afterEdges, beforeEdges) {
		t.Fatalf("restart changed graph:\nbefore nodes=%#v edges=%#v\nafter nodes=%#v edges=%#v", beforeNodes, beforeEdges, afterNodes, afterEdges)
	}
	gotA, err := reopened.ReadCognitiveNode(ctx, owner, "restart-a", 1)
	if err != nil || !reflect.DeepEqual(gotA, wantA) {
		t.Fatalf("restart node a = %#v, %v; want %#v", gotA, err, wantA)
	}
	gotB, err := reopened.ReadCognitiveNode(ctx, owner, "restart-b", 0)
	if err != nil || !reflect.DeepEqual(gotB, wantB) || gotB.Version != 2 {
		t.Fatalf("restart node b = %#v, %v; want %#v", gotB, err, wantB)
	}
	graph, _, err := reopened.CognitiveGraphSnapshot(ctx)
	if err != nil || len(graph.Nodes) != 2 || len(graph.Edges) != 1 {
		t.Fatalf("restart snapshot = %+v, %v", graph, err)
	}
	gotEdge := graph.Edges["restart-edge"]
	if gotEdge.ID != wantEdge.ID || gotEdge.From != wantEdge.From || gotEdge.To != wantEdge.To || gotEdge.Type != wantEdge.Type || gotEdge.Weight != wantEdge.Weight {
		t.Fatalf("restart edge = %+v, want %+v", gotEdge, wantEdge)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
}

func TestCognitiveGraphVerifiedEvidenceAndReadOnlyGovernor(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "inspect port", cogAnswer("checked 8080"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	assertion := portFact("8080")

	verified := CognitiveNode{ID: "verified", Kind: "belief", Title: "Verified port", Scope: scope, Assertion: &assertion}
	cognitiveMustPut(t, f.engine, owner, verified, 0)
	unverified := CognitiveNode{ID: "unverified", Kind: "belief", Title: "Unverified note", Scope: scope}
	cognitiveMustPut(t, f.engine, owner, unverified, 0)
	// The fixture binds port_value(service)=9090 to the "port9090" check, which fails while PORT is
	// 8080: the closed episode is a real failed verdict, not an unbound claim.
	contradicted := portFact("9090")
	failed := CognitiveNode{ID: "failed", Kind: "belief", Title: "Failed port", Scope: scope, Assertion: &contradicted}
	cognitiveMustPut(t, f.engine, owner, failed, 0)
	future := CognitiveNode{ID: "future", Kind: "belief", Title: "Future observation", Scope: scope,
		Times: CognitiveTimes{Valid: CognitiveValidTime{From: time.Now().UTC().Add(time.Hour)}}}
	cognitiveMustPut(t, f.engine, owner, future, 0)
	invalid := CognitiveNode{ID: "invalid", Kind: "belief", Title: "Invalidated belief", Scope: scope, Status: "invalid"}
	cognitiveMustPut(t, f.engine, owner, invalid, 0)

	if _, err := f.engine.VerifyCognitiveNode(ctx, owner, "verified", 1); err != nil {
		t.Fatalf("verify passed assertion: %v", err)
	}
	contradictedNode, err := f.engine.VerifyCognitiveNode(ctx, owner, "failed", 1)
	if err != nil {
		t.Fatalf("verify failed assertion: %v", err)
	}
	if contradictedNode.Status != "stale" || len(contradictedNode.Evidence) != 1 || contradictedNode.Evidence[0].Verification != "failed" {
		t.Fatalf("failed verification = %+v", contradictedNode)
	}
	if err := f.engine.PutCognitiveEdge(ctx, owner, CognitiveEdge{ID: "conflict", From: "verified", To: "unverified", Type: "contradicts", Weight: 0.75}); err != nil {
		t.Fatal(err)
	}

	path := f.engine.store.Path()
	for _, table := range []string{"sessions", "eval_results", "request_envelopes", "cognition_nodes", "cognition_edges"} {
		if n := cognitiveTableCount(t, path, table); n == 0 {
			t.Fatalf("the read-only snapshot would not cover actual content: %s has no rows", table)
		}
	}

	graph, _, err := f.engine.CognitiveGraphSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Nodes) != 5 || len(graph.Edges) != 1 {
		t.Fatalf("snapshot nodes=%d edges=%d", len(graph.Nodes), len(graph.Edges))
	}

	verifiedNode := graph.Nodes["verified"]
	if verifiedNode.Status != "active" || verifiedNode.Grade != "runtime" || verifiedNode.TrustZone != "runtime_verified" || verifiedNode.Metadata["useLimitation"] != "" {
		t.Fatalf("verified projection = %+v", verifiedNode)
	}
	verifiedEvidence, ok := verifiedNode.Metadata["evidence"].([]knowledge.Evidence)
	if !ok || len(verifiedEvidence) != 1 || verifiedEvidence[0].Verification != "passed" || verifiedEvidence[0].Times.VerifiedAt.IsZero() {
		t.Fatalf("verified evidence = %#v", verifiedNode.Metadata["evidence"])
	}
	unverifiedNode := graph.Nodes["unverified"]
	if unverifiedNode.Status != "active" || unverifiedNode.Grade != "hypothesis" || unverifiedNode.TrustZone != "model_inferred" || unverifiedNode.Metadata["useLimitation"] != "" {
		t.Fatalf("unverified projection = %+v", unverifiedNode)
	}
	if evidence, _ := unverifiedNode.Metadata["evidence"].([]knowledge.Evidence); len(evidence) != 0 {
		t.Fatalf("unverified node carried evidence: %#v", evidence)
	}
	failedNode := graph.Nodes["failed"]
	if failedNode.Status != "stale" || failedNode.Metadata["useLimitation"] != "invalidated" {
		t.Fatalf("failed projection = %+v", failedNode)
	}
	failedEvidence, ok := failedNode.Metadata["evidence"].([]knowledge.Evidence)
	if !ok || len(failedEvidence) != 1 || failedEvidence[0].Verification != "failed" || len(failedEvidence[0].ContradictingClaims) != 1 || failedEvidence[0].ContradictingClaims[0] != contradicted.ValueDigest() {
		t.Fatalf("failed evidence = %#v", failedNode.Metadata["evidence"])
	}
	futureNode := graph.Nodes["future"]
	if futureNode.Status != "active" || futureNode.Metadata["useLimitation"] != "not_yet_valid" {
		t.Fatalf("future projection = %+v", futureNode)
	}
	invalidNode := graph.Nodes["invalid"]
	if invalidNode.Status != "invalid" || invalidNode.Metadata["useLimitation"] != "invalidated" {
		t.Fatalf("invalid projection = %+v", invalidNode)
	}
	readInvalid, err := f.engine.ReadCognitiveNode(ctx, owner, "invalid", 0)
	if err != nil || readInvalid.Times.InvalidatedAt.IsZero() {
		t.Fatalf("invalid node = %+v, %v", readInvalid, err)
	}

	before := cognitiveStoreDump(t, path)
	analysis, err := f.engine.GovernorAnalyze(ctx, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if after := cognitiveStoreDump(t, path); after != before {
		t.Fatal("GovernorAnalyze changed store content")
	}
	if analysis.Metrics.NodeCount != 5 || analysis.Metrics.EdgeCount != 1 || analysis.Metrics.StaleCount != 1 || analysis.Metrics.ContradictionCount != 1 {
		t.Fatalf("metrics = %+v", analysis.Metrics)
	}
	if analysis.Values["verified"] <= analysis.Values["unverified"] {
		t.Fatalf("verified node did not outrank unverified: values=%#v", analysis.Values)
	}
	if len(analysis.Candidates.Branch) != 1 {
		t.Fatalf("branches = %#v", analysis.Candidates.Branch)
	}
	branch := analysis.Candidates.Branch[0]
	if branch.EdgeID != "conflict" || branch.From != "verified" || branch.To != "unverified" || branch.Relation != "contradicts" || branch.Weight != 0.75 {
		t.Fatalf("branch = %+v", branch)
	}
	if got := cognitiveArchiveIDs(analysis.Candidates.Archive); len(got) != 0 {
		t.Fatalf("protected failed evidence selected for archive: %v", got)
	}

	plan, err := f.engine.GovernorPlan(ctx, nil, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if after := cognitiveStoreDump(t, path); after != before {
		t.Fatal("GovernorPlan changed store content")
	}
	if plan.Curator || !plan.Validation.Valid || len(plan.Plan.Archive) != 0 {
		t.Fatalf("plan = %+v", plan)
	}

	validation, err := f.engine.GovernorValidate(ctx, plan.Plan)
	if err != nil {
		t.Fatal(err)
	}
	if after := cognitiveStoreDump(t, path); after != before {
		t.Fatal("GovernorValidate changed store content")
	}
	if !validation.Valid || len(validation.Normalized.Archive) != 0 {
		t.Fatalf("validation = %+v", validation)
	}
}

func TestCognitiveGraphScopePrivacyAndNarrowedHistory(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	a := f.run(t, "owner a", cogAnswer("a"))
	b := f.run(t, "owner b", cogAnswer("b"))
	scopeA := cognitiveMustScope(t, f.engine, a, "project")
	scopeASession := cognitiveMustScope(t, f.engine, a, "session")
	scopeBSession := cognitiveMustScope(t, f.engine, b, "session")
	scopeASource := cognitiveMustScope(t, f.engine, a, "source")

	shared := CognitiveNode{ID: "shared", Kind: "belief", Title: "Shared project claim", Scope: scopeA}
	cognitiveMustPut(t, f.engine, a, shared, 0)
	if got, err := f.engine.ReadCognitiveNode(ctx, b, "shared", 1); err != nil || got.Scope.Kind != knowledge.ScopeProject {
		t.Fatalf("project node read by peer = %+v, %v", got, err)
	}

	narrowed := shared
	narrowed.Scope = scopeASession
	narrowed.Title = "Shared claim narrowed"
	cognitiveMustPut(t, f.engine, a, narrowed, 1)
	if _, err := f.engine.ReadCognitiveNode(ctx, b, "shared", 0); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("peer read latest narrowed revision = %v, want ErrFenced", err)
	}
	if _, err := f.engine.ReadCognitiveNode(ctx, b, "shared", 1); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("peer read historical revision of narrowed node = %v, want ErrFenced", err)
	}
	if got, err := f.engine.ReadCognitiveNode(ctx, a, "shared", 0); err != nil || got.Scope != scopeASession {
		t.Fatalf("owner read latest narrowed revision = %+v, %v", got, err)
	}
	if got, err := f.engine.ReadCognitiveNode(ctx, a, "shared", 1); err != nil || got.Scope != scopeA {
		t.Fatalf("owner read historical revision = %+v, %v", got, err)
	}

	sessionOnly := CognitiveNode{ID: "session-only", Kind: "belief", Title: "Session only", Scope: scopeBSession}
	cognitiveMustPut(t, f.engine, b, sessionOnly, 0)
	if _, err := f.engine.ReadCognitiveNode(ctx, a, "session-only", 0); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("other session read = %v, want ErrFenced", err)
	}
	if got, err := f.engine.ReadCognitiveNode(ctx, b, "session-only", 0); err != nil || got.Scope != scopeBSession {
		t.Fatalf("owning session read = %+v, %v", got, err)
	}

	sourceOnly := CognitiveNode{ID: "source-only", Kind: "evidence", Title: "Source only", Scope: scopeASource}
	cognitiveMustPut(t, f.engine, a, sourceOnly, 0)
	if _, err := f.engine.ReadCognitiveNode(ctx, b, "source-only", 0); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("other source read = %v, want ErrFenced", err)
	}
	if got, err := f.engine.ReadCognitiveNode(ctx, a, "source-only", 0); err != nil || got.Scope != scopeASource {
		t.Fatalf("source owner read = %+v, %v", got, err)
	}

	scopeAReader := cognitiveMustScope(t, f.engine, a, "reader")
	readerOnly := CognitiveNode{ID: "reader-only", Kind: "belief", Title: "Reader only", Scope: scopeAReader}
	cognitiveMustPut(t, f.engine, a, readerOnly, 0)
	if _, err := f.engine.ReadCognitiveNode(ctx, b, "reader-only", 0); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("other reader read = %v, want ErrFenced", err)
	}
	if got, err := f.engine.ReadCognitiveNode(ctx, a, "reader-only", 0); err != nil || got.Scope != scopeAReader {
		t.Fatalf("reader owner read = %+v, %v", got, err)
	}
	readerB, err := f.engine.cognitiveReader(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	graphBNodes, _, err := f.engine.store.CognitiveGraph(ctx, readerB)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"shared", "source-only", "reader-only"} {
		if cognitiveHasNode(graphBNodes, id) {
			t.Fatalf("peer graph exposed %s", id)
		}
	}
	if !cognitiveHasNode(graphBNodes, "session-only") {
		t.Fatal("the owning session's graph lost its session-scoped node")
	}

	p1 := CognitiveNode{ID: "edge-p1", Kind: "entity", Title: "Edge P1", Scope: scopeA}
	p2 := CognitiveNode{ID: "edge-p2", Kind: "entity", Title: "Edge P2", Scope: scopeA}
	cognitiveMustPut(t, f.engine, a, p1, 0)
	cognitiveMustPut(t, f.engine, a, p2, 0)
	if err := f.engine.PutCognitiveEdge(ctx, b, CognitiveEdge{ID: "b-owned", From: "edge-p1", To: "edge-p2", Type: "relates_to", Weight: 0.5}); err != nil {
		t.Fatal(err)
	}
	if _, edges, err := f.engine.nativeGraph(ctx); err != nil || !cognitiveHasEdge(edges, "b-owned") {
		t.Fatalf("edge before its owner was fenced = %#v, %v", edges, err)
	}
	if _, _, err := f.engine.store.BeginPrivacyDeletion(ctx, []string{b}); err != nil {
		t.Fatalf("fence edge owner: %v", err)
	}
	if _, err := f.engine.ReadCognitiveNode(ctx, b, "session-only", 0); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("fenced consumer read = %v, want ErrFenced", err)
	}
	_, edges, err := f.engine.nativeGraph(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cognitiveHasEdge(edges, "b-owned") {
		t.Fatalf("graph exposed an edge owned by fenced session B: %#v", edges)
	}
	graph, _, err := f.engine.CognitiveGraphSnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := graph.Edges["b-owned"]; ok {
		t.Fatal("snapshot exposed an edge owned by fenced session B")
	}
	for _, id := range []string{"edge-p1", "edge-p2"} {
		if _, ok := graph.Nodes[id]; !ok {
			t.Fatalf("fencing B removed A-owned node %s", id)
		}
	}
}

func TestCognitiveGraphCASAttributionAndStaleSourceRevision(t *testing.T) {
	f := newCognitionFixture(t, false)
	ctx := context.Background()
	owner := f.run(t, "source owner", cogAnswer("first answer"))
	scope := cognitiveMustScope(t, f.engine, owner, "project")
	source, err := f.engine.store.CognitiveSource(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}

	original := CognitiveNode{ID: "cas", Kind: "belief", Title: "CAS belief", Body: "one", Scope: scope, Sources: []knowledge.SourceRef{source}}
	cognitiveMustPut(t, f.engine, owner, original, 0)
	first, err := f.engine.ReadCognitiveNode(ctx, owner, "cas", 1)
	if err != nil || first.Version != 1 || !reflect.DeepEqual(first.Sources, []knowledge.SourceRef{source}) {
		t.Fatalf("first revision = %+v, %v", first, err)
	}

	final := "second answer"
	if err := f.engine.store.Update(ctx, owner, session.Patch{Final: &final}); err != nil {
		t.Fatal(err)
	}
	replacement, err := f.engine.store.CognitiveSource(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	if replacement.SessionID != source.SessionID || replacement.Digest == source.Digest {
		t.Fatalf("stale source setup: first=%+v replacement=%+v", source, replacement)
	}

	revised := original
	revised.Body = "two"
	revised.Sources = []knowledge.SourceRef{replacement}
	if _, err := f.engine.PutCognitiveNode(ctx, owner, revised, 1); err != nil {
		t.Fatalf("replace a stale source version with the same source lineage: %v", err)
	}
	second, err := f.engine.ReadCognitiveNode(ctx, owner, "cas", 2)
	if err != nil || second.Version != 2 || !reflect.DeepEqual(second.Sources, []knowledge.SourceRef{replacement}) {
		t.Fatalf("second revision = %+v, %v", second, err)
	}

	if _, err := f.engine.PutCognitiveNode(ctx, owner, revised, 0); !errors.Is(err, session.ErrCognitiveRevision) {
		t.Fatalf("create over an existing id = %v, want ErrCognitiveRevision", err)
	}
	if _, err := f.engine.PutCognitiveNode(ctx, owner, revised, 1); !errors.Is(err, session.ErrCognitiveRevision) {
		t.Fatalf("stale CAS revision = %v, want ErrCognitiveRevision", err)
	}
	latest, err := f.engine.ReadCognitiveNode(ctx, owner, "cas", 0)
	if err != nil || latest.Version != 2 || !reflect.DeepEqual(latest, second) {
		t.Fatalf("CAS failure changed latest = %+v, %v; want %+v", latest, err, second)
	}
	historical, err := f.engine.ReadCognitiveNode(ctx, owner, "cas", 1)
	if err != nil || historical.Version != 1 || !reflect.DeepEqual(historical.Sources, []knowledge.SourceRef{source}) {
		t.Fatalf("historical revision = %+v, %v", historical, err)
	}
}

func TestCognitiveGraphCorruptionFailsClosed(t *testing.T) {
	cases := []struct {
		name       string
		inject     string
		readNodeID string
	}{
		{name: "node incomplete JSON", inject: `UPDATE cognition_nodes SET json='{"id":"n1"}' WHERE id='n1'`, readNodeID: "n1"},
		{name: "node column mismatch", inject: `UPDATE cognition_nodes SET kind='entity' WHERE id='n1'`, readNodeID: "n1"},
		{name: "edge target missing", inject: `DELETE FROM cognition_nodes WHERE id='n2'`},
		{name: "edge incomplete JSON", inject: `UPDATE cognition_edges SET json='{"id":"e1"}' WHERE id='e1'`},
		{name: "edge column mismatch", inject: `UPDATE cognition_edges SET to_id='ghost' WHERE id='e1'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCognitionFixture(t, false)
			ctx := context.Background()
			owner := f.run(t, "corrupt graph seed", cogAnswer("seed"))
			scope := cognitiveMustScope(t, f.engine, owner, "project")
			cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "n1", Kind: "belief", Title: "N1", Scope: scope}, 0)
			cognitiveMustPut(t, f.engine, owner, CognitiveNode{ID: "n2", Kind: "belief", Title: "N2", Scope: scope}, 0)
			if err := f.engine.PutCognitiveEdge(ctx, owner, CognitiveEdge{ID: "e1", From: "n1", To: "n2", Type: "relates_to", Weight: 0.5}); err != nil {
				t.Fatal(err)
			}

			path := f.engine.store.Path()
			cognitiveExecFault(t, path, tc.inject)

			nodes, edges, err := f.engine.nativeGraph(ctx)
			if !errors.Is(err, session.ErrCognitiveCorrupt) || nodes != nil || edges != nil {
				t.Fatalf("native graph = %#v, %#v, %v; want nil, nil, ErrCognitiveCorrupt", nodes, edges, err)
			}
			if tc.readNodeID != "" {
				if _, err := f.engine.ReadCognitiveNode(ctx, owner, tc.readNodeID, 0); !errors.Is(err, session.ErrCognitiveCorrupt) {
					t.Fatalf("read corrupt node = %v, want ErrCognitiveCorrupt", err)
				}
			}

			before := cognitiveStoreDump(t, path)
			graph, telemetry, err := f.engine.CognitiveGraphSnapshot(ctx)
			if !errors.Is(err, session.ErrCognitiveCorrupt) {
				t.Fatalf("snapshot = %+v, %v; want ErrCognitiveCorrupt", graph, err)
			}
			if !reflect.DeepEqual(graph, cognition.GraphState{}) || telemetry != nil {
				t.Fatalf("corrupt snapshot leaked partial state = %+v, %#v", graph, telemetry)
			}
			if after := cognitiveStoreDump(t, path); after != before {
				t.Fatal("CognitiveGraphSnapshot changed store content")
			}

			analysis, err := f.engine.GovernorAnalyze(ctx, cognition.GovernorAnalyzeOptions{})
			if !errors.Is(err, session.ErrCognitiveCorrupt) || !reflect.DeepEqual(analysis, cognition.GovernorAnalysis{}) {
				t.Fatalf("analyze corrupt graph = %+v, %v", analysis, err)
			}
			if after := cognitiveStoreDump(t, path); after != before {
				t.Fatal("GovernorAnalyze changed store content")
			}

			plan, err := f.engine.GovernorPlan(ctx, nil, cognition.GovernorAnalyzeOptions{})
			if !errors.Is(err, session.ErrCognitiveCorrupt) || !reflect.DeepEqual(plan, GovernorPlanResult{}) {
				t.Fatalf("plan corrupt graph = %+v, %v", plan, err)
			}
			if after := cognitiveStoreDump(t, path); after != before {
				t.Fatal("GovernorPlan changed store content")
			}

			validation, err := f.engine.GovernorValidate(ctx, cognition.GovernorPlan{})
			if !errors.Is(err, session.ErrCognitiveCorrupt) || !reflect.DeepEqual(validation, cognition.GovernorValidation{}) {
				t.Fatalf("validate corrupt graph = %+v, %v", validation, err)
			}
			if after := cognitiveStoreDump(t, path); after != before {
				t.Fatal("GovernorValidate changed store content")
			}
		})
	}
}

// cognitiveOpenUpgradeProject builds the same real trusted Go project the cognition fixture uses,
// but leaves the Engine lifecycle to the caller so an upgrade can close and reopen it exactly once.
func cognitiveOpenUpgradeProject(t *testing.T) (*Engine, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	initGitRepo(t, root)
	setProjectChecks(t, root, cognitionChecks)
	trust := `"trustedWorkspaces": [` + quoteJSON(root) + `], `
	useGlobalConfig(t, strings.Replace(boundCatalog("test/b0"), `{"model"`, `{`+trust+`"model"`, 1))
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "config.env"), cognitionConfig)
	writeFile(t, filepath.Join(root, "defaults.env"), "BASE=8080\n")
	runGitIn(t, root, "add", "config.env", "defaults.env")
	runGitIn(t, root, "commit", "-m", "config")
	approveChecks(t, engine)
	return engine, root
}

func cognitiveMustScope(t *testing.T, engine *Engine, sessionID, kind string) CognitiveScope {
	t.Helper()
	scope, err := engine.CognitiveScopeFor(context.Background(), sessionID, kind)
	if err != nil {
		t.Fatalf("scope %s for %s: %v", kind, sessionID, err)
	}
	return scope
}

func cognitiveMustPut(t *testing.T, engine *Engine, sessionID string, node CognitiveNode, expected int64) CognitiveNode {
	t.Helper()
	got, err := engine.PutCognitiveNode(context.Background(), sessionID, node, expected)
	if err != nil {
		t.Fatalf("put %s at expected %d: %v", node.ID, expected, err)
	}
	return got
}

func cognitiveArchiveIDs(values []cognition.ArchiveCandidate) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = value.NodeID
	}
	return out
}

func cognitiveHasEdge(values []knowledge.Edge, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}

func cognitiveHasNode(values []knowledge.Node, id string) bool {
	for _, value := range values {
		if value.ID == id {
			return true
		}
	}
	return false
}

func cognitiveSchemaVersion(t *testing.T, path string) int {
	t.Helper()
	db := cognitiveOpenReadOnly(t, path)
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

func cognitiveTableCount(t *testing.T, path, table string) int {
	t.Helper()
	db := cognitiveOpenReadOnly(t, path)
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func cognitiveOpenReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// cognitiveDumpSkip names tables that are not state a read must leave alone: runtime_events is an
// append-only diagnostic log that another component may flush while the test reads.
var cognitiveDumpSkip = map[string]bool{"runtime_events": true}

// cognitiveStoreDump is the complete content of every user table, used to prove that a read does not
// touch the graph or any adjacent row (results, envelopes, session/task state).
func cognitiveStoreDump(t *testing.T, path string) string {
	t.Helper()
	db := cognitiveOpenReadOnly(t, path)
	defer db.Close()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	tables := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if cognitiveDumpSkip[name] {
			continue
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	fmt.Fprintf(&out, "user_version=%d\n", version)
	for _, table := range tables {
		query := `SELECT * FROM "` + strings.ReplaceAll(table, `"`, `""`) + `" ORDER BY rowid`
		content, err := db.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := content.Columns()
		if err != nil {
			_ = content.Close()
			t.Fatal(err)
		}
		fmt.Fprintf(&out, "table=%s\n", table)
		for content.Next() {
			values := make([]any, len(columns))
			scan := make([]any, len(columns))
			for i := range values {
				scan[i] = &values[i]
			}
			if err := content.Scan(scan...); err != nil {
				_ = content.Close()
				t.Fatal(err)
			}
			for i, column := range columns {
				fmt.Fprintf(&out, "%s=%#v\x00", column, values[i])
			}
			out.WriteByte('\n')
		}
		if err := content.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return out.String()
}

func cognitiveExecFault(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("inject corruption: %v", err)
	}
}
