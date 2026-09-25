package cognition

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

func governorFixture() GraphState {
	return GraphState{
		Version: 1,
		Nodes: map[string]GraphNode{
			"a": {ID: "a", Kind: "belief", Title: "Provider Architecture", Body: "A", Status: "active", Grade: "tested", TrustZone: "repo_trusted", Tags: []string{"provider"}},
			"b": {ID: "b", Kind: "belief", Title: "provider architecture", Body: "B", Status: "active", Grade: "static", TrustZone: "model_inferred", Tags: []string{"provider"}},
			"c": {ID: "c", Kind: "evidence", Title: "Old hypothesis", Body: "C", Status: "stale", Grade: "hypothesis", TrustZone: "model_inferred", Tags: []string{"provider"}},
			"d": {ID: "d", Kind: "evidence", Title: "Runtime proof", Body: "D", Status: "stale", Grade: "reproduced", TrustZone: "runtime_verified"},
			"e": {ID: "e", Kind: "entity", Title: "Provider Registry", Body: "E", Status: "active", Grade: "static", TrustZone: "repo_trusted", Tags: []string{"provider"}},
		},
		Edges: map[string]GraphEdge{
			"e1": {ID: "e1", From: "a", To: "b", Type: "contradicts", Weight: 0.9},
		},
		Metadata: map[string]any{},
	}
}

func TestGovernorAnalyzerMatchesNodeCandidateSemantics(t *testing.T) {
	graph := governorFixture()
	before, _ := json.Marshal(graph)
	analysis := (GovernorAnalyzer{}).Analyze(graph, GovernorAnalyzeOptions{
		ArchiveThreshold: 0.5,
		PromotionMinGroup: 3,
		Now: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC),
	})
	if analysis.Metrics.NodeCount != 5 || analysis.Metrics.EdgeCount != 1 {
		t.Fatalf("metrics=%#v", analysis.Metrics)
	}
	if analysis.Metrics.ContradictionCount != 1 {
		t.Fatalf("contradictions=%d", analysis.Metrics.ContradictionCount)
	}
	if !hasCanonicalGroup(analysis.Candidates.Canonicalize, "a", "b") {
		t.Fatalf("canonical candidates=%#v", analysis.Candidates.Canonicalize)
	}
	if !hasPromotionTag(analysis.Candidates.Promote, "provider") {
		t.Fatalf("promotion candidates=%#v", analysis.Candidates.Promote)
	}
	if !hasArchiveNode(analysis.Candidates.Archive, "c") {
		t.Fatalf("archive candidates=%#v", analysis.Candidates.Archive)
	}
	if hasArchiveNode(analysis.Candidates.Archive, "d") {
		t.Fatal("reproduced evidence must not be an archive candidate")
	}
	after, _ := json.Marshal(graph)
	if string(after) != string(before) {
		t.Fatal("analyzer mutated input graph")
	}
}

func TestGovernorAnalyzerUsesAccessTelemetryWithoutChangingEvidence(t *testing.T) {
	graph := GraphState{Nodes: map[string]GraphNode{
		"recent": {ID: "recent", Kind: "entity", Title: "Recent", Status: "active", Grade: "static", TrustZone: "repo_trusted"},
		"quiet": {ID: "quiet", Kind: "entity", Title: "Quiet", Status: "active", Grade: "static", TrustZone: "repo_trusted"},
	}, Edges: map[string]GraphEdge{}}
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	analysis := (GovernorAnalyzer{}).Analyze(graph, GovernorAnalyzeOptions{
		Now: now,
		StorageByNode: map[string]StorageTelemetry{
			"recent": {Tier: "warm", AccessCount: 64, LastAccessAt: now.Add(-time.Minute)},
			"quiet": {Tier: "warm"},
		},
	})
	if analysis.Values["recent"] <= analysis.Values["quiet"] {
		t.Fatalf("values recent=%f quiet=%f", analysis.Values["recent"], analysis.Values["quiet"])
	}
	if graph.Nodes["recent"].Grade != "static" || graph.Nodes["quiet"].Grade != "static" {
		t.Fatal("storage telemetry changed evidence grade")
	}
}

func TestGovernorValidatorProtectsEvidenceAndCanonicalGrade(t *testing.T) {
	graph := governorFixture()
	blocked := ValidateGovernorPlan(GovernorPlan{Archive: []string{"d"}}, graph)
	if blocked.Valid || !containsText(blocked.Errors, "high-grade evidence") {
		t.Fatalf("validation=%#v", blocked)
	}

	badCanonical := ValidateGovernorPlan(GovernorPlan{
		Canonicalize: []CanonicalizePlan{{Canonical: "b", Aliases: []string{"a"}}},
	}, graph)
	if badCanonical.Valid || !containsText(badCanonical.Errors, "lower evidence grade") {
		t.Fatalf("canonical validation=%#v", badCanonical)
	}

	valid := ValidateGovernorPlan(GovernorPlan{
		Archive: []string{"c"},
		Tiers: GovernorTiers{Hot: []string{"a"}, Warm: []string{"b", "e"}, Cold: []string{"c", "d"}},
		Canonicalize: []CanonicalizePlan{{Canonical: "a", Aliases: []string{"b"}}},
		Branch: []BranchPlan{{From: "a", To: "b"}},
		Promote: []PromotePlan{{Title: "Provider overview", ChildIDs: []string{"a", "b", "e"}}},
		Summary: strings.Repeat("x", 5000),
	}, graph)
	if !valid.Valid { t.Fatalf("valid plan errors=%#v", valid.Errors) }
	if len(valid.Normalized.Summary) != 4000 { t.Fatalf("summary length=%d", len(valid.Normalized.Summary)) }
}

func TestDeterministicGovernorPlanContainsOnlySafeActions(t *testing.T) {
	analysis := (GovernorAnalyzer{}).Analyze(governorFixture(), GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	plan := DeterministicGovernorPlan(analysis)
	if !hasString(plan.Archive, "c") { t.Fatalf("archive=%#v", plan.Archive) }
	if len(plan.Canonicalize) != 0 || len(plan.Branch) != 0 || len(plan.Promote) != 0 {
		t.Fatalf("deterministic plan leaked semantic actions: %#v", plan)
	}
}

type governorScriptProvider struct {
	request protocol.ProviderRequest
}

func (p *governorScriptProvider) Model() string { return "governor-test" }

func (p *governorScriptProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.request = req
	return protocol.ProviderResponse{
		Message: protocol.Message{Content: "```json\n{\"archive\":[\"c\"],\"canonicalize\":[{\"canonical\":\"a\",\"aliases\":[\"b\"],\"reason\":\"same concept\"}],\"branch\":[{\"from\":\"a\",\"to\":\"b\",\"reason\":\"preserve competition\"}],\"promote\":[{\"title\":\"Provider overview\",\"childIds\":[\"a\",\"b\",\"e\"]}],\"summary\":\"curate provider cluster\"}\n```"},
		FinishReason: "stop",
	}, nil
}

func TestGovernorCuratorProducesBoundedPlanWithoutMutatingGraph(t *testing.T) {
	graph := governorFixture()
	before, _ := json.Marshal(graph)
	analysis := (GovernorAnalyzer{}).Analyze(graph, GovernorAnalyzeOptions{ArchiveThreshold: 0.5})
	provider := &governorScriptProvider{}
	curator := LLMGovernorCurator{Provider: provider, MaxTokens: 4000, ReasoningEffort: "high"}
	plan, err := curator.Propose(context.Background(), graph, analysis)
	if err != nil { t.Fatal(err) }
	validation := ValidateGovernorPlan(plan, graph)
	if !validation.Valid { t.Fatalf("validation=%#v", validation) }
	if provider.request.ReasoningEffort != "high" || provider.request.MaxTokens != 4000 {
		t.Fatalf("request=%#v", provider.request)
	}
	if len(provider.request.Messages) != 2 { t.Fatalf("messages=%#v", provider.request.Messages) }
	var payload map[string]any
	if err := json.Unmarshal([]byte(provider.request.Messages[1].Content), &payload); err != nil { t.Fatal(err) }
	if payload["objective"] != "Propose long-horizon graph maintenance. Do not execute changes." {
		t.Fatalf("objective=%#v", payload["objective"])
	}
	nodes, ok := payload["nodes"].(map[string]any)
	if !ok { t.Fatalf("nodes=%#v", payload["nodes"]) }
	if _, ok := nodes["a"]; !ok { t.Fatal("candidate node a missing") }
	if _, ok := nodes["d"]; ok { t.Fatal("unrelated protected node d leaked into Curator candidate context") }
	after, _ := json.Marshal(graph)
	if string(after) != string(before) { t.Fatal("Curator mutated graph") }
}

func hasArchiveNode(values []ArchiveCandidate, id string) bool {
	for _, item := range values { if item.NodeID == id { return true } }
	return false
}

func hasCanonicalGroup(values []CanonicalizeCandidate, ids ...string) bool {
	for _, item := range values {
		ok := true
		for _, id := range ids { if !hasString(item.NodeIDs, id) { ok = false } }
		if ok { return true }
	}
	return false
}

func hasPromotionTag(values []PromotionCandidate, tag string) bool {
	for _, item := range values { if item.Tag == tag { return true } }
	return false
}

func hasString(values []string, target string) bool {
	for _, value := range values { if value == target { return true } }
	return false
}

func containsText(values []string, target string) bool {
	for _, value := range values { if strings.Contains(value, target) { return true } }
	return false
}
