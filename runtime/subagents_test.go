package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/resource"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type finalProvider struct {
	model string
}

func (p *finalProvider) Model() string {
	if p.model == "" {
		return "final-model"
	}
	return p.model
}

func (p *finalProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", Content: "subagent done"},
		FinishReason: "stop",
	}, nil
}

type multiBlockingProvider struct {
	once    sync.Once
	started chan struct{}
}

func (p *multiBlockingProvider) Model() string { return "blocking-subagent-model" }

func (p *multiBlockingProvider) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if p.started != nil {
		p.once.Do(func() { close(p.started) })
	}
	<-ctx.Done()
	return protocol.ProviderResponse{}, ctx.Err()
}

// newParentSession creates a session pinned to a catalog model backed by p, as a started parent is.
// Children inherit that model.
func newParentSession(t *testing.T, engine *Engine, p protocol.Provider) string {
	t.Helper()
	ref := bind(engine, p)
	id, err := newSessionID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	v := session.Session{ID: id, CreatedAt: now, UpdatedAt: now, Status: "created", Goal: "parent task", ModelRef: string(ref), Metadata: json.RawMessage("{}")}
	if err := engine.store.CreateWithMessage(context.Background(), v, protocol.Message{Role: "user", Content: "parent task"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSubagentChildOptionsAreReadOnlyAndNonRecursive(t *testing.T) {
	controller := &subagentController{
		parentID: "parent",
		parentOpts: AgentOptions{
			Policy:              toolset.PolicyFull,
			MaxSteps:            100,
			MaxToolCallsPerStep: 100,
		},
	}
	opts := controller.childOptions("test/b0", "explore")
	if opts.Policy != toolset.PolicyReadOnly {
		t.Fatalf("policy=%q", opts.Policy)
	}
	if !opts.disableSubagents {
		t.Fatal("child subagents must be disabled")
	}
	if !opts.disableMCP {
		t.Fatal("child MCP must be disabled")
	}
	if opts.parentSessionID != "parent" {
		t.Fatalf("parent=%q", opts.parentSessionID)
	}
}

// withConcurrency is the fixture catalog with a global concurrency object.
func withConcurrency(maxAgents, maxChildren int) string {
	return strings.Replace(boundCatalog("test/b0"), "{",
		fmt.Sprintf(`{"concurrency": {"maxAgents": %d, "maxChildrenPerParent": %d}, `, maxAgents, maxChildren), 1)
}

// The per-parent limit is read from the global configuration at every spawn: a raised limit admits
// the next child of a parent that is already running, without reopening the Engine.
func TestSubagentControllerEnforcesConfiguredChildLimit(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 2))
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	provider := &multiBlockingProvider{}
	parentID := newParentSession(t, engine, provider)
	controller := &subagentController{
		supervisor: supervisor,
		parentID:   parentID,
		parentOpts: AgentOptions{},
	}
	spawn := func(goal string) (string, error) {
		value, err := controller.Spawn(context.Background(), goal, "")
		if err != nil {
			return "", err
		}
		return value.(SubagentNode).SessionID, nil
	}

	var children []string
	for i := 0; i < 2; i++ {
		child, err := spawn("inspect focused area")
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		children = append(children, child)
	}
	_, err = spawn("one too many")
	if !errors.Is(err, ErrSubagentLimitExceeded) || !strings.Contains(err.Error(), "2 running, limit 2") {
		t.Fatalf("third spawn err=%v", err)
	}

	// Saving a higher limit applies to the next spawn of the same running parent.
	useGlobalConfig(t, withConcurrency(8, 3))
	third, err := spawn("admitted after the change")
	if err != nil {
		t.Fatalf("spawn after raising the limit: %v", err)
	}
	children = append(children, third)
	if _, err := spawn("one too many again"); !errors.Is(err, ErrSubagentLimitExceeded) {
		t.Fatalf("fourth spawn err=%v", err)
	}

	tree, err := supervisor.SubagentTree(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 3 {
		t.Fatalf("refused spawns created sessions: tree=%#v", tree)
	}
	for _, node := range tree {
		if !node.Active || node.Runtime.Kind != RuntimeLocal {
			t.Fatalf("node=%#v", node)
		}
	}

	for _, childID := range children {
		if !supervisor.Cancel(childID) {
			t.Fatalf("child %s was not active", childID)
		}
	}
}

func TestSubagentUsesGlobalMaxAgentsBudget(t *testing.T) {
	engine, err := Open(Options{
		Workspace: t.TempDir(),
		Budget:    Budget{MaxAgents: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentProvider := &multiBlockingProvider{started: make(chan struct{})}
	parentID := newParentSession(t, engine, parentProvider)
	if _, err := supervisor.startWith(context.Background(), parentID, parentProvider, AgentOptions{

		Policy: toolset.PolicyReadOnly,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-parentProvider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("parent did not start")
	}

	controller := &subagentController{
		supervisor: supervisor,
		parentID:   parentID,
		parentOpts: AgentOptions{},
	}
	first, err := controller.Spawn(context.Background(), "first child", "")
	if err != nil {
		t.Fatal(err)
	}
	firstNode := first.(SubagentNode)

	if _, err := controller.Spawn(context.Background(), "second child", ""); !errors.Is(err, resource.ErrAgentLimitExceeded) {
		t.Fatalf("second child err=%v", err)
	}

	if !supervisor.Cancel(firstNode.SessionID) {
		t.Fatal("first child not active")
	}
	if !supervisor.Cancel(parentID) {
		t.Fatal("parent not active")
	}
}

func TestSubagentPersistsRelationEventsAndCheckpoints(t *testing.T) {
	engine, err := Open(Options{
		Workspace: t.TempDir(),
		Budget:    Budget{MaxAgents: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentID := newParentSession(t, engine, &finalProvider{})
	events, unsubscribe := engine.Events(64)
	defer unsubscribe()

	controller := &subagentController{
		supervisor: supervisor,
		parentID:   parentID,
		parentOpts: AgentOptions{},
	}
	value, err := controller.Spawn(context.Background(), "finish quickly", "")
	if err != nil {
		t.Fatal(err)
	}
	node := value.(SubagentNode)

	deadline := time.Now().Add(2 * time.Second)
	for supervisor.Active(node.SessionID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if supervisor.Active(node.SessionID) {
		t.Fatal("subagent remained active")
	}

	tree, err := supervisor.SubagentTree(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 {
		t.Fatalf("tree=%#v", tree)
	}
	if tree[0].Status != "completed" || tree[0].Final != "subagent done" {
		t.Fatalf("completed child=%#v", tree[0])
	}

	checkpoints, err := engine.SessionCheckpoints(context.Background(), parentID, 10)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]bool{}
	for _, checkpoint := range checkpoints {
		reasons[checkpoint.Reason] = true
	}
	if !reasons["subagent.spawn"] || !reasons["subagent.stopped"] {
		t.Fatalf("checkpoints=%#v", checkpoints)
	}

	seenAggregated := false
	timeout := time.After(time.Second)
	for !seenAggregated {
		select {
		case event := <-events:
			if event.SessionID == parentID && event.Type == "subagent.event" {
				seenAggregated = true
			}
		case <-timeout:
			t.Fatal("did not receive aggregated subagent event")
		}
	}
}

type spawnToolProvider struct {
	mu             sync.Mutex
	parentCalls    int
	parentSawSpawn bool
	childRequests  int
	childSawSpawn  bool
}

func (p *spawnToolProvider) Model() string { return "spawn-tool-model" }

func (p *spawnToolProvider) Complete(_ context.Context, request protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	hasSpawn := false
	for _, spec := range request.Tools {
		if spec.Name == "spawn_subagent" {
			hasSpawn = true
			break
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if !hasSpawn {
		p.childRequests++
		p.childSawSpawn = p.childSawSpawn || hasSpawn
		return protocol.ProviderResponse{
			Message:      protocol.Message{Role: "assistant", Content: "child evidence"},
			FinishReason: "stop",
		}, nil
	}

	p.parentSawSpawn = true
	p.parentCalls++
	switch p.parentCalls {
	case 1:
		return protocol.ProviderResponse{
			Message: protocol.Message{
				Role: "assistant",
				ToolCalls: []protocol.ToolCall{{
					ID:        "spawn-1",
					Name:      "spawn_subagent",
					Arguments: json.RawMessage(`{"goal":"inspect the child path"}`),
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	case 2:
		// A parent that finishes cancels its still-running children, so it waits
		// for the child before answering: the child's own outcome is the subject.
		child := ""
		for _, m := range request.Messages {
			if m.Role == "tool" {
				if match := spawnedSessionID.FindStringSubmatch(m.Content); match != nil {
					child = match[1]
				}
			}
		}
		if child == "" {
			return protocol.ProviderResponse{}, errors.New("spawn result names no child session")
		}
		return protocol.ProviderResponse{
			Message: protocol.Message{
				Role: "assistant",
				ToolCalls: []protocol.ToolCall{{
					ID:        "wait-1",
					Name:      "wait_subagent",
					Arguments: json.RawMessage(`{"child_session_id":"` + child + `","timeout_seconds":10}`),
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", Content: "parent done"},
		FinishReason: "stop",
	}, nil
}

var spawnedSessionID = regexp.MustCompile(`"sessionId":\s*"([^"]+)"`)

func TestTopLevelAgentGetsSpawnToolButChildCannotRecurse(t *testing.T) {
	engine, err := Open(Options{
		Workspace: t.TempDir(),
		Budget:    Budget{MaxAgents: 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	provider := &spawnToolProvider{}
	parentID := newParentSession(t, engine, provider)
	if _, err := supervisor.startWith(context.Background(), parentID, provider, AgentOptions{

		Policy:   toolset.PolicyReadOnly,
		MaxSteps: 4,
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for supervisor.Active(parentID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if supervisor.Active(parentID) {
		t.Fatal("parent remained active")
	}

	tree, err := supervisor.SubagentTree(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 {
		t.Fatalf("tree=%#v", tree)
	}
	for tree[0].Active && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		tree, err = supervisor.SubagentTree(context.Background(), parentID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if tree[0].Status != "completed" || tree[0].Final != "child evidence" {
		t.Fatalf("child=%#v", tree[0])
	}

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if !provider.parentSawSpawn || provider.parentCalls < 3 {
		t.Fatalf("parent tool visibility calls=%d saw=%v", provider.parentCalls, provider.parentSawSpawn)
	}
	if provider.childRequests == 0 {
		t.Fatal("child provider was not invoked")
	}
	if provider.childSawSpawn {
		t.Fatal("child unexpectedly received recursive spawn tool")
	}
}

func TestWaitSubagentReturnsCompletedResult(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir(), Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentID := newParentSession(t, engine, &finalProvider{})
	controller := &subagentController{
		supervisor: supervisor,
		parentID:   parentID,
		parentOpts: AgentOptions{},
	}
	value, err := controller.Spawn(context.Background(), "finish for wait", "")
	if err != nil {
		t.Fatal(err)
	}
	child := value.(SubagentNode)

	waited, err := controller.Wait(context.Background(), child.SessionID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result := waited.(SubagentWaitResult)
	if result.TimedOut || result.Node.Status != "completed" || result.Node.Final != "subagent done" {
		t.Fatalf("wait result=%#v", result)
	}
}

func TestWaitSubagentTimeoutReturnsActiveNode(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir(), Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentID := newParentSession(t, engine, &multiBlockingProvider{})
	controller := &subagentController{
		supervisor: supervisor,
		parentID:   parentID,
		parentOpts: AgentOptions{},
	}
	value, err := controller.Spawn(context.Background(), "block for timeout", "")
	if err != nil {
		t.Fatal(err)
	}
	child := value.(SubagentNode)

	waited, err := controller.Wait(context.Background(), child.SessionID, 40*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	result := waited.(SubagentWaitResult)
	if !result.TimedOut || !result.Node.Active {
		t.Fatalf("wait result=%#v", result)
	}
	if !supervisor.Cancel(child.SessionID) {
		t.Fatal("timed-out child should remain active until explicitly cancelled")
	}
}

// wait_subagent ends at the run's tool deadline when it comes before the timeout, reports a deadline
// already past as a timed-out wait instead of waiting the default timeout, and returns the caller's
// cancellation, which wins over a passed deadline. No outcome stops the child.
func TestWaitSubagentIsBoundedByTheToolDeadlineAndCancellation(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir(), Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentID := newParentSession(t, engine, &multiBlockingProvider{})
	controller := &subagentController{supervisor: supervisor, parentID: parentID, parentOpts: AgentOptions{}}
	value, err := controller.Spawn(context.Background(), "block while the parent waits", "")
	if err != nil {
		t.Fatal(err)
	}
	child := value.(SubagentNode)
	defer supervisor.Cancel(child.SessionID)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cancelling, cancelSoon := context.WithCancel(context.Background())
	defer cancelSoon()
	cases := []struct {
		name    string
		ctx     func() context.Context
		timeout time.Duration
		wantErr error
	}{
		{"deadline before the timeout", func() context.Context {
			return protocol.WithToolDeadline(context.Background(), time.Now().Add(80*time.Millisecond))
		}, time.Second, nil},
		{"deadline already past", func() context.Context {
			return protocol.WithToolDeadline(context.Background(), time.Now().Add(-time.Second))
		}, 0, nil},
		{"caller cancels while waiting", func() context.Context {
			time.AfterFunc(50*time.Millisecond, cancelSoon)
			return cancelling
		}, time.Second, context.Canceled},
		{"cancellation wins over a passed deadline", func() context.Context {
			return protocol.WithToolDeadline(cancelled, time.Now().Add(-time.Second))
		}, time.Second, context.Canceled},
	}
	for _, tc := range cases {
		ctx := tc.ctx()
		started := time.Now()
		waited, err := controller.Wait(ctx, child.SessionID, tc.timeout)
		elapsed := time.Since(started)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s: err=%v, want %v", tc.name, err, tc.wantErr)
			}
		} else {
			if err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if result := waited.(SubagentWaitResult); !result.TimedOut || !result.Node.Active {
				t.Fatalf("%s: wait result=%#v", tc.name, result)
			}
		}
		if elapsed > 500*time.Millisecond {
			t.Fatalf("%s: the wait took %s", tc.name, elapsed)
		}
		if !supervisor.Active(child.SessionID) {
			t.Fatalf("%s: the wait stopped the child", tc.name)
		}
	}
}

func TestWaitSubagentRejectsForeignChild(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir(), Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentA := newParentSession(t, engine, &finalProvider{})
	parentB := newParentSession(t, engine, &finalProvider{})
	controllerA := &subagentController{
		supervisor: supervisor,
		parentID:   parentA,
		parentOpts: AgentOptions{},
	}
	value, err := controllerA.Spawn(context.Background(), "belongs to A", "")
	if err != nil {
		t.Fatal(err)
	}
	child := value.(SubagentNode)
	controllerB := &subagentController{
		supervisor: supervisor,
		parentID:   parentB,
		parentOpts: AgentOptions{},
	}
	if _, err := controllerB.Wait(context.Background(), child.SessionID, time.Second); err == nil {
		t.Fatal("expected foreign child wait rejection")
	}
	supervisor.Cancel(child.SessionID)
}

// parentExitProvider spawns one child, waits until the child's model call is
// running, and then finishes without waiting for it. The child's call blocks
// until its context ends.
type parentExitProvider struct {
	mu            sync.Mutex
	parentCalls   int
	childStarted  chan struct{}
	childCanceled chan struct{}
}

func (p *parentExitProvider) Model() string { return "parent-exit-model" }

func (p *parentExitProvider) Complete(ctx context.Context, request protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	parent := false
	for _, spec := range request.Tools {
		if spec.Name == "spawn_subagent" {
			parent = true
			break
		}
	}
	if !parent {
		close(p.childStarted)
		<-ctx.Done()
		close(p.childCanceled)
		return protocol.ProviderResponse{}, ctx.Err()
	}
	p.mu.Lock()
	p.parentCalls++
	calls := p.parentCalls
	p.mu.Unlock()
	if calls == 1 {
		return protocol.ProviderResponse{
			Message: protocol.Message{
				Role: "assistant",
				ToolCalls: []protocol.ToolCall{{
					ID:        "spawn-1",
					Name:      "spawn_subagent",
					Arguments: json.RawMessage(`{"goal":"inspect"}`),
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	select {
	case <-p.childStarted:
	case <-ctx.Done():
		return protocol.ProviderResponse{}, ctx.Err()
	}
	return protocol.ProviderResponse{
		Message:      protocol.Message{Role: "assistant", Content: "parent done"},
		FinishReason: "stop",
	}, nil
}

// A parent that finishes while its child still runs stops the child: the
// child's model call sees its context end, the child ends interrupted and not
// completed, and nothing of it stays active.
func TestParentCompletionCancelsItsRunningChild(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir(), Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	provider := &parentExitProvider{childStarted: make(chan struct{}), childCanceled: make(chan struct{})}
	parentID := newParentSession(t, engine, provider)
	if _, err := supervisor.startWith(context.Background(), parentID, provider, AgentOptions{
		Policy:   toolset.PolicyReadOnly,
		MaxSteps: 4,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-provider.childCanceled:
	case <-time.After(10 * time.Second):
		t.Fatal("the child's model call was not cancelled when the parent finished")
	}
	deadline := time.Now().Add(10 * time.Second)
	for supervisor.Active(parentID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if supervisor.Active(parentID) {
		t.Fatal("parent remained active")
	}
	tree, err := supervisor.SubagentTree(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 {
		t.Fatalf("tree=%#v", tree)
	}
	if tree[0].Active || tree[0].Status != "interrupted" {
		t.Fatalf("child=%#v", tree[0])
	}
	if supervisor.Active(tree[0].SessionID) {
		t.Fatal("child run still active")
	}
}
