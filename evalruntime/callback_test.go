package evalruntime

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

func echoSpec() protocol.ToolSpec {
	return protocol.ToolSpec{
		Name:        "echo",
		Description: "echo text",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
			"required":   []string{"text"},
		},
	}
}

func TestCallbackPolicyIsEnforcedByTheHost(t *testing.T) {
	h, _ := newFixtureHarness(t)
	created := h.create("fixture", "owner", "owner")
	session, err := h.m.session(created.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	caller := &fakeCaller{specs: []protocol.ToolSpec{echoSpec(), {Name: "other", Parameters: map[string]any{"type": "object"}}}}
	budget := evalapi.Budget{MaxCallbacks: 2, MaxDepth: 2, OutputBytes: 8 << 10, ArtifactBytes: 1 << 20}
	call := newActiveCall(context.Background(), session, caller, evalapi.Request{ID: "r", AllowedCapabilities: []string{"echo"}}, budget, time.Now().Add(time.Minute))
	tools, err := call.loadTools(context.Background())
	if err != nil {
		t.Fatalf("loadTools: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("tools = %+v", tools)
	}

	denied := func(req wireToolCall) string {
		t.Helper()
		value, err := call.invoke(req)
		if err != nil {
			t.Fatalf("invoke: %v", err)
		}
		result, ok := value.(map[string]any)
		if !ok {
			t.Fatalf("result type %T", value)
		}
		if result["denied"] != true {
			t.Fatalf("call %+v was not denied: %+v", req, result)
		}
		return fmt.Sprint(result["content"])
	}

	if reason := denied(wireToolCall{CallID: "a1", Name: "nope"}); !strings.Contains(reason, "not offered") {
		t.Fatalf("unknown tool reason = %q", reason)
	}
	if reason := denied(wireToolCall{CallID: "a2", Name: "other"}); !strings.Contains(reason, "not offered") {
		t.Fatalf("filtered tool reason = %q", reason)
	}
	if reason := denied(wireToolCall{CallID: "a3", Name: "echo", Arguments: map[string]any{"text": 5}}); !strings.Contains(reason, "invalid arguments") {
		t.Fatalf("schema reason = %q", reason)
	}
	if reason := denied(wireToolCall{CallID: "a4", Name: "echo", Arguments: map[string]any{}}); !strings.Contains(reason, "invalid arguments") {
		t.Fatalf("required-argument reason = %q", reason)
	}

	value, err := call.invoke(wireToolCall{CallID: "ok1", Name: "echo", Arguments: map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	result := value.(map[string]any)
	if result["ok"] != true || result["content"] != "ok" {
		t.Fatalf("allowed call = %+v", result)
	}
	if caller.callCount() != 1 {
		t.Fatalf("caller invocations = %d", caller.callCount())
	}
	if reason := denied(wireToolCall{CallID: "ok1", Name: "echo", Arguments: map[string]any{"text": "again"}}); !strings.Contains(reason, "duplicate") {
		t.Fatalf("duplicate reason = %q", reason)
	}

	// Budget: MaxCallbacks=2, one call already made.
	call.invoke(wireToolCall{CallID: "ok2", Name: "echo", Arguments: map[string]any{"text": "two"}})
	if reason := denied(wireToolCall{CallID: "ok3", Name: "echo", Arguments: map[string]any{"text": "three"}}); !strings.Contains(reason, "budget") {
		t.Fatalf("callback budget reason = %q", reason)
	}

	// Recursion depth: a callback made at the depth limit is refused.
	deepSession := session
	deep := newActiveCall(evalapi.WithCallDepth(context.Background(), budget.MaxDepth), deepSession, caller, evalapi.Request{ID: "d", AllowedCapabilities: []string{"echo"}}, budget, time.Now().Add(time.Minute))
	if _, err := deep.loadTools(context.Background()); err != nil {
		t.Fatalf("loadTools: %v", err)
	}
	value, err = deep.invoke(wireToolCall{CallID: "d1", Name: "echo", Arguments: map[string]any{"text": "x"}})
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if value.(map[string]any)["denied"] != true || !strings.Contains(fmt.Sprint(value.(map[string]any)["content"]), "depth") {
		t.Fatalf("depth guard = %+v", value)
	}

	// A failing tool is reported, not swallowed.
	failing := &fakeCaller{specs: []protocol.ToolSpec{echoSpec()}, fn: func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		return protocol.ToolResult{}, fmt.Errorf("tool exploded")
	}}
	failCall := newActiveCall(context.Background(), session, failing, evalapi.Request{ID: "f", AllowedCapabilities: []string{"echo"}}, budget, time.Now().Add(time.Minute))
	if _, err := failCall.loadTools(context.Background()); err != nil {
		t.Fatalf("loadTools: %v", err)
	}
	value, _ = failCall.invoke(wireToolCall{CallID: "f1", Name: "echo", Arguments: map[string]any{"text": "x"}})
	if value.(map[string]any)["ok"] != false || !strings.Contains(fmt.Sprint(value.(map[string]any)["content"]), "tool exploded") {
		t.Fatalf("tool failure = %+v", value)
	}

	// Result content is bounded.
	big := &fakeCaller{specs: []protocol.ToolSpec{echoSpec()}, fn: func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true, Content: strings.Repeat("x", 1<<20)}, nil
	}}
	bigCall := newActiveCall(context.Background(), session, big, evalapi.Request{ID: "b", AllowedCapabilities: []string{"echo"}}, budget, time.Now().Add(time.Minute))
	if _, err := bigCall.loadTools(context.Background()); err != nil {
		t.Fatalf("loadTools: %v", err)
	}
	value, _ = bigCall.invoke(wireToolCall{CallID: "b1", Name: "echo", Arguments: map[string]any{"text": "x"}})
	content := fmt.Sprint(value.(map[string]any)["content"])
	if len(content) > maxToolContent+200 {
		t.Fatalf("tool content was not bounded: %d bytes", len(content))
	}
}

func TestPythonToolCallbackEndToEnd(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	caller := &fakeCaller{specs: []protocol.ToolSpec{echoSpec()}, fn: func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true, Content: "echo:" + fmt.Sprint(args["text"])}, nil
	}}
	result := h.execRequest(session, "owner", evalapi.Request{
		ID:                  "cb",
		CellID:              "cell",
		Code:                `print(lc_tool("echo", text="hello"))`,
		AllowedCapabilities: []string{"echo"},
	}, caller)
	if !result.Succeeded() {
		t.Fatalf("callback cell failed: %+v", result)
	}
	if !strings.Contains(result.Stdout, "echo:hello") {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if caller.callCount() != 1 {
		t.Fatalf("caller invocations = %d", caller.callCount())
	}
	if len(h.events.byType(evalapi.EventToolCall)) == 0 {
		t.Fatalf("no tool_call audit event")
	}

	// A tool the worker does not hold is refused before any host call.
	unknown := h.execRequest(session, "owner", evalapi.Request{
		ID:                  "cb-unknown",
		CellID:              "cell",
		Code:                `lc_tool("bash", command="id")`,
		AllowedCapabilities: []string{"echo"},
	}, caller)
	if unknown.Status != evalapi.ResultStatusFailed || !strings.Contains(unknown.Error, "ToolDenied") {
		t.Fatalf("unauthorized callback = %+v", unknown)
	}
	if caller.callCount() != 1 {
		t.Fatalf("unauthorized call reached the caller")
	}

	// A schema-invalid call reaches the host bridge and is denied there.
	invalid := h.execRequest(session, "owner", evalapi.Request{
		ID:                  "cb-invalid",
		CellID:              "cell",
		Code:                `lc_tool("echo", text=123)`,
		AllowedCapabilities: []string{"echo"},
	}, caller)
	if invalid.Status != evalapi.ResultStatusFailed || !strings.Contains(invalid.Error, "ToolDenied") {
		t.Fatalf("schema-invalid callback = %+v", invalid)
	}
	if len(h.events.byType(evalapi.EventToolDenied)) == 0 {
		t.Fatalf("no tool_denied audit event")
	}

	noCaller := h.execRequest(session, "owner", evalapi.Request{
		ID:                  "cb-nocaller",
		CellID:              "cell",
		Code:                `lc_tool("echo", text="x")`,
		AllowedCapabilities: []string{"echo"},
	}, nil)
	if noCaller.Status != evalapi.ResultStatusFailed || !strings.Contains(noCaller.Error, "ToolDenied") {
		t.Fatalf("callback without a caller = %+v", noCaller)
	}
}

func TestCallbackCancelPropagates(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	started := make(chan struct{})
	caller := &fakeCaller{specs: []protocol.ToolSpec{echoSpec()}, fn: func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		close(started)
		<-ctx.Done()
		return protocol.ToolResult{OK: false, Interrupted: true, Content: "cancelled"}, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	result, err := h.m.Execute(ctx, evalapi.Request{
		ID:                  "cb-cancel",
		SessionID:           session.ID,
		CellID:              "cell",
		AgentID:             "owner",
		Code:                `lc_tool("echo", text="wait")`,
		AllowedCapabilities: []string{"echo"},
	}, caller)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Status != evalapi.ResultStatusCancelled && result.Status != evalapi.ResultStatusTimeout {
		t.Fatalf("status = %q", result.Status)
	}
	if result.StateStatus != evalapi.StateStatusOK && result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("state = %q", result.StateStatus)
	}
}
