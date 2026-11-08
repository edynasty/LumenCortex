package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/protocol"
)

type debuggerProvider struct{ calls atomic.Int64 }

func (*debuggerProvider) Model() string { return "debugger-fixture" }
func (p *debuggerProvider) Complete(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	n := p.calls.Add(1)
	if n == 1 {
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "write-one", Name: "write_file", Arguments: json.RawMessage(`{"path":"first.txt","content":"ONE"}`)},
			{ID: "write-two", Name: "write_file", Arguments: json.RawMessage(`{"path":"second.txt","content":"TWO"}`)},
		}}}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Content: "finished"}}, nil
}
func awaitDebugPause(t *testing.T, e *Engine, id string, after uint64) AgentDebugSnapshot {
	t.Helper()
	deadline := time.NewTimer(8 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		state, err := e.AgentDebugSnapshot(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if state.State == "paused" && state.Revision > after {
			return state
		}
		select {
		case <-deadline.C:
			t.Fatalf("not paused: %+v", state)
		case <-tick.C:
		}
	}
}
func commandDebug(t *testing.T, e *Engine, id string, s AgentDebugSnapshot, action string) {
	t.Helper()
	if _, err := e.ControlAgentDebug(context.Background(), AgentDebugCommand{SessionID: id, RunID: s.RunID, Revision: s.Revision, Action: action}); err != nil {
		t.Fatal(err)
	}
}
func TestAgentDebuggerModelToolTurnAndNoReplay(t *testing.T) {
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &debuggerProvider{}
	id := newParentSession(t, e, p)
	if _, err = sup.Start(context.Background(), id, AgentOptions{Policy: "workspace", Interactive: true, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}
	s := awaitDebugPause(t, e, id, 0)
	if s.Boundary.Event != "BeforeModelCall" || p.calls.Load() != 0 {
		t.Fatalf("started before pause %+v", s)
	}
	commandDebug(t, e, id, s, "step_model")
	s = awaitDebugPause(t, e, id, s.Revision)
	if s.Boundary.Event != "AfterModelCall" || p.calls.Load() != 1 {
		t.Fatalf("model step %+v calls=%d", s, p.calls.Load())
	}
	if _, err = os.Stat(filepath.Join(e.workspace, "first.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("tool executed during model step")
	}
	commandDebug(t, e, id, s, "step_tool")
	s = awaitDebugPause(t, e, id, s.Revision)
	first, err := os.ReadFile(filepath.Join(e.workspace, "first.txt"))
	if err != nil || string(first) != "ONE" {
		t.Fatalf("first tool %q %v", first, err)
	}
	if _, err = os.Stat(filepath.Join(e.workspace, "second.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("second tool executed during one-tool step")
	}
	if _, err = e.ControlAgentDebug(context.Background(), AgentDebugCommand{SessionID: id, RunID: s.RunID, Revision: s.Revision - 1, Action: "continue"}); !errors.Is(err, agent.ErrDebugStale) {
		t.Fatalf("stale command %v", err)
	}
	commandDebug(t, e, id, s, "step_turn")
	s = awaitDebugPause(t, e, id, s.Revision)
	if s.Boundary.Event != "BeforeModelCall" || p.calls.Load() != 1 {
		t.Fatalf("turn crossed next model %+v calls=%d", s, p.calls.Load())
	}
	second, err := os.ReadFile(filepath.Join(e.workspace, "second.txt"))
	if err != nil || string(second) != "TWO" {
		t.Fatalf("second tool %q %v", second, err)
	}
	commandDebug(t, e, id, s, "cancel")
	waitInactive(t, sup, id)
	page, err := e.QueryRuntimeEvents(context.Background(), RuntimeEventQuery{SessionID: id, Type: "tool.end"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("tool replay or loss: %d", len(page.Events))
	}
	state, err := e.AgentDebugSnapshot(context.Background(), id)
	if err != nil || state.State != "inactive" {
		t.Fatalf("controller survived run %+v %v", state, err)
	}
}

func TestAgentDebuggerPausedDeadlineAndRevocation(t *testing.T) {
	for _, mode := range []string{"deadline", "revoke", "close"} {
		t.Run(mode, func(t *testing.T) {
			e := openTestEngine(t)
			sup, _ := NewRunSupervisor(e)
			defer sup.Close()
			p := &debuggerProvider{}
			id := newParentSession(t, e, p)
			opts := AgentOptions{Debug: &AgentDebugOptions{PauseOnStart: true}}
			if mode == "deadline" {
				deadline := time.Now().Add(400 * time.Millisecond)
				opts.Deadline = &deadline
			}
			if _, err := sup.Start(context.Background(), id, opts); err != nil {
				t.Fatal(err)
			}
			s := awaitDebugPause(t, e, id, 0)
			if mode == "close" {
				commandDebug(t, e, id, s, "close")
			}
			if mode == "revoke" {
				if _, err := e.RevokeSource(context.Background(), SourceSession, id, "debugger_test"); err != nil {
					t.Fatal(err)
				}
				if _, err := e.ControlAgentDebug(context.Background(), AgentDebugCommand{SessionID: id, RunID: s.RunID, Revision: s.Revision, Action: "step_model"}); err == nil {
					t.Fatal("revoked run resumed")
				}
			}
			waitInactive(t, sup, id)
			if p.calls.Load() != 0 {
				t.Fatal("paused run dispatched a model request")
			}
		})
	}
}

func TestAgentDebuggerStepCannotGrantWritePermission(t *testing.T) {
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &debuggerProvider{}
	id := newParentSession(t, e, p)
	if _, err := sup.Start(context.Background(), id, AgentOptions{Policy: "read-only", Debug: &AgentDebugOptions{Breakpoints: []AgentDebugBreakpoint{{ID: "denied", Event: "PermissionDenied", Enabled: true, Once: true}}}}); err != nil {
		t.Fatal(err)
	}
	s := awaitDebugPause(t, e, id, 0)
	if s.Boundary.Event != "PermissionDenied" {
		t.Fatalf("wrong boundary %+v", s)
	}
	if _, err := os.Stat(filepath.Join(e.workspace, "first.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("debugger broadened policy")
	}
	commandDebug(t, e, id, s, "cancel")
	waitInactive(t, sup, id)
}
