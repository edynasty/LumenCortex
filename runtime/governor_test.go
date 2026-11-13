package runtime

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/protocol"
	_ "github.com/mattn/go-sqlite3"
)

func TestGovernorReadsNativeSQLiteGraphWithoutMutation(t *testing.T) {
	f := newCognitionFixture(t, false)
	engine := f.engine
	seedGovernorGraph(t, f)

	ctx := context.Background()
	before := rawGraphRows(t, engine)
	analysis, err := engine.GovernorAnalyze(ctx, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	if planResult.Curator {
		t.Fatal("deterministic plan unexpectedly used curator")
	}
	if len(planResult.Plan.Canonicalize) != 0 || len(planResult.Plan.Branch) != 0 || len(planResult.Plan.Promote) != 0 {
		t.Fatalf("deterministic plan leaked semantic actions: %#v", planResult.Plan)
	}
	if !planResult.Validation.Valid {
		t.Fatalf("validation=%#v", planResult.Validation)
	}

	blocked, err := engine.GovernorValidate(ctx, cognition.GovernorPlan{Archive: []string{"d"}})
	if err != nil {
		t.Fatal(err)
	}
	if blocked.Valid || !runtimeContainsText(blocked.Errors, "high-grade evidence") {
		t.Fatalf("blocked=%#v", blocked)
	}
	after := rawGraphRows(t, engine)
	if before != after {
		t.Fatal("read-only Governor runtime mutated shared graph rows")
	}
}

type runtimeGovernorProvider struct {
	request protocol.ProviderRequest
	calls   int
}

func (p *runtimeGovernorProvider) Model() string { return "governor-model" }

func (p *runtimeGovernorProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls++
	p.request = req
	return protocol.ProviderResponse{Message: protocol.Message{Content: `{"archive":["c"],"canonicalize":[{"canonical":"a","aliases":["b"]}],"summary":"curated"}`}}, nil
}

// The Governor's model curator is fail-closed (ADR-0018, plan T1-S3): a curator proposal is a model
// request with no envelope, so GovernorPlan refuses it before reading the graph or calling the
// provider, and the deterministic plan is what the runtime produces.
func TestGovernorPlanRefusesTheCuratorWithoutCallingTheModel(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	// Empty native graphs are also valid; no special schema is created.
	before := rawGraphRows(t, engine)
	provider := &runtimeGovernorProvider{}
	curator := &cognition.LLMGovernorCurator{Provider: provider, MaxTokens: 3000, ReasoningEffort: "high"}
	result, err := engine.GovernorPlan(context.Background(), curator, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if !errors.Is(err, ErrGovernorCuratorDisabled) {
		t.Fatalf("GovernorPlan with a curator = %#v, %v; want ErrGovernorCuratorDisabled", result, err)
	}
	if provider.calls != 0 {
		t.Fatalf("the curator provider was called %d times", provider.calls)
	}
	if !reflect.DeepEqual(result, GovernorPlanResult{}) {
		t.Fatalf("a refused plan returned %#v", result)
	}
	if before != rawGraphRows(t, engine) {
		t.Fatal("a refused curator plan changed the graph")
	}

	// The deterministic plan is still available and still validates.
	deterministic, err := engine.GovernorPlan(context.Background(), nil, cognition.GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	if err != nil {
		t.Fatalf("deterministic plan: %v", err)
	}
	if deterministic.Curator || deterministic.CuratorModel != "" || !deterministic.Validation.Valid {
		t.Fatalf("deterministic plan=%#v", deterministic)
	}
	if provider.calls != 0 {
		t.Fatalf("the deterministic plan called the curator provider %d times", provider.calls)
	}
}

func TestGovernorUsesEmptyGoWorkspace(t *testing.T) {
	root := t.TempDir()
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	analysis, err := engine.GovernorAnalyze(context.Background(), cognition.GovernorAnalyzeOptions{})
	if err != nil || analysis.Metrics.NodeCount != 0 || analysis.Metrics.EdgeCount != 0 {
		t.Fatalf("empty graph: %+v %v", analysis, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	engine, err = Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	plan, err := engine.GovernorPlan(context.Background(), nil, cognition.GovernorAnalyzeOptions{})
	if err != nil || !plan.Validation.Valid {
		t.Fatalf("reopened empty graph: %+v %v", plan, err)
	}
}

func seedGovernorGraph(t *testing.T, f *cognitionFixture) {
	t.Helper()
	ctx := context.Background()
	owner := f.run(t, "inspect service", cogAnswer("checked"))
	scope, err := f.engine.CognitiveScopeFor(ctx, owner, "project")
	if err != nil {
		t.Fatal(err)
	}
	a, b := portFact("8080"), portFact("9090")
	nodes := []CognitiveNode{
		{ID: "a", Kind: "belief", Title: "Provider Architecture", Body: "A", Assertion: &a, Tags: []string{"provider"}},
		{ID: "b", Kind: "belief", Title: "provider architecture", Body: "B", Assertion: &b, Tags: []string{"provider"}},
		{ID: "c", Kind: "evidence", Title: "Old hypothesis", Body: "C", Status: "stale"},
		{ID: "d", Kind: "evidence", Title: "Runtime proof", Body: "D", Status: "stale", Assertion: &a},
		{ID: "e", Kind: "entity", Title: "Provider Registry", Body: "E", Tags: []string{"provider"}},
	}
	for _, node := range nodes {
		node.Scope = scope
		if _, err := f.engine.PutCognitiveNode(ctx, owner, node, 0); err != nil {
			t.Fatal(err)
		}
		if node.ID == "a" || node.ID == "d" {
			if _, err := f.engine.VerifyCognitiveNode(ctx, owner, node.ID, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := f.engine.PutCognitiveEdge(ctx, owner, CognitiveEdge{ID: "e1", From: "a", To: "b", Type: "contradicts", Weight: 0.9}); err != nil {
		t.Fatal(err)
	}
}

func rawGraphRows(t *testing.T, engine *Engine) string {
	t.Helper()
	db, err := sql.Open("sqlite3", engine.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT json FROM cognition_nodes ORDER BY id,version")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	values := []string{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		values = append(values, raw)
	}
	edges, err := db.Query("SELECT json FROM cognition_edges ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer edges.Close()
	for edges.Next() {
		var raw string
		if err := edges.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		values = append(values, raw)
	}
	return strings.Join(values, "\n")
}

func runtimeHasArchive(values []cognition.ArchiveCandidate, id string) bool {
	for _, item := range values {
		if item.NodeID == id {
			return true
		}
	}
	return false
}

func runtimeContainsText(values []string, target string) bool {
	for _, value := range values {
		if strings.Contains(value, target) {
			return true
		}
	}
	return false
}
