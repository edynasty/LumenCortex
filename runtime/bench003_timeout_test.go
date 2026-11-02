package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/protocol"
)

// BENCH003 through the real Engine, SQLite and shell: a foreground command past its timeout is one
// observable tool result the model continues from, while cancellation and the run deadline stay run
// exits that answer the stopped call before the run ends.

type shellReport struct {
	ExitCode  int    `json:"exitCode"`
	Cancelled bool   `json:"cancelled"`
	TimedOut  bool   `json:"timedOut"`
	Error     string `json:"error"`
	Stdout    struct {
		Head string `json:"head"`
	} `json:"stdout"`
}

func decodeShellReport(t *testing.T, content string) shellReport {
	t.Helper()
	var report shellReport
	if err := json.Unmarshal([]byte(content), &report); err != nil {
		t.Fatalf("shell result %q: %v", content, err)
	}
	return report
}

func rowsFor(messages []protocol.Message, role string) []protocol.Message {
	var out []protocol.Message
	for _, m := range messages {
		if m.Role == role {
			out = append(out, m)
		}
	}
	return out
}

// The configured default bounds a command that names no timeout_sec: the call returns timedOut with
// its partial output, the model's next turn sees that result, the run completes, and the call has
// exactly one execution row, outcome_unknown. No hidden reasoning from the tool-call turn is stored.
func TestShellTimeoutIsAToolResultAndTheRunContinues(t *testing.T) {
	useGlobalConfig(t, strings.Replace(boundCatalog("test/b0"), `{"model"`,
		`{"execution": {"shellTimeoutSeconds": 1}, "model"`, 1))
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "run a slow command")

	const sentinel = "HIDDEN-REASONING-SENTINEL-TIMEOUT"
	var (
		mu       sync.Mutex
		requests []protocol.ProviderRequest
	)
	provider := &exitProvider{fn: func(_ context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		mu.Lock()
		requests = append(requests, req)
		mu.Unlock()
		if n == 1 {
			response := toolCalls(tc("sh-1", "bash", `{"command":"echo before-timeout; sleep 30"}`))
			response.Echo = protocol.NewEcho("reasoning_content", []byte(sentinel), 12)
			return response, nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "recovered after the timeout"}, FinishReason: "stop"}, nil
	}}

	started := time.Now()
	result, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 4})
	elapsed := time.Since(started)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Final != "recovered after the timeout" {
		t.Fatalf("result = %+v, want completed with the later answer", result)
	}
	if elapsed > 15*time.Second {
		t.Fatalf("the run took %v; the 30 s command was not stopped at its 1 s default timeout", elapsed)
	}
	if provider.n != 2 {
		t.Fatalf("provider requests = %d, want 2 (the call, then the answer)", provider.n)
	}
	if got := lastToolResult(requests[1], "sh-1"); !strings.Contains(got, `"timedOut":true`) {
		t.Fatalf("the model was not shown the timeout result: %+v", requests[1].Messages)
	}

	rows := transcript(t, handle)
	tools := rowsFor(rows, "tool")
	if len(tools) != 1 || tools[0].ToolCallID != "sh-1" || tools[0].Origin == protocol.OriginToolInterrupted {
		t.Fatalf("tool rows = %+v, want exactly the call's own result", tools)
	}
	report := decodeShellReport(t, tools[0].Content)
	if !report.TimedOut || report.Cancelled || report.ExitCode != -1 {
		t.Fatalf("shell report = %+v, want timedOut, not cancelled, exit -1", report)
	}
	if !strings.Contains(report.Stdout.Head, "before-timeout") || !strings.Contains(report.Error, "timeout") {
		t.Fatalf("the partial output or the reason is missing: %+v", report)
	}
	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "completed" || cause != nil {
		t.Fatalf("status = %q cause = %+v, want completed without a cause", info.Status, cause)
	}

	execs, _ := readProvenance(t, engine, handle.ID)
	if len(execs) != 1 || execs[0].Tool != "bash" || execs[0].Outcome != "outcome_unknown" {
		t.Fatalf("executions = %+v, want one bash call settled outcome_unknown", execs)
	}

	mu.Lock()
	sent := requests[1]
	mu.Unlock()
	encoded, _ := json.Marshal(sent.Messages)
	if bytes.Contains(encoded, []byte(sentinel)) {
		t.Fatal("provider reasoning was sent back in a message")
	}
	for _, path := range []string{engine.store.Path(), engine.store.Path() + "-wal"} {
		raw, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(sentinel)) {
			t.Fatalf("provider reasoning stored in %s", filepath.Base(path))
		}
	}
}

// Two sessions run at once: the first command's timeout kills only its own process group. The second
// session's command runs to completion and the run finishes normally.
func TestShellTimeoutKillsOnlyItsOwnProcessGroup(t *testing.T) {
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	slow := newTestSession(t, engine, "slow command")
	other := newTestSession(t, engine, "other command")
	marker := filepath.Join(engine.workspace, "other-survived")

	slowProvider := &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 1 {
			return toolCalls(tc("slow-1", "bash", `{"command":"echo slow-started; exec sleep 30","timeout_sec":1}`)), nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "slow done"}, FinishReason: "stop"}, nil
	}}
	otherProvider := &exitProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n == 1 {
			return toolCalls(tc("other-1", "bash", `{"command":"sleep 3; echo survivor > other-survived","timeout_sec":20}`)), nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "other done"}, FinishReason: "stop"}, nil
	}}

	if _, err := supervisor.startWith(context.Background(), slow.ID, slowProvider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.startWith(context.Background(), other.ID, otherProvider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, slow.ID, 20*time.Second)
	waitUntilIdle(t, supervisor, other.ID, 20*time.Second)

	slowRows := rowsFor(transcript(t, slow), "tool")
	if len(slowRows) != 1 {
		t.Fatalf("slow session tool rows = %+v", slowRows)
	}
	if report := decodeShellReport(t, slowRows[0].Content); !report.TimedOut || report.ExitCode != -1 || !strings.Contains(report.Stdout.Head, "slow-started") {
		t.Fatalf("slow session result = %+v, want timedOut with partial output", report)
	}
	otherRows := rowsFor(transcript(t, other), "tool")
	if len(otherRows) != 1 {
		t.Fatalf("other session tool rows = %+v", otherRows)
	}
	if report := decodeShellReport(t, otherRows[0].Content); report.TimedOut || report.ExitCode != 0 {
		t.Fatalf("the timeout of the other session's call reached this one: %+v", report)
	}
	body, err := os.ReadFile(marker)
	if err != nil || strings.TrimSpace(string(body)) != "survivor" {
		t.Fatalf("the other session's command did not finish: %q err=%v", body, err)
	}
	if info, cause := sessionState(t, engine, other.ID); info.Status != "completed" || cause != nil {
		t.Fatalf("other session = %q cause %+v, want completed", info.Status, cause)
	}
}

// The host's cancellation during a shell call ends the run interrupted/cancelled and answers the
// stopped call in that run: one tool row with the partial output, its execution settled
// outcome_unknown, and no model turn afterwards. The next Start therefore appends no reconcile row.
func TestCancelledShellCallIsClosedByTheRunAndNotReconcile(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "cancel me")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	events, unsubscribe := engine.Events(512)
	defer unsubscribe()

	var cancelledCalls int
	cancelledProvider := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		cancelledCalls++
		return toolCalls(tc("sh-1", "bash", `{"command":"echo ready; exec sleep 30","timeout_sec":60}`)), nil
	}}
	if _, err := supervisor.startWith(context.Background(), handle.ID, cancelledProvider, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitForEventWith(t, events, "tool.output", "ready")
	if !supervisor.Cancel(handle.ID) {
		t.Fatal("Cancel reported no active run")
	}
	waitUntilIdle(t, supervisor, handle.ID, 15*time.Second)

	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "interrupted" || cause == nil || cause.Kind != "cancelled" {
		t.Fatalf("status = %q cause = %+v, want interrupted/cancelled", info.Status, cause)
	}
	if cancelledCalls != 1 {
		t.Fatalf("provider requests = %d, want no turn after the cancellation", cancelledCalls)
	}
	stopped := rowsFor(transcript(t, handle), "tool")
	if len(stopped) != 1 || stopped[0].ToolCallID != "sh-1" || stopped[0].Origin != protocol.OriginToolInterrupted {
		t.Fatalf("tool rows = %+v, want the stopped call's one terminal row", stopped)
	}
	report := decodeShellReport(t, stopped[0].Content)
	if !report.Cancelled || report.TimedOut || report.ExitCode != -1 {
		t.Fatalf("shell report = %+v, want cancelled, not timed out, exit -1", report)
	}
	if !strings.Contains(report.Stdout.Head, "ready") {
		t.Fatalf("the partial output before the cancellation is missing: %q", report.Stdout.Head)
	}
	execs, _ := readProvenance(t, engine, handle.ID)
	if len(execs) != 1 || execs[0].Outcome != "outcome_unknown" || execs[0].Post.Valid {
		t.Fatalf("executions = %+v, want one settled outcome_unknown without a post record", execs)
	}
	assertPaired(t, transcript(t, handle))

	var followUp protocol.ProviderRequest
	follow := &exitProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		followUp = req
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "continued"}, FinishReason: "stop"}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), handle.ID, follow, AgentOptions{Policy: "full", MaxSteps: 2, Input: "continue"}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)

	rows := transcript(t, handle)
	assertPaired(t, rows)
	if tools := rowsFor(rows, "tool"); len(tools) != 1 {
		t.Fatalf("Reconcile appended another row for the answered call: %+v", tools)
	}
	if got := lastToolResult(followUp, "sh-1"); !strings.Contains(got, `"cancelled":true`) {
		t.Fatalf("the resumed request lacks the cancelled result: %+v", followUp.Messages)
	}
	execs, _ = readProvenance(t, engine, handle.ID)
	if len(execs) != 1 || execs[0].Outcome != "outcome_unknown" {
		t.Fatalf("executions = %+v, want the one settled row unchanged", execs)
	}
	if info, cause := sessionState(t, engine, handle.ID); info.Status != "completed" || cause != nil {
		t.Fatalf("resumed session = %q cause %+v, want completed", info.Status, cause)
	}
}

// The run's deadline that passes during a shell call ends interrupted/deadline, never cancelled: the
// call's own timeout result is stored once (it ran into the deadline reserve), no model turn starts
// after the deadline, and the execution is settled once.
func TestRunDeadlineDuringShellEndsDeadlineWithoutATurnAfterIt(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "deadline")
	deadline := time.Now().Add(2 * time.Second)
	var (
		mu     sync.Mutex
		starts []time.Time
	)
	provider := &exitProvider{fn: func(ctx context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		mu.Lock()
		starts = append(starts, time.Now())
		mu.Unlock()
		if n == 1 {
			return toolCalls(tc("d-1", "bash", `{"command":"echo begun; exec sleep 30","timeout_sec":30}`)), nil
		}
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}}

	started := time.Now()
	result, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 4, Deadline: &deadline})
	if elapsed := time.Since(started); elapsed > 15*time.Second {
		t.Fatalf("the run took %v, long past its 2 s deadline", elapsed)
	}
	if !errors.Is(err, agent.ErrRunDeadline) {
		t.Fatalf("err = %v, want ErrRunDeadline", err)
	}
	if result.Status != "interrupted" {
		t.Fatalf("result = %+v, want interrupted", result)
	}
	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "interrupted" || cause == nil || cause.Kind != "deadline" {
		t.Fatalf("status = %q cause = %+v, want interrupted/deadline", info.Status, cause)
	}
	mu.Lock()
	for _, at := range starts {
		if at.After(deadline) {
			t.Errorf("a model turn started at %s, after the deadline %s", at, deadline)
		}
	}
	turns := len(starts)
	mu.Unlock()
	if turns > 2 {
		t.Fatalf("provider requests = %d, want at most the call's turn and one cut by the deadline", turns)
	}
	tools := rowsFor(transcript(t, handle), "tool")
	if len(tools) != 1 || tools[0].ToolCallID != "d-1" {
		t.Fatalf("tool rows = %+v, want the call's one timeout result", tools)
	}
	report := decodeShellReport(t, tools[0].Content)
	if !report.TimedOut || report.Cancelled || report.ExitCode != -1 {
		t.Fatalf("shell report = %+v, want the deadline-bounded timeout, not a cancellation", report)
	}
	execs, _ := readProvenance(t, engine, handle.ID)
	if len(execs) != 1 || execs[0].Outcome != "outcome_unknown" {
		t.Fatalf("executions = %+v, want one settled outcome_unknown", execs)
	}
}
