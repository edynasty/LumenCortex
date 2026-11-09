package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func TestContextGraphUsesCommittedEvidenceNotForgedEvents(t *testing.T) {
	e := openTestEngine(t)
	ctx := context.Background()
	id := newParentSession(t, e, &debuggerProvider{})
	if err := e.RegisterLifecycleHook(&platformHook{id: "deny-first", events: []LifecycleEvent{LifecycleBeforeToolCall}, fn: func(_ context.Context, r LifecycleRequest) (LifecycleResult, error) {
		if r.ToolArgs["path"] == "first.txt" {
			return LifecycleResult{Decision: HookDeny}, nil
		}
		return LifecycleResult{Decision: HookContinue}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := e.RunAgent(ctx, id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 4})
	if err != nil || result.Status != "completed" {
		t.Fatalf("run %+v %v", result, err)
	}
	e.events.publish("verification.completed", id, map[string]any{"status": "passed", "reasoning": "PRIVATE-SENTINEL"})
	e.events.publish("tool.end", id, map[string]any{"runId": "foreign", "executionId": "missing", "name": "write_file", "ok": true})
	graph, err := e.GetContextGraph(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	failed, completed := false, false
	for _, node := range graph.Nodes {
		if node.Kind == "tool" {
			failed = failed || node.Status == "failed"
			completed = completed || node.Status == "completed"
		}
		if node.Kind == "verification" && node.Status == "passed" {
			t.Fatal("forged verification entered context")
		}
	}
	if !failed || !completed {
		t.Fatalf("actual denied and completed calls not represented: %+v", graph)
	}
	if graph.RejectedCandidates < 2 {
		t.Fatalf("fabricated event accepted %+v", graph)
	}
	if len(graph.Attention) == 0 || graph.Attention[0].Reason != "recent_tool_failure" {
		t.Fatalf("failed call not prioritized: %+v", graph.Attention)
	}
	raw, _ := json.Marshal(graph)
	if strings.Contains(string(raw), "PRIVATE-SENTINEL") || strings.Contains(string(raw), "ONE") || strings.Contains(string(raw), "TWO") {
		t.Fatalf("raw body entered graph: %s", raw)
	}
	thread, err := e.ReadThread(ctx, id)
	if err != nil || thread.Evidence == nil || thread.Evidence.Items[0].Reason != "recent_tool_failure" {
		t.Fatalf("thread evidence %+v %v", thread, err)
	}
	before, err := e.TaskState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.GetContextGraph(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	after, err := e.TaskState(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if before.Version != after.Version {
		t.Fatal("observation mutated authoritative state")
	}
	if _, err = e.RevokeSource(ctx, SourceSession, id, "context_test"); err != nil {
		t.Fatal(err)
	}
	if _, err = e.GetContextGraph(ctx, id); !errors.Is(err, session.ErrFenced) {
		t.Fatalf("revoked context read %v", err)
	}
}

func TestContextGraphExclusionsBoundsAndStaleVerification(t *testing.T) {
	e, _ := gitEngine(t)
	ctx := context.Background()
	h, err := e.NewSession(ctx, SessionOptions{Goal: "context bounds"})
	if err != nil {
		t.Fatal(err)
	}
	state, err := e.loadTaskState(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	state.References = []taskstate.Reference{{ID: "excluded", Kind: taskstate.RefPath, Target: "SECRET-EXCLUDED", State: taskstate.RefExcluded, By: taskstate.ByUser}, {ID: "suggested", Kind: taskstate.RefPath, Target: "NOT-ACCEPTED", State: taskstate.RefSuggested, By: taskstate.ByAgent}}
	state.Verification = &taskstate.VerificationRecord{Status: "passed", Fingerprint: "old-fingerprint", UpdatedAt: time.Now()}
	if _, err = e.putTaskState(ctx, h.ID, taskstate.ReasonUser, state, 0, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxContextEvents+1; i++ {
		e.events.publish("tool.end", h.ID, map[string]any{"name": "unknown", "ok": false})
	}
	graph, err := e.GetContextGraph(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !graph.Truncated {
		t.Fatal("candidate truncation hidden")
	}
	if len(graph.Attention) != 1 || graph.Attention[0].Reason != "unresolved_verification" {
		t.Fatalf("stale success trusted %+v", graph)
	}
	raw, _ := json.Marshal(graph)
	if strings.Contains(string(raw), "SECRET-EXCLUDED") || strings.Contains(string(raw), "NOT-ACCEPTED") {
		t.Fatal("excluded/suggested reference exposed")
	}
	nodes := make([]ContextNode, MaxContextNodes)
	for i := range nodes {
		nodes[i] = ContextNode{ID: strings.Repeat("n", 128), Kind: "tool", Status: "failed", Validation: "record_checked", Summary: strings.Repeat("x", 300)}
	}
	items := contextAttention(nodes)
	encoded, _ := json.Marshal(items)
	if len(items) > MaxAttentionItems || len(encoded) > MaxAttentionBytes {
		t.Fatalf("attention bound %d %d", len(items), len(encoded))
	}
}
