package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/protocol"
	_ "github.com/mattn/go-sqlite3"
)

func TestGovernorReadsSharedSQLiteGraphWithoutMutation(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil { t.Fatal(err) }
	defer engine.Close()
	seedGovernorGraph(t, engine)

	ctx := context.Background()
	before := rawGraphRows(t, engine)
	analysis, err := engine.GovernorAnalyze(ctx, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if err != nil { t.Fatal(err) }
	if analysis.Metrics.NodeCount != 5 || analysis.Metrics.ContradictionCount != 1 {
		t.Fatalf("metrics=%#v", analysis.Metrics)
	}
	if !runtimeHasArchive(analysis.Candidates.Archive, "c") {
		t.Fatalf("archive=%#v", analysis.Candidates.Archive)
	}
	if runtimeHasArchive(analysis.Candidates.Archive, "d") {
		t.Fatal("reproduced evidence became archive candidate")
	}
	if analysis.Values["a"] <= analysis.Values["b"] {
		t.Fatalf("expected tested repo-trusted a to outrank b: values=%#v", analysis.Values)
	}

	planResult, err := engine.GovernorPlan(ctx, nil, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if err != nil { t.Fatal(err) }
	if planResult.Curator { t.Fatal("deterministic plan unexpectedly used curator") }
	if len(planResult.Plan.Canonicalize) != 0 || len(planResult.Plan.Branch) != 0 || len(planResult.Plan.Promote) != 0 {
		t.Fatalf("deterministic plan leaked semantic actions: %#v", planResult.Plan)
	}
	if !planResult.Validation.Valid { t.Fatalf("validation=%#v", planResult.Validation) }

	blocked, err := engine.GovernorValidate(ctx, cognition.GovernorPlan{Archive: []string{"d"}})
	if err != nil { t.Fatal(err) }
	if blocked.Valid || !runtimeContainsText(blocked.Errors, "high-grade evidence") {
		t.Fatalf("blocked=%#v", blocked)
	}
	after := rawGraphRows(t, engine)
	if before != after { t.Fatal("read-only Governor runtime mutated shared graph rows") }
}

type runtimeGovernorProvider struct {
	request protocol.ProviderRequest
}

func (p *runtimeGovernorProvider) Model() string { return "governor-model" }

func (p *runtimeGovernorProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.request = req
	return protocol.ProviderResponse{Message: protocol.Message{Content: `{"archive":["c"],"canonicalize":[{"canonical":"a","aliases":["b"]}],"summary":"curated"}`}}, nil
}

func TestGovernorPlanCanUseCuratorOverSharedSQLiteSnapshot(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil { t.Fatal(err) }
	defer engine.Close()
	seedGovernorGraph(t, engine)
	before := rawGraphRows(t, engine)
	provider := &runtimeGovernorProvider{}
	curator := &cognition.LLMGovernorCurator{Provider: provider, MaxTokens: 3000, ReasoningEffort: "high"}
	result, err := engine.GovernorPlan(context.Background(), curator, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if err != nil { t.Fatal(err) }
	if !result.Curator || result.CuratorModel != "governor-model" {
		t.Fatalf("curator result=%#v", result)
	}
	if !result.Validation.Valid { t.Fatalf("validation=%#v", result.Validation) }
	if len(result.Plan.Canonicalize) != 1 || result.Plan.Canonicalize[0].Canonical != "a" {
		t.Fatalf("plan=%#v", result.Plan)
	}
	if provider.request.ReasoningEffort != "high" || provider.request.MaxTokens != 3000 {
		t.Fatalf("request=%#v", provider.request)
	}
	if before != rawGraphRows(t, engine) { t.Fatal("Curator planning mutated shared graph") }
}

func TestGovernorReportsMissingGraphTablesInGoOnlyWorkspace(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil { t.Fatal(err) }
	defer engine.Close()
	_, err = engine.GovernorAnalyze(context.Background(), cognition.GovernorAnalyzeOptions{})
	if err == nil || !strings.Contains(err.Error(), "cognitive graph tables") {
		t.Fatalf("err=%v", err)
	}
}

func seedGovernorGraph(t *testing.T, engine *Engine) {
	t.Helper()
	db, err := sql.Open("sqlite3", engine.store.Path())
	if err != nil { t.Fatal(err) }
	defer db.Close()
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS graph_nodes (
  id TEXT PRIMARY KEY, json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS graph_edges (
  id TEXT PRIMARY KEY, json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS graph_node_storage (
  node_id TEXT PRIMARY KEY, tier TEXT NOT NULL, last_access_at TEXT, access_count INTEGER NOT NULL DEFAULT 0
);
`)
	if err != nil { t.Fatal(err) }
	nodes := []cognition.GraphNode{
		{ID: "a", Kind: "belief", Title: "Provider Architecture", Body: "A", Status: "active", Grade: "tested", TrustZone: "repo_trusted", Tags: []string{"provider"}},
		{ID: "b", Kind: "belief", Title: "provider architecture", Body: "B", Status: "active", Grade: "static", TrustZone: "model_inferred", Tags: []string{"provider"}},
		{ID: "c", Kind: "evidence", Title: "Old hypothesis", Body: "C", Status: "stale", Grade: "hypothesis", TrustZone: "model_inferred", Tags: []string{"provider"}},
		{ID: "d", Kind: "evidence", Title: "Runtime proof", Body: "D", Status: "stale", Grade: "reproduced", TrustZone: "runtime_verified"},
		{ID: "e", Kind: "entity", Title: "Provider Registry", Body: "E", Status: "active", Grade: "static", TrustZone: "repo_trusted", Tags: []string{"provider"}},
	}
	for _, node := range nodes {
		raw, _ := json.Marshal(node)
		if _, err := db.Exec("INSERT INTO graph_nodes(id, json) VALUES(?, ?)", node.ID, string(raw)); err != nil { t.Fatal(err) }
	}
	edge := cognition.GraphEdge{ID: "e1", From: "a", To: "b", Type: "contradicts", Weight: 0.9}
	rawEdge, _ := json.Marshal(edge)
	if _, err := db.Exec("INSERT INTO graph_edges(id, json) VALUES(?, ?)", edge.ID, string(rawEdge)); err != nil { t.Fatal(err) }
	if _, err := db.Exec("INSERT INTO graph_node_storage(node_id, tier, last_access_at, access_count) VALUES(?, ?, ?, ?)", "a", "hot", "2026-09-25T00:00:00Z", 64); err != nil { t.Fatal(err) }
	if _, err := db.Exec("INSERT INTO metadata(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "graph_version", "1"); err != nil { t.Fatal(err) }
	if _, err := db.Exec("INSERT INTO metadata(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", "graph_metadata", "{}"); err != nil { t.Fatal(err) }
}

func rawGraphRows(t *testing.T, engine *Engine) string {
	t.Helper()
	db, err := sql.Open("sqlite3", engine.store.Path())
	if err != nil { t.Fatal(err) }
	defer db.Close()
	rows, err := db.Query("SELECT json FROM graph_nodes ORDER BY id")
	if err != nil { t.Fatal(err) }
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil { t.Fatal(err) }
		values = append(values, raw)
	}
	edges, err := db.Query("SELECT json FROM graph_edges ORDER BY id")
	if err != nil { t.Fatal(err) }
	defer edges.Close()
	for edges.Next() {
		var raw string
		if err := edges.Scan(&raw); err != nil { t.Fatal(err) }
		values = append(values, raw)
	}
	return strings.Join(values, "\n")
}

func runtimeHasArchive(values []cognition.ArchiveCandidate, id string) bool {
	for _, item := range values { if item.NodeID == id { return true } }
	return false
}

func runtimeContainsText(values []string, target string) bool {
	for _, value := range values { if strings.Contains(value, target) { return true } }
	return false
}
