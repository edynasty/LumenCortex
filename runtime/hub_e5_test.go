package runtime

import (
	"context"
	"encoding/json"

	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// This file is the Go port of experiment E5 (bench/evidence/agent-harness-capability-roadmap-20261110,
// prototypes/e5-control-semantics): do Pause / Resume / Steer / Cancel have real runtime semantics,
// observed only through public Engine APIs? The provider is scripted and records every request, so the
// suite makes no model request.

// e5Provider answers like the experiment's fixture: the first request asks for a sleep in the workspace,
// the second finishes. It records each request's messages as JSON, which is what the checks read.
type e5Provider struct {
	mu   sync.Mutex
	reqs []string
}

func (*e5Provider) Model() string { return "e5-fixture" }

func (p *e5Provider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	raw, _ := json.Marshal(req.Messages)
	p.mu.Lock()
	p.reqs = append(p.reqs, string(raw))
	n := len(p.reqs)
	p.mu.Unlock()
	if n == 1 {
		args, _ := json.Marshal(map[string]any{"command": "sleep 2; echo slept > slept.txt"})
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "sleep", Name: "bash", Arguments: args}}}, FinishReason: "tool_calls"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
}

func (p *e5Provider) requests() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.reqs...)
}

func (p *e5Provider) n() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

// awaitPause waits for a paused snapshot past revision after, the way the experiment's harness did.
func awaitPause(t *testing.T, e *Engine, id string, after uint64) AgentDebugSnapshot {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		s, err := e.AgentDebugSnapshot(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if s.State == "paused" && s.Revision > after {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	s, _ := e.AgentDebugSnapshot(context.Background(), id)
	t.Fatalf("not paused within 10s: %+v", s)
	return s
}

func debugCommand(t *testing.T, e *Engine, id string, s AgentDebugSnapshot, action string) {
	t.Helper()
	if _, err := e.ControlAgentDebug(context.Background(), AgentDebugCommand{SessionID: id, RunID: s.RunID, Revision: s.Revision, Action: action}); err != nil {
		t.Fatal(err)
	}
}

func TestE5ControlSemanticsPort(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &e5Provider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	root := e.workspace

	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "full", Interactive: true, MaxSteps: 10, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}

	// C1: PauseOnStart blocks before the first model request.
	snap := awaitPause(t, e, id, 0)
	time.Sleep(300 * time.Millisecond)
	if snap.Boundary.Event != "BeforeModelCall" || p.n() != 0 {
		t.Fatalf("C1 boundary=%s requests=%d", snap.Boundary.Event, p.n())
	}

	// C2: a second RunAgent cannot inject a chat turn into a live run.
	if _, err := e.RunAgent(ctx, id, AgentOptions{Input: "INJECT-ATTEMPT"}); err == nil {
		t.Fatal("C2 injected a message into a live run")
	}

	// C3: a steer queued while paused is visible in the request the pause guarded (F-STEER-1 fixed).
	marker := "STEER-MARKER-7f3a"
	if _, err := e.EditTaskState(ctx, id, TaskStateEdit{Brief: &marker}); err != nil {
		t.Fatal(err)
	}
	debugCommand(t, e, id, snap, "step_model")
	snap2 := awaitPause(t, e, id, snap.Revision)
	if snap2.Boundary.Event != "AfterModelCall" {
		t.Fatalf("C3 boundary after model step %s", snap2.Boundary.Event)
	}
	requests := p.requests()
	if len(requests) != 1 || !strings.Contains(requests[0], marker) {
		t.Fatalf("C3 steer not visible in the first request: requests=%d contains=%v", len(requests), len(requests) > 0 && strings.Contains(requests[0], marker))
	}
	// C3b: the refused injection never reached the model.
	if strings.Contains(requests[0], "INJECT-ATTEMPT") {
		t.Fatal("C3b injected text reached the model")
	}

	// C4: a delayed command with an older revision cannot drive the new pause.
	if _, err := e.ControlAgentDebug(ctx, AgentDebugCommand{SessionID: id, RunID: snap.RunID, Revision: snap.Revision, Action: "continue"}); err == nil {
		t.Fatal("C4 stale revision was accepted")
	}

	// C5: a pause requested during a running tool waits for the tool, then pauses after it.
	debugCommand(t, e, id, snap2, "continue")
	time.Sleep(400 * time.Millisecond)
	current, err := e.AgentDebugSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pauseAt := time.Now()
	if _, err := e.ControlAgentDebug(ctx, AgentDebugCommand{SessionID: id, RunID: current.RunID, Revision: current.Revision, Action: "pause"}); err != nil {
		t.Fatal(err)
	}
	snap3 := awaitPause(t, e, id, current.Revision)
	waited := time.Since(pauseAt)
	if waited <= time.Second || p.n() != 1 {
		t.Fatalf("C5 pause_latency=%s requests=%d", waited, p.n())
	}
	if snap3.Boundary.Event != "AfterToolCall" {
		t.Fatalf("C5 boundary %s", snap3.Boundary.Event)
	}
	// The tool ran to completion: its result row is in the transcript, so the pause did not preempt it.
	rows, err := e.store.RecentMessages(ctx, id, 100)
	if err != nil {
		t.Fatal(err)
	}
	slept := false
	for _, row := range rows {
		var message struct {
			Role string `json:"role"`
			Name string `json:"name"`
		}
		_ = json.Unmarshal(row.JSON, &message)
		if message.Role == "tool" && message.Name == "bash" {
			slept = true
		}
	}
	if !slept {
		t.Fatal("C5 the running tool's result is missing: the pause preempted it")
	}
	_ = root

	// C6: cancel while paused ends the run without another model request.
	debugCommand(t, e, id, snap3, "cancel")
	waitInactive(t, sup, id)
	if p.n() != 1 {
		t.Fatalf("C6 requests after cancel %d", p.n())
	}
	_, info, err := e.Session(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != "interrupted" {
		t.Fatalf("C6 status %s", info.Status)
	}

	// C7: resume after cancel is a new Start; the completed bash call is not replayed.
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "full", Interactive: true, MaxSteps: 10}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: id, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	tools := 0
	for _, ev := range page.Events {
		if ev.Type == "tool.end" {
			tools++
		}
	}
	if tools != 1 || p.n() != 2 {
		t.Fatalf("C7 tool.end=%d requests=%d", tools, p.n())
	}

	// C8: the Hub view is available for a session without a task DAG.
	view, err := sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tasks) != 0 || len(view.Agents) != 1 || view.Agents[0].SessionID != id {
		t.Fatalf("C8 view %+v", view)
	}

	// C3c: the steer is no longer one model turn late. It appears in the first request (C3) and it is
	// applied exactly once, so the later request sees it as Task State, not as a second application.
	requests = p.requests()
	if len(requests) < 2 || !strings.Contains(requests[0], marker) {
		t.Fatalf("C3c the steer did not reach the first request: requests=%d", len(requests))
	}
	events, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: id, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, event := range events.Events {
		if event.Type == "task_state.edits_applied" {
			applied++
		}
	}
	if applied != 1 {
		t.Fatalf("C3c the queued edit was applied %d times", applied)
	}
}

// TestE5SteerDuringToolLandsInTheToolFollowUpRequest pins HUB-A3's first half: a steer queued while a
// tool runs is visible in the request built after that tool, never during it.
func TestE5SteerDuringToolLandsInTheToolFollowUpRequest(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &e5Provider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "full", Interactive: true, MaxSteps: 10}); err != nil {
		t.Fatal(err)
	}
	// The scripted provider asks for the two-second sleep first; steer while it runs.
	deadline := time.Now().Add(5 * time.Second)
	for p.n() != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.n() != 1 {
		t.Fatal("provider was never called")
	}
	if _, err := e.SteerAgent(ctx, id, "", "STEER-DURING-TOOL", "steer-1"); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
	requests := p.requests()
	if len(requests) < 2 {
		t.Fatalf("requests %d", len(requests))
	}
	if strings.Contains(requests[0], "STEER-DURING-TOOL") {
		t.Fatal("the steer interrupted the running tool's request")
	}
	if !strings.Contains(requests[1], "STEER-DURING-TOOL") {
		t.Fatal("the steer did not reach the follow-up request")
	}
}

// TestE5MultipleSteersApplyInSequenceOrder pins the deterministic order of concurrent steers: the inbox
// sequence decides, whatever order the host calls arrived in.
func TestE5MultipleSteersApplyInOrder(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &e5Provider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "full", Interactive: true, MaxSteps: 10, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}
	snap := awaitPause(t, e, id, 0)
	for i, text := range []string{"STEER-ONE", "STEER-TWO", "STEER-THREE"} {
		if _, err := e.SteerAgent(ctx, id, "", text, "steer-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	debugCommand(t, e, id, snap, "step_model")
	awaitPause(t, e, id, snap.Revision)
	requests := p.requests()
	if len(requests) != 1 {
		t.Fatalf("requests %d", len(requests))
	}
	one := strings.Index(requests[0], "STEER-ONE")
	two := strings.Index(requests[0], "STEER-TWO")
	three := strings.Index(requests[0], "STEER-THREE")
	if one < 0 || two < 0 || three < 0 || !(one < two && two < three) {
		t.Fatalf("steers out of order: %d %d %d", one, two, three)
	}
}

// TestE5SteerIsDurableAcrossRuns pins that a steer queued for an idle session reaches the next run and
// is applied once, and that the stored row carries only the host's text.
func TestE5SteerIsDurableAndAppliedOnce(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &e5Provider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	message, err := e.SteerAgent(ctx, id, "", "IDLE-STEER", "idle-1")
	if err != nil {
		t.Fatal(err)
	}
	if message.AppliedAt != nil || message.AppliedSeq != -1 {
		t.Fatalf("a queued steer is applied: %+v", message)
	}
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
	requests := p.requests()
	if len(requests) == 0 || !strings.Contains(requests[0], "IDLE-STEER") {
		t.Fatal("the idle steer did not reach the first request")
	}
	pending, err := e.store.PendingSteerMessages(ctx, id, 8)
	if err != nil || len(pending) != 0 {
		t.Fatalf("steer still pending: %v %v", pending, err)
	}
	// The persisted row holds only the host's text, and records the transcript row it became.
	stored, err := e.store.SteerMessages(ctx, id, 8)
	if err != nil || len(stored) != 1 {
		t.Fatalf("stored steers %+v %v", stored, err)
	}
	if stored[0].Body != "IDLE-STEER" || stored[0].AppliedAt == nil || stored[0].AppliedSeq < 0 {
		t.Fatalf("stored steer %+v", stored[0])
	}
}
