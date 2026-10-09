package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider/openai"
)

// These tests cover the run-exit contract (design §4.1, invariant I1) through the real
// engine: SQLite store, real tools, the run supervisor.

type exitProvider struct {
	n  int
	fn func(ctx context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error)
}

func (p *exitProvider) Model() string { return "exit-test" }

func (p *exitProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.n++
	return p.fn(ctx, p.n, req)
}

type sessionCause struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	At      string `json:"at"`
}

func openTestEngine(t *testing.T) *Engine {
	t.Helper()
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func newTestSession(t *testing.T, engine *Engine, goal string) *SessionHandle {
	t.Helper()
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: goal})
	if err != nil {
		t.Fatal(err)
	}
	return handle
}

func sessionState(t *testing.T, engine *Engine, id string) (SessionInfo, *sessionCause) {
	t.Helper()
	_, info, err := engine.Session(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.Error) == 0 {
		return info, nil
	}
	var cause sessionCause
	if err := json.Unmarshal(info.Error, &cause); err != nil {
		t.Fatalf("error_json %s: %v", info.Error, err)
	}
	return info, &cause
}

func transcript(t *testing.T, handle *SessionHandle) []protocol.Message {
	t.Helper()
	rows, err := handle.RecentMessages(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]protocol.Message, 0, len(rows))
	for _, row := range rows {
		var message protocol.Message
		if err := json.Unmarshal(row.JSON, &message); err != nil {
			t.Fatalf("row %d: %v", row.Seq, err)
		}
		out = append(out, message)
	}
	return out
}

func waitForEvent(t *testing.T, events <-chan Event, eventType string) Event {
	t.Helper()
	return waitForEventWith(t, events, eventType, "")
}

// waitForEventWith waits for an event of eventType whose data contains text.
func waitForEventWith(t *testing.T, events <-chan Event, eventType, text string) Event {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Type == eventType && strings.Contains(string(event.Data), text) {
				return event
			}
		case <-timeout:
			t.Fatalf("no %q event containing %q", eventType, text)
		}
	}
}

func waitUntilIdle(t *testing.T, supervisor *RunSupervisor, id string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for supervisor.Active(id) {
		if time.Now().After(deadline) {
			t.Fatalf("run %s still active after %v", id, within)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func completingProvider(final string) *exitProvider {
	return &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{Content: final}}, nil
	}}
}

// F4: a tool call whose arguments are not valid JSON used to end RunAgent before anything was
// saved and left the session running. The call is now kept as a JSON string, never executed,
// answered with a tool error, and the run continues to completion.
func TestInvalidToolArgumentsDoNotStrandRun(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "write a file")
	workspace := engine.workspace
	invalid := `{"path":"created.txt","content":"x"`

	var second protocol.ProviderRequest
	provider := &exitProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 1 {
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				{ID: "bad-1", Name: "write_file", Arguments: json.RawMessage(invalid)},
			}}, FinishReason: "tool_calls"}, nil
		}
		second = req
		return protocol.ProviderResponse{Message: protocol.Message{Content: "retry later"}}, nil
	}}
	result, err := engine.RunAgent(context.Background(), handle.ID, provider, AgentOptions{Policy: "workspace", MaxSteps: 3})
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if result.Status != "completed" || result.Final != "retry later" {
		t.Fatalf("result = %+v", result)
	}
	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "completed" || cause != nil {
		t.Fatalf("status=%q cause=%+v", info.Status, cause)
	}
	if _, statErr := os.Stat(filepath.Join(workspace, "created.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("write_file ran with invalid arguments (stat err = %v)", statErr)
	}

	rows := transcript(t, handle)
	if len(rows) != 4 || rows[1].Role != "assistant" || rows[2].Role != "tool" || rows[3].Role != "assistant" {
		t.Fatalf("transcript = %+v", rows)
	}
	var stored string
	if err := json.Unmarshal(rows[1].ToolCalls[0].Arguments, &stored); err != nil || stored != invalid {
		t.Fatalf("stored arguments = %s, want the raw text as a JSON string (err %v)", rows[1].ToolCalls[0].Arguments, err)
	}
	if rows[2].ToolCallID != "bad-1" || !strings.Contains(rows[2].Content, "invalid tool arguments") {
		t.Fatalf("tool row = %+v", rows[2])
	}
	// The model sees its own call and the error in the next request.
	var sawCall, sawError bool
	for _, message := range second.Messages {
		sawCall = sawCall || (message.Role == "assistant" && len(message.ToolCalls) == 1 && message.ToolCalls[0].ID == "bad-1")
		sawError = sawError || (message.Role == "tool" && message.ToolCallID == "bad-1" && strings.Contains(message.Content, "invalid tool arguments"))
	}
	if !sawCall || !sawError {
		t.Fatalf("second request lacks the call (%v) or its error (%v): %+v", sawCall, sawError, second.Messages)
	}
}

// The model must be shown its own argument text again, byte for byte, whatever it sent,
// and only a JSON object may run the tool. The run goes through the OpenAI-compatible
// adapter, the loop and SQLite, so the replayed request is built from the stored rows.
// The response also carries provider reasoning, which must reach neither the database
// nor a later request (AGENTS.md reasoning boundary).
func TestToolArgumentTextReplaysExactlyThroughOpenAIAdapter(t *testing.T) {
	const reasoning = "HIDDEN-REASONING-SENTINEL"
	cases := []struct {
		name     string
		text     string // function.arguments as the model sent it
		executes bool
	}{
		{"object", `{"path":"."}`, true},
		{"truncated object", `{"path":"a<b>&c"`, false},
		{"plain text", `list everything`, false},
		{"JSON string", `"foo"`, false},
		{"double-encoded object", `"{\"path\":\".\"}"`, false},
		{"null", `null`, false},
		{"number", `42`, false},
		{"array", `["."]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu       sync.Mutex
				requests [][]byte
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				requests = append(requests, body)
				first := len(requests) == 1
				mu.Unlock()
				message := map[string]any{"role": "assistant", "content": "done"}
				finish := "stop"
				if first {
					message = map[string]any{"role": "assistant", "reasoning_content": reasoning, "tool_calls": []any{map[string]any{
						"id": "call-1", "type": "function", "function": map[string]any{"name": "list_dir", "arguments": tc.text},
					}}}
					finish = "tool_calls"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": finish}}})
			}))
			defer server.Close()
			client, err := openai.New(openai.Config{Endpoint: server.URL, Model: "wire-test", DisableStreaming: true, DisableRetries: true})
			if err != nil {
				t.Fatal(err)
			}
			engine := openTestEngine(t)
			handle := newTestSession(t, engine, "list the workspace")

			result, err := engine.RunAgent(context.Background(), handle.ID, client, AgentOptions{MaxSteps: 3})
			if err != nil || result.Status != "completed" {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(requests) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(requests))
			}
			var replay struct {
				Messages []struct {
					Role       string `json:"role"`
					Content    string `json:"content"`
					ToolCallID string `json:"tool_call_id"`
					ToolCalls  []struct {
						ID       string `json:"id"`
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"messages"`
			}
			if err := json.Unmarshal(requests[1], &replay); err != nil {
				t.Fatal(err)
			}
			var sentArgs, toolResult *string
			for i := range replay.Messages {
				message := &replay.Messages[i]
				if message.Role == "assistant" && len(message.ToolCalls) == 1 && message.ToolCalls[0].ID == "call-1" {
					sentArgs = &message.ToolCalls[0].Function.Arguments
				}
				if message.Role == "tool" && message.ToolCallID == "call-1" {
					toolResult = &message.Content
				}
			}
			if sentArgs == nil || toolResult == nil {
				t.Fatalf("replayed request lacks the call or its result: %s", requests[1])
			}
			if *sentArgs != tc.text {
				t.Fatalf("replayed arguments = %q, want the model's text %q", *sentArgs, tc.text)
			}
			if rejected := strings.Contains(*toolResult, "invalid tool arguments"); rejected == tc.executes {
				t.Fatalf("executes = %v, but the tool result is %s", tc.executes, *toolResult)
			}

			if bytes.Contains(requests[1], []byte(reasoning)) {
				t.Fatalf("provider reasoning was sent back: %s", requests[1])
			}
			for _, path := range []string{engine.store.Path(), engine.store.Path() + "-wal"} {
				raw, err := os.ReadFile(path)
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
				if bytes.Contains(raw, []byte(reasoning)) {
					t.Fatalf("provider reasoning stored in %s", filepath.Base(path))
				}
			}
		})
	}
}

// Audit E4: cancelling a run while a real `shell` call runs `sleep 5` must end it
// interrupted/cancelled instead of stranding it as running, kill the command instead of
// waiting for it, and save no result for the stopped call. The command prints a line and
// then becomes `sleep 5`; Cancel waits for that line. Cancelling at tool.start instead
// lands before the process exists in most runs and only tests a refused process start.
func TestCancelDuringShellSleepEndsInterruptedCancelled(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "sleep")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	events, unsubscribe := engine.Events(512)
	defer unsubscribe()

	provider := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "sleep-1", Name: "shell", Arguments: json.RawMessage(`{"command":"echo ready && exec sleep 5"}`)},
		}}, FinishReason: "tool_calls"}, nil
	}}
	if _, err := supervisor.Start(context.Background(), handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitForEventWith(t, events, "tool.output", "ready")
	cancelledAt := time.Now()
	if !supervisor.Cancel(handle.ID) {
		t.Fatal("Cancel reported no active run")
	}
	waitUntilIdle(t, supervisor, handle.ID, 15*time.Second)
	if elapsed := time.Since(cancelledAt); elapsed > 4*time.Second {
		t.Fatalf("the run took %v to stop after Cancel; the 5 s command was waited for, not killed", elapsed)
	}

	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "interrupted" {
		t.Fatalf("status = %q, want interrupted", info.Status)
	}
	if cause == nil || cause.Kind != "cancelled" || cause.Message == "" || cause.At == "" {
		t.Fatalf("cause = %+v", cause)
	}
	for _, message := range transcript(t, handle) {
		if message.Role == "tool" {
			t.Fatalf("the stopped call got a result row: %+v", message)
		}
	}
	stopped := waitForEvent(t, events, "run.stopped")
	if !strings.Contains(string(stopped.Data), "canceled") {
		t.Fatalf("run.stopped = %s", stopped.Data)
	}
}

// A panic in the run (here in the provider) must end the run failed/panic, release the
// supervisor slot, and leave the process and the session usable.
func TestPanicFinalizesRunAsFailed(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "panic")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	events, unsubscribe := engine.Events(512)
	defer unsubscribe()

	exploding := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		panic("provider exploded")
	}}
	if _, err := supervisor.Start(context.Background(), handle.ID, exploding, AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)

	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "failed" {
		t.Fatalf("status = %q, want failed", info.Status)
	}
	if cause == nil || cause.Kind != "panic" || !strings.Contains(cause.Message, "provider exploded") {
		t.Fatalf("cause = %+v", cause)
	}
	stopped := waitForEvent(t, events, "run.stopped")
	if !strings.Contains(string(stopped.Data), "provider exploded") {
		t.Fatalf("run.stopped = %s", stopped.Data)
	}

	// failed is "start after a change": the next Start clears the cause and completes.
	if _, err := supervisor.Start(context.Background(), handle.ID, completingProvider("fine now"), AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
	info, cause = sessionState(t, engine, handle.ID)
	if info.Status != "completed" || cause != nil {
		t.Fatalf("after restart: status=%q cause=%+v", info.Status, cause)
	}
}

// If Finish itself fails (here: a trigger standing in for a full disk), the supervisor
// still releases the run and reports the failure; the row stays running, and the next
// Start runs normally and ends in a proper terminal state.
func TestFinishFailureThenStart(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "finish fails")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	events, unsubscribe := engine.Events(512)
	defer unsubscribe()

	side, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer side.Close()
	if _, err := side.Exec(`CREATE TRIGGER inject_finish_failure BEFORE UPDATE OF status ON sessions
WHEN NEW.status IN ('interrupted', 'failed', 'completed')
BEGIN SELECT RAISE(ABORT, 'database or disk is full (injected)'); END`); err != nil {
		t.Fatal(err)
	}

	blocking := &blockingProvider{started: make(chan struct{})}
	if _, err := supervisor.Start(context.Background(), handle.ID, blocking, AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocking.started:
	case <-time.After(10 * time.Second):
		t.Fatal("provider never called")
	}
	supervisor.Cancel(handle.ID)
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)

	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "running" || cause != nil {
		t.Fatalf("after the failed Finish: status=%q cause=%+v, want the untouched running row", info.Status, cause)
	}
	stopped := waitForEvent(t, events, "run.stopped")
	if !strings.Contains(string(stopped.Data), "injected") || !strings.Contains(string(stopped.Data), "canceled") {
		t.Fatalf("run.stopped must carry the cause and the failed write: %s", stopped.Data)
	}

	if _, err := side.Exec(`DROP TRIGGER inject_finish_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Start(context.Background(), handle.ID, completingProvider("second run"), AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatalf("Start after a failed Finish: %v", err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
	info, cause = sessionState(t, engine, handle.ID)
	if info.Status != "completed" || cause != nil || info.Final == nil || *info.Final != "second run" {
		t.Fatalf("second run: status=%q cause=%+v final=%v", info.Status, cause, info.Final)
	}
}
