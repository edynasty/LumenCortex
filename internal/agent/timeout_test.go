package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// BENCH003 regressions at the loop: a foreground shell call that ran past its timeout is a tool
// result the model sees, never a run exit (ADR-0019 §1), while host cancellation and the run's
// deadline stay run exits with no further model turn (ADR-0018, ADR-0019 §2).

// timedOutShell is the result the bash tool returns for a command past its timeout: a failed,
// interrupted call whose content reports the timeout and the partial output.
func timedOutShell(command string) protocol.ToolResult {
	content, _ := json.Marshal(map[string]any{
		"command":    command,
		"exitCode":   -1,
		"cancelled":  false,
		"timedOut":   true,
		"timeoutSec": 1,
		"stdout": map[string]any{
			"head": "before-timeout\n", "tail": "before-timeout\n", "total": 15, "truncated": false,
		},
		"error": "the command ran past its 1s timeout; its process group was terminated (SIGTERM, then SIGKILL). The output is partial and the command may have left partial effects.",
	})
	return protocol.ToolResult{OK: false, Interrupted: true, MutatesWorkspace: true, Content: string(content)}
}

// A command past its timeout returns one tool result the model sees, the run continues to the next
// model turn, and the call's execution is settled exactly once as outcome_unknown. The timeout is not
// the run's exit: the run completes with the model's later answer.
func TestShellTimeoutIsAToolResultAndTheRunContinues(t *testing.T) {
	store := envStore("tmo")
	files := &fakeFiles{blobs: map[string]string{}}
	const sentinel = "HIDDEN-REASONING-SENTINEL-TIMEOUT"
	var calls int
	tools := &tierTools{fn: func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		calls++
		if name != "bash" {
			t.Fatalf("tool = %q, want bash", name)
		}
		if err := ctx.Err(); err != nil {
			t.Errorf("the tool ran under a cancelled context: %v", err)
		}
		if args["timeout_sec"] != float64(1) {
			t.Errorf("args = %v, want the call's timeout_sec", args)
		}
		return timedOutShell("echo before-timeout; sleep 30"), nil
	}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{
			Message:      protocol.Message{ToolCalls: []protocol.ToolCall{call("t1", "bash", `{"command":"echo before-timeout; sleep 30","timeout_sec":1}`)}},
			FinishReason: "tool_calls",
			Echo:         protocol.NewEcho("reasoning_content", []byte(sentinel), 12),
		},
		answerOf("recovered after the timeout"),
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Snapshots: files}
	result, err := loop.Run(context.Background(), "tmo", Options{Session: testRef, MaxSteps: 3})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Final != "recovered after the timeout" {
		t.Fatalf("result = %+v, want completed with the later answer", result)
	}
	if calls != 1 {
		t.Fatalf("the shell ran %d times, want once", calls)
	}
	// Exactly one model turn after the timeout: the run did not end with the call.
	if len(provider.seen) != 2 {
		t.Fatalf("provider requests = %d, want 2 (the call, then the answer)", len(provider.seen))
	}
	// The tool result is observable: it is one row, and the next request carries it.
	rows := toolRows(store.messages)
	if len(rows) != 1 || rows[0].ToolCallID != "t1" || rows[0].Origin == protocol.OriginToolInterrupted {
		t.Fatalf("tool rows = %+v, want exactly the call's own result", rows)
	}
	for _, want := range []string{`"timedOut":true`, `"exitCode":-1`, "before-timeout", "1s timeout"} {
		if !strings.Contains(rows[0].Content, want) {
			t.Fatalf("tool row does not report %s: %s", want, rows[0].Content)
		}
	}
	var sent *protocol.Message
	for i := range provider.seen[1].Messages {
		if m := provider.seen[1].Messages[i]; m.Role == "tool" && m.ToolCallID == "t1" {
			sent = &provider.seen[1].Messages[i]
		}
	}
	if sent == nil || !strings.Contains(sent.Content, `"timedOut":true`) {
		t.Fatalf("the model was not shown the timeout result: %+v", provider.seen[1].Messages)
	}
	// One admitted execution, settled once as outcome_unknown (the command may have acted).
	if len(store.executions) != 1 || store.executions[0].post == nil {
		t.Fatalf("executions = %+v, want one settled", store.executions)
	}
	if got := store.executions[0].post.Outcome; got != session.OutcomeUnknown {
		t.Fatalf("outcome = %s, want %s", got, session.OutcomeUnknown)
	}
	if store.state.Status != StatusCompleted || store.cause != nil {
		t.Fatalf("status = %q cause = %v, want completed without a cause", store.state.Status, store.cause)
	}
	// Two steps: the call's, then the answer's. Nothing was retried or replayed.
	if len(store.steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(store.steps))
	}
	// Provider reasoning stays run memory: no row, and no step record, carries it.
	stored, _ := json.Marshal(store.messages)
	steps, _ := json.Marshal(store.steps)
	if strings.Contains(string(stored), sentinel) || strings.Contains(string(steps), sentinel) {
		t.Fatal("hidden reasoning was persisted")
	}
}

// The host's cancellation while the shell works is still a run exit: no further model turn, and the
// stopped call is answered in the same run by one terminal row carrying its partial output, with its
// execution settled outcome_unknown - Reconcile finds no gap at the next Start.
func TestHostCancellationDuringAShellCallAnswersTheCallAndEnds(t *testing.T) {
	store := envStore("cxl")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tools := &tierTools{fn: func(ctx context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
		cancel()
		<-ctx.Done()
		// The shell runner reports a killed command as a result, not an error.
		return protocol.ToolResult{OK: false, Interrupted: true, MutatesWorkspace: true, Content: `{"exitCode":-1,"cancelled":true,"stdout":{"head":"partial\n"}}`}, nil
	}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("c1", "bash", `{"command":"sleep 30","timeout_sec":30}`)),
		answerOf("must not be reached"),
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Snapshots: &fakeFiles{blobs: map[string]string{}}}
	result, err := loop.Run(ctx, "cxl", Options{Session: testRef, MaxSteps: 3})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertExit(t, store, result, StatusInterrupted, KindCancelled)
	if len(provider.seen) != 1 {
		t.Fatalf("provider requests = %d, want no turn after the cancellation", len(provider.seen))
	}
	rows := toolRows(store.messages)
	if len(rows) != 1 || rows[0].ToolCallID != "c1" || rows[0].Origin != protocol.OriginToolInterrupted {
		t.Fatalf("tool rows = %+v, want the stopped call's one terminal row", rows)
	}
	if !strings.Contains(rows[0].Content, "partial") || !strings.Contains(rows[0].Content, `"cancelled":true`) {
		t.Fatalf("the row lost the tool's stopped result: %s", rows[0].Content)
	}
	// Nothing is left unanswered, so Reconcile has no row to append.
	answered := map[string]bool{}
	for _, m := range store.messages {
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range store.messages {
		if m.Role != "assistant" {
			continue
		}
		for _, c := range m.ToolCalls {
			if !answered[c.ID] {
				t.Fatalf("call %s is left unanswered: %+v", c.ID, store.messages)
			}
		}
	}
	if len(store.executions) != 1 || store.executions[0].post == nil || store.executions[0].post.Outcome != session.OutcomeUnknown {
		t.Fatalf("executions = %+v, want one settled outcome_unknown", store.executions)
	}
}

// A run deadline that passes while the shell works ends interrupted/deadline, not cancelled, and no
// model turn follows it; the stopped call keeps its one terminal row.
func TestAgentDeadlineDuringAShellCallEndsWithoutAnotherTurn(t *testing.T) {
	store := envStore("dl")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		toolStep(call("d1", "bash", `{"command":"sleep 30","timeout_sec":30}`)),
		answerOf("must not be reached"),
	}}
	tools := &tierTools{fn: func(ctx context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
		<-ctx.Done()
		return protocol.ToolResult{OK: false, Interrupted: true, MutatesWorkspace: true, Content: `{"exitCode":-1,"timedOut":true}`}, nil
	}}
	started := time.Now()
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: tools, Snapshots: &fakeFiles{blobs: map[string]string{}}}).Run(
		context.Background(), "dl", Options{Session: testRef, MaxSteps: 4, Deadline: time.Now().Add(400 * time.Millisecond)})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the run ended %s after start, long past its deadline", elapsed)
	}
	if !errors.Is(err, ErrRunDeadline) {
		t.Fatalf("err = %v, want ErrRunDeadline", err)
	}
	assertExit(t, store, result, StatusInterrupted, KindDeadline)
	if len(provider.seen) != 1 {
		t.Fatalf("provider requests = %d, want no turn after the deadline", len(provider.seen))
	}
	rows := toolRows(store.messages)
	if len(rows) != 1 || rows[0].ToolCallID != "d1" || rows[0].Origin != protocol.OriginToolInterrupted || !strings.Contains(rows[0].Content, `"timedOut":true`) {
		t.Fatalf("tool rows = %+v, want the stopped call's one terminal row", rows)
	}
	if len(store.executions) != 1 || store.executions[0].post == nil || store.executions[0].post.Outcome != session.OutcomeUnknown {
		t.Fatalf("executions = %+v, want one settled outcome_unknown", store.executions)
	}
}
