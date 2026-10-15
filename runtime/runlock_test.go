package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/runlock"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// These tests cover M1.2 (design §4.2, §4.9, invariant I2): the cross-process run
// lock, Reconcile at every Start and Open, AgentOptions.Input, and the first row
// NewSession writes.

// blockedProvider never answers until the run is cancelled, so the run stays active.
func blockedProvider() *exitProvider {
	return &exitProvider{fn: func(ctx context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}}
}

// startBlockedRun starts a run that holds the session's run lock until cleanup.
func startBlockedRun(t *testing.T, engine *Engine, sessionID string) *RunSupervisor {
	t.Helper()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(supervisor.Close)
	if _, err := supervisor.startWith(context.Background(), sessionID, blockedProvider(), AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	return supervisor
}

func waitForRunStopped(t *testing.T, events <-chan Event, sessionID string) {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case event := <-events:
			if event.Type == "run.stopped" && event.SessionID == sessionID {
				return
			}
		case <-timeout:
			t.Fatalf("no run.stopped event for %s", sessionID)
		}
	}
}

func openEngineAt(t *testing.T, workspace string) *Engine {
	t.Helper()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func rowCount(t *testing.T, handle *SessionHandle) int {
	t.Helper()
	return len(transcript(t, handle))
}

// assertPaired fails unless every assistant row with tool calls is followed, before
// any other row, by a tool row for each of its calls.
func assertPaired(t *testing.T, messages []protocol.Message) {
	t.Helper()
	for i, message := range messages {
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			continue
		}
		answered := map[string]bool{}
		for _, next := range messages[i+1:] {
			if next.Role != "tool" {
				break
			}
			answered[next.ToolCallID] = true
		}
		for _, call := range message.ToolCalls {
			if !answered[call.ID] {
				t.Fatalf("row %d: call %s has no result before the next non-tool row; transcript: %+v", i, call.ID, messages)
			}
		}
	}
}

// A2 inverted: the old RecoverStaleRuns turned every running row interrupted. With
// the run lock, an Engine that does not own a run can neither recover it at Open nor
// start over it, and writes nothing.
func TestEngineCannotRecoverOrClobberARunHeldElsewhere(t *testing.T) {
	workspace := t.TempDir()
	owner := openEngineAt(t, workspace)
	handle := newTestSession(t, owner, "long task")
	startBlockedRun(t, owner, handle.ID)
	before := rowCount(t, handle)

	// A second Engine in the same process opens the same database: Open reconciles
	// running sessions only when it can take their free lock.
	other := openEngineAt(t, workspace)
	info, cause := sessionState(t, other, handle.ID)
	if info.Status != "running" || cause != nil {
		t.Fatalf("Open recovered a run held by another Engine: status=%q cause=%+v", info.Status, cause)
	}

	otherSupervisor, err := NewRunSupervisor(other)
	if err != nil {
		t.Fatal(err)
	}
	defer otherSupervisor.Close()
	_, err = otherSupervisor.startWith(context.Background(), handle.ID, completingProvider("x"), AgentOptions{Input: "must not be appended"})
	if !errors.Is(err, ErrRunHeld) {
		t.Fatalf("Start over a held run: err=%v, want ErrRunHeld", err)
	}
	if _, err := other.runAgentWith(context.Background(), handle.ID, completingProvider("x"), AgentOptions{Input: "nor this"}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("RunAgent over a held run: err=%v, want ErrRunHeld", err)
	}
	if got := rowCount(t, handle); got != before {
		t.Fatalf("a refused Start wrote transcript rows: %d -> %d", before, got)
	}
	if info, cause := sessionState(t, other, handle.ID); info.Status != "running" || cause != nil {
		t.Fatalf("a refused Start changed the session: status=%q cause=%+v", info.Status, cause)
	}
}

func TestSecondStartOnTheSameSupervisorIsErrRunHeld(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "g")
	supervisor := startBlockedRun(t, engine, handle.ID)
	_, err := supervisor.startWith(context.Background(), handle.ID, completingProvider("x"), AgentOptions{Input: "second"})
	if !errors.Is(err, ErrRunHeld) {
		t.Fatalf("err=%v, want ErrRunHeld", err)
	}
	if got := rowCount(t, handle); got != 1 {
		t.Fatalf("the refused Start appended rows: %d", got)
	}
}

func TestApprovalAndDeleteDuringARunAreErrRunHeld(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "g")
	startBlockedRun(t, engine, handle.ID)

	if _, err := engine.ApproveWorkflowGate(context.Background(), handle.ID, "ship", "alice"); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("ApproveWorkflowGate during a run: err=%v, want ErrRunHeld", err)
	}
	if _, err := engine.DeleteSession(context.Background(), handle.ID, WorktreeCleanup{}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("DeleteSession during a run: err=%v, want ErrRunHeld", err)
	}
	if _, _, err := engine.Session(context.Background(), handle.ID); err != nil {
		t.Fatalf("the refused delete removed the session: %v", err)
	}
}

func TestDeleteSessionRemovesRowsAndLockFile(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "g")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	if _, err := supervisor.startWith(context.Background(), handle.ID, completingProvider("done"), AgentOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)
	lockFile := filepath.Join(engine.runDir(), handle.ID+".lock")
	if _, err := os.Stat(lockFile); err != nil {
		t.Fatalf("a finished run should leave its lock file: %v", err)
	}

	if _, err := engine.DeleteSession(context.Background(), handle.ID, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := engine.Session(context.Background(), handle.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("session after delete: err=%v", err)
	}
	if _, err := os.Stat(lockFile); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file survived the delete: %v", err)
	}
	if _, err := engine.DeleteSession(context.Background(), handle.ID, WorktreeCleanup{}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("second delete: err=%v, want ErrNotFound", err)
	}
}

// The lock is released on the normal, the error and the cancel path.
func TestRunLockIsReleasedOnEveryExitPath(t *testing.T) {
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	cases := map[string]struct {
		provider *exitProvider
		cancel   bool
	}{
		"completed": {provider: completingProvider("done")},
		"failed": {provider: &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			return protocol.ProviderResponse{}, errors.New("provider down")
		}}},
		"panic":     {provider: &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) { panic("boom") }}},
		"cancelled": {provider: blockedProvider(), cancel: true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			handle := newTestSession(t, engine, name)
			events, unsubscribe := engine.Events(256)
			defer unsubscribe()
			if _, err := supervisor.startWith(context.Background(), handle.ID, tc.provider, AgentOptions{MaxSteps: 2}); err != nil {
				t.Fatal(err)
			}
			if tc.cancel {
				supervisor.Cancel(handle.ID)
			}
			// A host that sees run.stopped must be able to take the lock at once.
			waitForRunStopped(t, events, handle.ID)
			lock, err := runlock.TryAcquire(engine.runDir(), handle.ID)
			if err != nil {
				t.Fatalf("lock still held after run.stopped: %v", err)
			}
			lock.Release()
		})
	}
}

func TestStartAppendsNothingWhenValidationFails(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "g")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	bad := []AgentOptions{
		{Input: "   \n\t"},
		{Input: "follow-up", Policy: "nonsense"},
		{Input: "follow-up", Workflow: json.RawMessage(`{not json`)},
	}
	for _, opts := range bad {
		if _, err := supervisor.startWith(context.Background(), handle.ID, completingProvider("x"), opts); err == nil {
			t.Fatalf("Start accepted %+v", opts)
		}
	}
	if got := rowCount(t, handle); got != 1 {
		t.Fatalf("a rejected Start appended rows: %d", got)
	}
	// The failed Starts released the lock.
	if _, err := supervisor.startWith(context.Background(), handle.ID, completingProvider("x"), AgentOptions{Input: "ok"}); err != nil {
		t.Fatalf("Start after rejected Starts: %v", err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)
}

func TestNewSessionRejectsBlankGoals(t *testing.T) {
	engine := openTestEngine(t)
	for _, goal := range []string{"", " ", "\n\t ", "\u3000", "\u00a0\u2003"} {
		if _, err := engine.NewSession(context.Background(), SessionOptions{Goal: goal, Input: "not a goal"}); err == nil {
			t.Errorf("NewSession accepted the blank goal %q", goal)
		}
	}
	sessions, err := engine.ListSessions(context.Background(), 10, 0)
	if err != nil || len(sessions) != 0 {
		t.Fatalf("sessions=%d err=%v; a rejected goal must create nothing", len(sessions), err)
	}
}

func TestNewSessionWritesExactlyTheFirstRow(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()

	byGoal, err := engine.NewSession(ctx, SessionOptions{Goal: "fix the bug"})
	if err != nil {
		t.Fatal(err)
	}
	rows := transcript(t, byGoal)
	if len(rows) != 1 || rows[0].Role != "user" || rows[0].Content != "fix the bug" || rows[0].Origin != "" {
		t.Fatalf("goal first row = %+v", rows)
	}

	byInput, err := engine.NewSession(ctx, SessionOptions{Goal: "fix the bug", Input: "fix the bug\n\nContext: a.go"})
	if err != nil {
		t.Fatal(err)
	}
	rows = transcript(t, byInput)
	if len(rows) != 1 || rows[0].Role != "user" || rows[0].Content != "fix the bug\n\nContext: a.go" {
		t.Fatalf("input first row = %+v", rows)
	}
	if _, info, _ := engine.Session(ctx, byInput.ID); info.Goal != "fix the bug" {
		t.Fatalf("goal = %q; Input must not replace the goal", info.Goal)
	}

	// The loop no longer adds a goal row to a session that already has its first one.
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	if _, err := supervisor.startWith(ctx, byInput.ID, completingProvider("done"), AgentOptions{}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, byInput.ID, 5*time.Second)
	rows = transcript(t, byInput)
	if len(rows) != 2 || rows[0].Content != "fix the bug\n\nContext: a.go" || rows[1].Role != "assistant" {
		t.Fatalf("transcript after the run = %+v", rows)
	}
}

// F8 end to end in core: a tool cancelled mid-call leaves its call unanswered; the
// next Start with Input closes it first, so the follow-up lands after a paired
// transcript and the model never sees an unanswered call.
func TestCancelDuringToolThenStartWithInputLeavesAPairedTranscript(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "sleep")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	events, unsubscribe := engine.Events(512)
	defer unsubscribe()

	cancelled := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "sleep-1", Name: "shell", Arguments: json.RawMessage(`{"command":"echo ready && exec sleep 5"}`)},
		}}, FinishReason: "tool_calls"}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), handle.ID, cancelled, AgentOptions{Policy: "full", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitForEventWith(t, events, "tool.output", "ready")
	supervisor.Cancel(handle.ID)
	waitUntilIdle(t, supervisor, handle.ID, 15*time.Second)

	before := transcript(t, handle)
	if last := before[len(before)-1]; last.Role != "assistant" || len(last.ToolCalls) != 1 {
		t.Fatalf("expected the cancelled call to be unanswered, transcript: %+v", before)
	}

	var sent []protocol.Message
	followUp := &exitProvider{fn: func(_ context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		sent = req.Messages
		return protocol.ProviderResponse{Message: protocol.Message{Content: "continued"}}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), handle.ID, followUp, AgentOptions{Input: "now just list the files", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)

	after := transcript(t, handle)
	assertPaired(t, after)
	assertPaired(t, sent)
	// goal, assistant(call), tool_interrupted, follow-up, assistant: Reconcile came first.
	if len(after) != 5 {
		t.Fatalf("transcript = %+v", after)
	}
	closer, input := after[2], after[3]
	if closer.Role != "tool" || closer.ToolCallID != "sleep-1" || closer.Origin != protocol.OriginToolInterrupted || closer.Content != session.InterruptedToolResult {
		t.Fatalf("row 2 = %+v, want the tool_interrupted result for sleep-1", closer)
	}
	if input.Role != "user" || input.Content != "now just list the files" || input.Origin != "" {
		t.Fatalf("row 3 = %+v, want the follow-up input", input)
	}
	info, _ := sessionState(t, engine, handle.ID)
	if info.Status != "completed" {
		t.Fatalf("status = %q, want completed", info.Status)
	}

	// Origin is stored in the message JSON and stays there.
	rows, err := handle.RecentMessages(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rows[2].JSON), `"origin":"tool_interrupted"`) {
		t.Fatalf("stored row = %s", rows[2].JSON)
	}
}

// Open reconciles a running session whose lock is free: its owner died.
func TestOpenReconcilesARunningSessionWhoseLockIsFree(t *testing.T) {
	workspace := t.TempDir()
	first := openEngineAt(t, workspace)
	handle := newTestSession(t, first, "g")
	running := "running"
	if err := first.store.Update(context.Background(), handle.ID, session.Patch{Status: &running}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.store.AppendMessage(context.Background(), handle.ID, "assistant", protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "c1", Name: "shell", Arguments: json.RawMessage(`{}`)}}}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second := openEngineAt(t, workspace)
	info, cause := sessionState(t, second, handle.ID)
	if info.Status != "interrupted" || cause == nil || cause.Kind != session.KindStaleRunRecovered {
		t.Fatalf("status=%q cause=%+v, want interrupted/stale_run_recovered", info.Status, cause)
	}
	reopened, _, err := second.Session(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows := transcript(t, reopened)
	assertPaired(t, rows)
	if last := rows[len(rows)-1]; last.Origin != protocol.OriginToolInterrupted || last.ToolCallID != "c1" {
		t.Fatalf("last row = %+v", last)
	}
}

// Approval changes workflow state only (F7): it no longer fakes a running session.
// The host continues with Start, which owns the transition to running.
func TestApprovalLeavesStatusAloneAndStartContinues(t *testing.T) {
	engine := openTestEngine(t)
	handle := waitingGateSession(t, engine)
	_, causeBefore := sessionState(t, engine, handle.ID)
	if causeBefore == nil || causeBefore.Kind != "workflow_gate" {
		t.Fatalf("cause before approval = %+v", causeBefore)
	}

	if _, err := engine.ApproveWorkflowGate(context.Background(), handle.ID, "ship", "alice"); err != nil {
		t.Fatal(err)
	}
	info, cause := sessionState(t, engine, handle.ID)
	if info.Status != "waiting_gate" || cause == nil || cause.Kind != "workflow_gate" {
		t.Fatalf("after approval status=%q cause=%+v; approval must not change the status", info.Status, cause)
	}
	if !approvedGateIn(t, engine, handle.ID) {
		t.Fatal("approval was not recorded in the workflow")
	}

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	if _, err := supervisor.startWith(context.Background(), handle.ID, completingProvider("shipped"), AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, handle.ID, 5*time.Second)
	if info, _ := sessionState(t, engine, handle.ID); info.Status != "completed" {
		t.Fatalf("status after Start = %q, want completed", info.Status)
	}
}
