package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type platformHook struct {
	id     string
	events []LifecycleEvent
	fn     func(context.Context, LifecycleRequest) (LifecycleResult, error)
}

func (h *platformHook) ID() string                       { return h.id }
func (h *platformHook) Version() int                     { return 1 }
func (h *platformHook) Events() []LifecycleEvent         { return h.events }
func (h *platformHook) Priority() int                    { return 0 }
func (h *platformHook) Timeout() time.Duration           { return time.Second }
func (h *platformHook) FailurePolicy() HookFailurePolicy { return HookFailClosed }
func (h *platformHook) MutableArgs() bool                { return false }
func (h *platformHook) Safety() bool                     { return true }
func (h *platformHook) Handle(ctx context.Context, r LifecycleRequest) (LifecycleResult, error) {
	return h.fn(ctx, r)
}

func TestLifecycleHooksControlRealExecution(t *testing.T) {
	e := openTestEngine(t)
	p := &debuggerProvider{}
	id := newParentSession(t, e, p)
	var mu sync.Mutex
	seen := map[LifecycleEvent]int{}
	err := e.RegisterLifecycleHook(&platformHook{id: "guard", events: LifecycleEvents(), fn: func(_ context.Context, r LifecycleRequest) (LifecycleResult, error) {
		mu.Lock()
		seen[r.Event]++
		mu.Unlock()
		if r.Event == LifecycleBeforeToolCall && r.ToolArgs["path"] == "first.txt" {
			return LifecycleResult{Decision: HookDeny, Reason: "first-write-denied"}, nil
		}
		return LifecycleResult{Decision: HookContinue}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := e.RunAgent(context.Background(), id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 4})
	if err != nil || result.Status != "completed" {
		t.Fatalf("run %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(e.workspace, "first.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("denied operation executed: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(e.workspace, "second.txt")); err != nil || string(data) != "TWO" {
		t.Fatalf("allowed operation %q %v", data, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, event := range []LifecycleEvent{LifecycleSessionStart, LifecycleBeforeModelCall, LifecycleAfterModelCall, LifecycleBeforeToolCall, LifecycleAfterToolCall, LifecycleBeforeDelivery, LifecycleSessionEnd} {
		if seen[event] == 0 {
			t.Fatalf("missing actual lifecycle %s: %v", event, seen)
		}
	}
	if seen[LifecycleSessionStart] != 1 || seen[LifecycleSessionEnd] != 1 {
		t.Fatalf("duplicate run lifecycle: %v", seen)
	}
	t.Log("HOOK_ENGINE_REAL: denied first file absent; allowed second file TWO; model/tool/session lifecycle observed; external model requests=0")
}

func TestLifecycleBeforeModelDenialMakesNoRequest(t *testing.T) {
	e := openTestEngine(t)
	p := &debuggerProvider{}
	id := newParentSession(t, e, p)
	if err := e.RegisterLifecycleHook(&platformHook{id: "no-model", events: []LifecycleEvent{LifecycleBeforeModelCall}, fn: func(context.Context, LifecycleRequest) (LifecycleResult, error) {
		return LifecycleResult{Decision: HookDeny}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	result, err := e.RunAgent(context.Background(), id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 2})
	if err == nil || result.Status == "completed" || p.calls.Load() != 0 {
		t.Fatalf("denial bypass: %+v %v calls=%d", result, err, p.calls.Load())
	}
}

func TestWorkflowApprovalHookCannotCommitDeniedTransition(t *testing.T) {
	e := openTestEngine(t)
	s := waitingGateSession(t, e)
	before, err := e.loadTaskState(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterLifecycleHook(&platformHook{id: "no-transition", events: []LifecycleEvent{LifecycleBeforeWorkflowTransition}, fn: func(context.Context, LifecycleRequest) (LifecycleResult, error) {
		return LifecycleResult{Decision: HookDeny}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ApproveWorkflowGate(context.Background(), s.ID, "ship", "alice"); err == nil {
		t.Fatal("denied transition approved")
	}
	after, err := e.loadTaskState(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != before.Version {
		t.Fatal("denied approval persisted workflow mutation")
	}
}
