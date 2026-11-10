package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// hubScriptProvider answers a fixed script and records the requests it received, so a Hub command's
// effect is observed without a model.
type hubScriptProvider struct {
	mu   sync.Mutex
	reqs []protocol.ProviderRequest
	// respond, when set, decides each answer; nil finishes on the second request.
	respond func(n int, req protocol.ProviderRequest) protocol.ProviderResponse
}

func (*hubScriptProvider) Model() string { return "hub-script" }

func (p *hubScriptProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.reqs = append(p.reqs, req)
	n := len(p.reqs)
	p.mu.Unlock()
	if p.respond != nil {
		return p.respond(n, req), nil
	}
	if n == 1 {
		body, _ := json.Marshal(map[string]string{"path": "result.txt", "content": "one"})
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "w1", Name: "write_file", Arguments: body},
			{ID: "u1", Name: "work_unit_update", Arguments: json.RawMessage(`{"id":"task","status":"completed","summary":"wrote result.txt"}`)},
		}}, FinishReason: "tool_calls"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Content: "finished"}, FinishReason: "stop"}, nil
}

func (p *hubScriptProvider) requests() []protocol.ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]protocol.ProviderRequest(nil), p.reqs...)
}

func (p *hubScriptProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reqs)
}

func hubCommand(t *testing.T, sup *RunSupervisor, c HubCommand) (HubCommandResult, error) {
	t.Helper()
	return sup.Command(context.Background(), c)
}

// TestHubCommandIdempotencyAndRevisionFence pins the two control invariants of the Hub command plane: the
// same CommandID returns the first result without a second effect, and a stale revision is refused
// instead of driving a run the host has not seen.
func TestHubCommandIdempotencyAndRevisionFence(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 5, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}
	snap := awaitPause(t, e, id, 0)

	// A pause command with the live revision is accepted and the view shows pause_requested.
	view, err := sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Agents[0].State != "paused" || view.Agents[0].Revision != snap.Revision {
		t.Fatalf("view before command %+v", view.Agents[0])
	}
	first, err := hubCommand(t, sup, HubCommand{
		SessionID: id, CommandID: "cmd-step-1", ExpectedRevision: snap.Revision, Verb: HubVerbStep,
		Args: json.RawMessage(`{"unit":"model"}`),
	})
	if err != nil || first.Status != HubAccepted {
		t.Fatalf("step %+v %v", first, err)
	}
	if first.Agent.State != "pause_requested" && first.Agent.State != "running" && first.Agent.State != "paused" {
		t.Fatalf("step left state %s", first.Agent.State)
	}
	// The same command id returns the recorded result and starts nothing new.
	before := p.count()
	again, err := hubCommand(t, sup, HubCommand{
		SessionID: id, CommandID: "cmd-step-1", ExpectedRevision: snap.Revision, Verb: HubVerbStep,
		Args: json.RawMessage(`{"unit":"model"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.Status != HubDuplicate || again.Agent.State != first.Agent.State {
		t.Fatalf("duplicate %+v", again)
	}
	if p.count() > before+1 {
		t.Fatalf("a duplicate command drove extra requests: %d -> %d", before, p.count())
	}
	// A reused id for another verb is an error, never a silent replay.
	if _, err := hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cmd-step-1", ExpectedRevision: snap.Revision, Verb: HubVerbCancel}); err == nil {
		t.Fatal("a reused command id was accepted for another verb")
	}

	// A stale revision is refused with the explicit stale error: the revision the step command used is no
	// longer the live one.
	_, err = hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cmd-stale", ExpectedRevision: snap.Revision, Verb: HubVerbResume})
	if !errors.Is(err, ErrHubStaleRevision) {
		t.Fatalf("stale revision: %v", err)
	}
	// End the run so the test leaves nothing running. The accepted step is still advancing the run, and
	// each boundary it reaches bumps the revision, so a cancel may be refused as stale; like any host, the
	// test refreshes the view and sends a new command. The run may also finish on its own first.
	for attempt := 0; ; attempt++ {
		current, err := e.AgentDebugSnapshot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = hubCommand(t, sup, HubCommand{SessionID: id, CommandID: fmt.Sprintf("cmd-cancel-end-%d", attempt), ExpectedRevision: current.Revision, Verb: HubVerbCancel})
		if err == nil || errors.Is(err, ErrHubRunInactive) {
			break
		}
		if !errors.Is(err, ErrHubStaleRevision) || attempt == 50 {
			t.Fatalf("cancel at the end (attempt %d): %v", attempt, err)
		}
	}
	waitInactive(t, sup, id)
	// A command for a session with no live run needs revision 0.
	if _, err := hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cmd-stale-idle", ExpectedRevision: 7, Verb: HubVerbCancel}); !errors.Is(err, ErrHubStaleRevision) {
		t.Fatalf("stale revision without a live run: %v", err)
	}
	if _, err := hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cmd-idle-cancel", Verb: HubVerbCancel}); !errors.Is(err, ErrHubRunInactive) {
		t.Fatalf("cancel without a live run: %v", err)
	}
}

// TestHubCommandCancelAndRetry pins cancel (the run really stops) and retry (a new run from the last safe
// state that replays no committed tool).
func TestHubCommandCancelAndRetry(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 5, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}
	snap := awaitPause(t, e, id, 0)
	result, err := hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cmd-cancel", ExpectedRevision: snap.Revision, Verb: HubVerbCancel})
	if err != nil || result.Status != HubAccepted {
		t.Fatalf("cancel %+v %v", result, err)
	}
	waitInactive(t, sup, id)
	view, err := sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Agents[0].Live || view.Agents[0].State != "interrupted" {
		t.Fatalf("after cancel %+v", view.Agents[0])
	}

	// Retry reuses the recorded run's options and starts a new run without replaying the write tool.
	if _, err := hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cmd-retry", Verb: HubVerbRetry}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: id, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	toolEnds, retries := 0, 0
	for _, event := range page.Events {
		switch event.Type {
		case "tool.end":
			// The write is the one side effect the retry must not replay; the work-unit update is not.
			if bytesContains(event.Data, `"name":"write_file"`) {
				toolEnds++
			}
		case "agent.retry":
			retries++
		}
	}
	if toolEnds != 1 {
		t.Fatalf("retry replayed a tool: write_file tool.end=%d", toolEnds)
	}
	if retries != 1 {
		t.Fatalf("retry event %d", retries)
	}
	view, err = sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Agents[0].RetryOf == "" {
		t.Fatal("the retried run does not name the run it retries")
	}
	// A retry with no recorded run is refused, never silently started with defaults.
	other := newParentSession(t, e, p)
	if _, err := hubCommand(t, sup, HubCommand{SessionID: other, CommandID: "cmd-retry-2", Verb: HubVerbRetry}); err == nil {
		t.Fatal("retry without a recorded run was accepted")
	}
}

// TestHubViewProjectsAgentTreeAndState pins the projection: the root and its children appear with their
// role, state and revision, and the view carries metadata only.
func TestHubViewProjectsAgentTreeAndState(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	root, _ := taskRepo(t)
	e, sup := openTaskEngine(t, root)
	defer e.Close()
	defer sup.Close()
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	// A paused root shows its live boundary and revision.
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "workspace", Interactive: true, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}
	snap := awaitPause(t, e, id, 0)
	view, err := sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Agents) != 1 {
		t.Fatalf("agents %+v", view.Agents)
	}
	agent := view.Agents[0]
	if agent.SessionID != id || agent.Role != "main" || agent.State != "paused" ||
		agent.Revision != snap.Revision || agent.RunID != snap.RunID || agent.Boundary.Event != "BeforeModelCall" || !agent.Live {
		t.Fatalf("projected agent %+v", agent)
	}
	if view.Cursor == 0 {
		t.Fatal("view has no event cursor")
	}
	if !json.Valid(hubJSON(t, agent)) {
		t.Fatal("agent projection is not JSON")
	}
	// Nothing content-bearing is projected: the goal text lives only in the session row.
	goal := "parent task"
	if strings.Contains(string(hubJSON(t, view)), goal) {
		t.Fatal("the hub view carries the goal text")
	}
	if _, err := hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cancel-tree", ExpectedRevision: snap.Revision, Verb: HubVerbCancel}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
}

func hubJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestHubPauseSurvivesRestart pins the durable pause: a paused run that the process ends with is shown as
// interrupted(paused) by a reopened Engine, and the pause row is cleared when the session runs again.
func TestHubPauseSurvivesRestart(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	dir := t.TempDir()
	e, err := Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "read-only", Interactive: true, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}
	awaitPause(t, e, id, 0)
	pauses, err := e.store.RunPauses(ctx, []string{id})
	if err != nil || pauses[id].RunID == "" || pauses[id].Boundary != "BeforeModelCall" {
		t.Fatalf("pause not recorded: %+v %v", pauses, err)
	}
	// The process ends while the run is paused: closing cancels the goroutine, the pause row stays.
	sup.Close()
	e.Close()

	e, err = Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	sup, err = NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	view, err := sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Agents) != 1 || view.Agents[0].State != HubStateInterruptedStop || view.Agents[0].Paused == nil {
		t.Fatalf("after restart %+v", view.Agents)
	}
	// A new Start clears it: the pause belonged to the run that ended. The reopened Engine needs its own
	// provider bound to the session's catalog model.
	bind(e, p)
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
	view, err = sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if view.Agents[0].Paused != nil || view.Agents[0].State == HubStateInterruptedStop {
		t.Fatalf("pause survived the next run: %+v", view.Agents[0])
	}
}

// TestHubEventReplayReconnectAndGap pins the event contract: a subscriber resumes from a cursor without
// loss, and a bounded buffer that had to drop events reports the durable cursor range it lost.
func TestHubEventReplayReconnectAndGap(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	events, unsubscribe, err := e.SubscribeRuntimeEvents(ctx, id, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer unsubscribe()
	// Fill the subscriber's buffer past its capacity: the newest deliveries survive, the rest is lost and
	// reported as a gap.
	for i := range 12 {
		e.events.publish("hub.test.marker", id, map[string]any{"n": i})
	}
	var gap Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && gap.Type == "" {
		select {
		case event := <-events:
			if event.Type == "hub.events.gap" {
				gap = event
			}
		case <-time.After(20 * time.Millisecond):
		}
	}
	if gap.Type != "hub.events.gap" {
		t.Fatal("no gap was reported for a dropped range")
	}
	var payload struct {
		From uint64 `json:"from"`
		To   uint64 `json:"to"`
		Lost int    `json:"lost"`
	}
	if err := json.Unmarshal(gap.Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Lost <= 0 || payload.From == 0 || payload.To < payload.From {
		t.Fatalf("gap payload %+v", payload)
	}
	// The durable history still holds every event: a reconnect replays the range the gap named.
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: id, After: payload.From - 1, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, event := range page.Events {
		if event.Cursor >= payload.From && event.Cursor <= payload.To && event.Type == "hub.test.marker" {
			seen++
		}
	}
	if seen != payload.Lost {
		t.Fatalf("replay repaired %d of %d lost events", seen, payload.Lost)
	}
	// Per-session sequence is monotonic in durable order.
	var last uint64
	for _, event := range page.Events {
		if event.Cursor < last {
			t.Fatalf("cursor went backwards: %d after %d", event.Cursor, last)
		}
		last = event.Cursor
	}
}

// bytesContains reports whether raw contains needle, for reading a durable event's metadata.
func bytesContains(raw []byte, needle string) bool { return strings.Contains(string(raw), needle) }

// TestHubCancelWhilePausedIsNotReportedPausedOnceStopped pins the run-exit order: a run cancelled while
// paused has cleared its durable pause before it leaves the supervisor, so a host that sees the run
// stopped reads interrupted, never interrupted(paused). The hook holds cleanup right after the run left
// the supervisor; before the fix the pause was cleared only after that point.
func TestHubCancelWhilePausedIsNotReportedPausedOnceStopped(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	release := make(chan struct{})
	var once sync.Once
	releaseCleanup := func() { once.Do(func() { close(release) }) }
	left := make(chan struct{})
	testHookAfterRunLeaves = func() { <-release; close(left) }
	defer func() {
		releaseCleanup()
		// The hook is reset only after the run's cleanup returned from it, so the reset does not race
		// the cleanup's read of the hook.
		select {
		case <-left:
		case <-time.After(10 * time.Second):
		}
		testHookAfterRunLeaves = nil
	}()

	id := newParentSession(t, e, &hubScriptProvider{})
	ctx := context.Background()
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "workspace", Interactive: true, MaxSteps: 5, Debug: &AgentDebugOptions{PauseOnStart: true}}); err != nil {
		t.Fatal(err)
	}
	snap := awaitPause(t, e, id, 0)
	if _, err := hubCommand(t, sup, HubCommand{SessionID: id, CommandID: "cmd-cancel-paused", ExpectedRevision: snap.Revision, Verb: HubVerbCancel}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
	view, err := sup.View(ctx, id)
	releaseCleanup()
	if err != nil {
		t.Fatal(err)
	}
	if view.Agents[0].Live || view.Agents[0].State != HubStateInterrupted || view.Agents[0].Paused != nil {
		t.Fatalf("stopped run still reported paused: state=%s paused=%+v", view.Agents[0].State, view.Agents[0].Paused)
	}
}
