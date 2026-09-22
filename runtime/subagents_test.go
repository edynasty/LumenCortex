package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/resource"
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
		Message: protocol.Message{Role: "assistant", Content: "subagent done"},
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

func newParentSession(t *testing.T, engine *Engine) string {
	t.Helper()
	handle, err := engine.NewSession(context.Background(), SessionOptions{
		Goal:     "parent task",
		Provider: "test-provider",
		Model:    "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	return handle.ID
}

func TestSubagentChildOptionsAreReadOnlyAndNonRecursive(t *testing.T) {
	controller := &subagentController{
		parentID: "parent",
		parentOpts: AgentOptions{
			ProviderName:        "provider",
			Policy:              toolset.PolicyFull,
			MaxSteps:            100,
			RecentMessages:      100,
			MaxToolCallsPerStep: 100,
		},
	}
	opts := controller.childOptions()
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
	if opts.MaxSteps != 12 || opts.RecentMessages != 8 || opts.MaxToolCallsPerStep != 6 {
		t.Fatalf("bounded child options=%#v", opts)
	}
}

func TestSubagentControllerEnforcesFourActiveChildren(t *testing.T) {
	engine, err := Open(Options{
		Workspace: t.TempDir(),
		Budget:    Budget{MaxAgents: 8},
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

	parentID := newParentSession(t, engine)
	provider := &multiBlockingProvider{}
	controller := &subagentController{
		supervisor: supervisor,
		parentID:   parentID,
		provider:   provider,
		parentOpts: AgentOptions{ProviderName: "test-provider"},
	}

	children := make([]string, 0, MaxConcurrentSubagentsPerParent)
	for i := 0; i < MaxConcurrentSubagentsPerParent; i++ {
		value, err := controller.Spawn(context.Background(), "inspect focused area")
		if err != nil {
			t.Fatalf("spawn %d: %v", i, err)
		}
		node, ok := value.(SubagentNode)
		if !ok {
			t.Fatalf("spawn result type=%T", value)
		}
		children = append(children, node.SessionID)
	}
	if _, err := controller.Spawn(context.Background(), "one too many"); !errors.Is(err, ErrSubagentLimitExceeded) {
		t.Fatalf("fifth spawn err=%v", err)
	}

	tree, err := supervisor.SubagentTree(context.Background(), parentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != MaxConcurrentSubagentsPerParent {
		t.Fatalf("tree=%#v", tree)
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

	parentID := newParentSession(t, engine)
	parentProvider := &multiBlockingProvider{started: make(chan struct{})}
	if _, err := supervisor.Start(context.Background(), parentID, parentProvider, AgentOptions{
		ProviderName: "test-provider",
		Policy:       toolset.PolicyReadOnly,
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
		provider:   &multiBlockingProvider{},
		parentOpts: AgentOptions{ProviderName: "test-provider"},
	}
	first, err := controller.Spawn(context.Background(), "first child")
	if err != nil {
		t.Fatal(err)
	}
	firstNode := first.(SubagentNode)

	if _, err := controller.Spawn(context.Background(), "second child"); !errors.Is(err, resource.ErrAgentLimitExceeded) {
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

	parentID := newParentSession(t, engine)
	events, unsubscribe := engine.Events(64)
	defer unsubscribe()

	controller := &subagentController{
		supervisor: supervisor,
		parentID:   parentID,
		provider:   &finalProvider{},
		parentOpts: AgentOptions{ProviderName: "test-provider"},
	}
	value, err := controller.Spawn(context.Background(), "finish quickly")
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
