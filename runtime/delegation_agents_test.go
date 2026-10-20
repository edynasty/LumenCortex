package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// delegationAgents are the custom agents of the A3 tests (ADR-0009): one per launch kind, one
// conversation agent that neither delegation tool may run. audit runs on test/b63, fixer on test/b62.
const delegationAgents = `{
	"audit": {"use": "delegation", "policy": "read-only", "description": "Audits code for security issues.",
		"instructions": "AUDIT-RULES: cite file and line.", "models": [{"model": "test/b63"}]},
	"fixer": {"use": "delegation", "policy": "workspace", "description": "Fixes one failing test.",
		"models": [{"model": "test/b62"}]},
	"chat": {"use": "conversation", "description": "Talks."}
}`

// catalogWithAgents is the fixture catalog with an agents object.
func catalogWithAgents(agents string) string {
	return `{"agents": ` + agents + `, ` + strings.TrimPrefix(boundCatalog("test/b0"), "{")
}

// scriptedParent makes one tool call on the first request of a run, then answers. It keeps every
// request and the content of every tool result it was sent.
type scriptedParent struct {
	mu       sync.Mutex
	call     protocol.ToolCall
	requests []protocol.ProviderRequest
	results  []string
}

func (p *scriptedParent) Model() string { return "scripted-parent" }

func (p *scriptedParent) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		p.results = append(p.results, last.Content)
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "parent done"}, FinishReason: "stop"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{p.call}}, FinishReason: "tool_calls"}, nil
}

// workerFake is an execution child that writes one file and completes its unit, recording requests.
type workerFake struct {
	mu       sync.Mutex
	requests []protocol.ProviderRequest
}

func (p *workerFake) Model() string { return "worker-fake" }

func (p *workerFake) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	if req.Messages[len(req.Messages)-1].Role == "tool" {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child done"}, FinishReason: "stop"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
		call("w", "write_file", `{"path":"out.txt","content":"x\n"}`),
		call("u", "work_unit_update", `{"id":"task","status":"completed","summary":"done"}`),
	}}, FinishReason: "tool_calls"}, nil
}

func specOf(t *testing.T, req protocol.ProviderRequest, name string) protocol.ToolSpec {
	t.Helper()
	for _, spec := range req.Tools {
		if spec.Name == name {
			return spec
		}
	}
	t.Fatalf("%s not offered: %v", name, toolNames(req))
	return protocol.ToolSpec{}
}

// agentEnum is the enum of a delegation tool's agent parameter, as the wire request marshals it.
func agentEnum(t *testing.T, spec protocol.ToolSpec) []string {
	t.Helper()
	raw, err := json.Marshal(spec.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Agent struct {
				Enum []string `json:"enum"`
			} `json:"agent"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	decodeJSON(t, raw, &schema)
	if slices.Contains(schema.Required, "agent") {
		t.Fatalf("%s requires agent; omitting it must keep the default", spec.Name)
	}
	return schema.Properties.Agent.Enum
}

func worktreeCount(t *testing.T, root string) int {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(out), "worktree ")
}

// Acceptance 1: each delegation tool's enum lists exactly the agents its launch kind may run, the
// built-in default first, and the description lists them as "- id: description" lines.
func TestDelegationToolsOfferOnlyEligibleAgents(t *testing.T) {
	useGlobalConfig(t, catalogWithAgents(delegationAgents))
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	p := &recordingProvider{final: "ok"}
	parent := newParentSession(t, engine, p)
	if _, err := supervisor.Start(context.Background(), parent, AgentOptions{Input: "hi", Interactive: true, Delegation: DelegationIsolated, MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, parent, 5*time.Second)

	spawn := specOf(t, p.requests[0], "spawn_subagent")
	task := specOf(t, p.requests[0], "start_task")
	if got := agentEnum(t, spawn); !slices.Equal(got, []string{config.AgentExplore, "audit"}) {
		t.Fatalf("spawn_subagent enum %v", got)
	}
	if got := agentEnum(t, task); !slices.Equal(got, []string{config.AgentWorker, "fixer"}) {
		t.Fatalf("start_task enum %v", got)
	}
	if !strings.Contains(spawn.Description, "\n- audit: Audits code for security issues.") || strings.Contains(spawn.Description, "fixer") {
		t.Fatalf("spawn_subagent description %q", spawn.Description)
	}
	if !strings.Contains(task.Description, "\n- fixer: Fixes one failing test.") || strings.Contains(task.Description, "audit") {
		t.Fatalf("start_task description %q", task.Description)
	}
	for _, spec := range []protocol.ToolSpec{spawn, task} {
		if strings.Contains(spec.Description, "chat") || strings.Contains(spec.Description, "\n- agent:") || strings.Contains(spec.Description, "\n- ask:") {
			t.Fatalf("%s lists a conversation agent: %q", spec.Name, spec.Description)
		}
	}
}

// Acceptance 2 and 5: spawn with a custom read-only agent pins it on the child, which runs its own
// model with the agent's instructions after the launch prompt and no write tool. After a crash and
// Reconcile, a host resume asking for full policy keeps the agent and its read-only authority.
// Ineligible and unknown agents start nothing.
func TestSpawnRunsTheChosenAgentAndResumeKeepsIt(t *testing.T) {
	useGlobalConfig(t, catalogWithAgents(delegationAgents))
	workspace := t.TempDir()
	engine := openEngineAt(t, workspace)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	parent := newParentSession(t, engine, &recordingProvider{final: "parent"})
	child := &recordingProvider{final: "found it"}
	bindRef(engine, "b63", child)
	ctx := context.Background()
	controller := &subagentController{supervisor: supervisor, parentID: parent}

	for agentID, want := range map[string]error{"fixer": ErrAgentUseMismatch, "chat": ErrAgentUseMismatch, "nobody": ErrUnknownAgent} {
		if _, err := controller.Spawn(ctx, "inspect", agentID); !errors.Is(err, want) {
			t.Fatalf("%s: err=%v, want %v", agentID, err, want)
		}
	}
	if tree, _ := supervisor.SubagentTree(ctx, parent); len(tree) != 0 {
		t.Fatalf("a refused spawn left a child: %+v", tree)
	}

	value, err := controller.Spawn(ctx, "inspect", "audit")
	if err != nil {
		t.Fatal(err)
	}
	childID := value.(SubagentNode).SessionID
	waitUntilIdle(t, supervisor, childID, 5*time.Second)
	info, _ := sessionState(t, engine, childID)
	if info.Agent != "audit" || info.ModelRef != "test/b63" || info.Final == nil || *info.Final != "found it" {
		t.Fatalf("child %+v", info)
	}
	systems := systemContents(child.requests[0].Messages)
	if len(systems) < 2 || !strings.Contains(systems[0], "read-only LumenCortex subagent") || systems[len(systems)-1] != "AUDIT-RULES: cite file and line." {
		t.Fatalf("child system messages %q", systems)
	}
	for _, forbidden := range []string{"write_file", "shell", "spawn_subagent", "start_task"} {
		if containsString(toolNames(child.requests[0]), forbidden) {
			t.Fatalf("read-only child has %s", forbidden)
		}
	}

	// Crash: the child is recorded running when its process dies; the next Open reconciles it.
	running := agent.StatusRunning
	if err := engine.store.Update(ctx, childID, session.Patch{Status: &running}); err != nil {
		t.Fatal(err)
	}
	supervisor.Close()
	engine.Close()
	engine = openEngineAt(t, workspace)
	supervisor, err = NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	if info, cause := sessionState(t, engine, childID); info.Status != "interrupted" || cause == nil || cause.Kind != session.KindStaleRunRecovered {
		t.Fatalf("after reopen: %q %+v", info.Status, cause)
	}
	resumed := &recordingProvider{final: "resumed"}
	bindRef(engine, "b63", resumed)
	if _, err := supervisor.Start(ctx, childID, AgentOptions{Input: "continue", Policy: "full", Delegation: DelegationIsolated, MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, childID, 5*time.Second)
	if info, _ := sessionState(t, engine, childID); info.Agent != "audit" || info.ModelRef != "test/b63" {
		t.Fatalf("resumed child %+v", info)
	}
	names := toolNames(resumed.requests[0])
	if containsString(names, "write_file") || containsString(names, "shell") || containsString(names, "start_task") {
		t.Fatalf("resume widened the read-only agent: %v", names)
	}
	entries := steps(t, engine, childID)
	if rule := agentRule(entries[len(entries)-1]); rule != agent.RuleAgentSession {
		t.Fatalf("resume agent rule %q", rule)
	}
}

// Acceptance 3: start_task naming a read-only agent is a tool error that starts no session and makes
// no worktree; the runtime refuses it again however it is reached.
func TestStartTaskRefusesAnIneligibleAgent(t *testing.T) {
	useGlobalConfig(t, catalogWithAgents(delegationAgents))
	root, _ := taskRepo(t)
	engine, supervisor := openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	ctx := context.Background()
	p := &scriptedParent{call: call("start", "start_task", `{"objective":"CHILD: fix it","agent":"audit"}`)}
	parent := newParentSession(t, engine, p)
	worktrees := worktreeCount(t, root)

	startAndWait(t, supervisor, parent, "please start", DelegationIsolated)
	if len(p.results) != 1 || !strings.Contains(p.results[0], `cannot run agent \"audit\"`) || !strings.Contains(p.results[0], "worker, fixer") {
		t.Fatalf("tool result %q", p.results)
	}
	if _, err := supervisor.startExecutionTask(ctx, parent, "CHILD: fix it", "", "audit", AgentOptions{}); !errors.Is(err, ErrAgentUseMismatch) {
		t.Fatalf("runtime start with audit: %v", err)
	}
	if tree, _ := supervisor.SubagentTree(ctx, parent); len(tree) != 0 {
		t.Fatalf("children %+v", tree)
	}
	if sessions, _ := engine.ListSessions(ctx, 10, 0); len(sessions) != 1 {
		t.Fatalf("sessions %d, want the parent only", len(sessions))
	}
	if worktreeCount(t, root) != worktrees {
		t.Fatal("a refused task made a worktree")
	}
	if tasks := taskStateOf(t, engine, parent).Tasks; len(tasks) != 0 {
		t.Fatalf("a refused task was recorded: %+v", tasks)
	}
}

// Acceptance 4: a proposal records its agent and the proposing run's bounds; the user's Start runs
// that agent on its model under those bounds and that denylist. The agent is checked again then: a
// proposal whose agent stopped being eligible starts nothing.
func TestProposalRecordsAgentAndParentBounds(t *testing.T) {
	useGlobalConfig(t, catalogWithAgents(delegationAgents))
	root, _ := taskRepo(t)
	engine, supervisor := openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	ctx := context.Background()
	p := &scriptedParent{call: call("start", "start_task", `{"objective":"CHILD: fix the test","agent":"fixer"}`)}
	parent := newParentSession(t, engine, p)
	worker := &workerFake{}
	bindRef(engine, "b62", worker)

	opts := AgentOptions{Input: "please propose", Interactive: true, Delegation: DelegationPropose,
		MaxSteps: 5, MaxToolCallsPerStep: 3, MaxTokens: 500, ToolDenylist: []string{"replace_in_file"}}
	if _, err := supervisor.Start(ctx, parent, opts); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, parent)
	tasks := taskStateOf(t, engine, parent).Tasks
	want := taskstate.TaskBounds{MaxSteps: 5, MaxToolCallsPerStep: 3, MaxTokens: 500, ToolDenylist: []string{"replace_in_file"}}
	if len(tasks) != 1 || tasks[0].Agent != "fixer" || tasks[0].Bounds == nil || !slices.Equal(tasks[0].Bounds.ToolDenylist, want.ToolDenylist) ||
		tasks[0].Bounds.MaxSteps != 5 || tasks[0].Bounds.MaxToolCallsPerStep != 3 || tasks[0].Bounds.MaxTokens != 500 {
		t.Fatalf("proposal %+v", tasks)
	}

	started, err := supervisor.StartProposedTask(ctx, parent, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, started.SessionID)
	info, _ := sessionState(t, engine, started.SessionID)
	if info.Agent != "fixer" || info.ModelRef != "test/b62" {
		t.Fatalf("child %+v", info)
	}
	first := worker.requests[0]
	if first.MaxTokens != 500 || containsString(toolNames(first), "replace_in_file") || !containsString(toolNames(first), "write_file") {
		t.Fatalf("child request maxTokens=%d tools=%v", first.MaxTokens, toolNames(first))
	}
	if tree, _ := supervisor.SubagentTree(ctx, parent); len(tree) != 1 || tree[0].Agent != "fixer" {
		t.Fatalf("tree %+v", tree)
	}
	// The next Start of the parent delivers the result, naming the agent that produced it.
	startAndWait(t, supervisor, parent, "", DelegationPropose)
	if rows := taskResultRows(t, engine, parent); len(rows) != 1 || rows[0].Agent != "fixer" {
		t.Fatalf("task_result %+v", rows)
	}

	// The scripted parent proposed again in that delivery run. Its agent then stops being eligible:
	// the user's Start of that proposal is refused and starts nothing.
	tasks = taskStateOf(t, engine, parent).Tasks
	if len(tasks) != 2 || tasks[1].State != taskstate.TaskProposed || tasks[1].Agent != "fixer" {
		t.Fatalf("second proposal %+v", tasks)
	}
	useGlobalConfig(t, catalogWithAgents(`{"fixer": {"use": "delegation", "policy": "read-only", "description": "Now read-only."}}`))
	worktrees := worktreeCount(t, root)
	if _, err := supervisor.StartProposedTask(ctx, parent, tasks[1].ID); !errors.Is(err, ErrAgentUseMismatch) {
		t.Fatalf("ineligible proposal: %v", err)
	}
	if worktreeCount(t, root) != worktrees {
		t.Fatal("a refused proposal made a worktree")
	}
	if got := taskStateOf(t, engine, parent).Tasks[1]; got.State != taskstate.TaskProposed {
		t.Fatalf("refused proposal %+v", got)
	}
}

// A3: a child runs its own agent's effort, not the thread's per-Start override. The thread asks for
// max; the explore child of a thread configured with explore {effort: low} sends low.
func TestChildRunsItsAgentEffortNotTheThreadOverride(t *testing.T) {
	useGlobalConfig(t, `{"model": "test/a", "agents": {"explore": {"effort": "low", "models": [{"model": "test/b"}]}},
	  "providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {
	  "a": {"modelID": "a", "limit": {"context": 8000, "output": 1000}, "reasoning": {"format": "reasoning-effort", "efforts": ["low", "medium", "high", "max"]}},
	  "b": {"modelID": "b", "limit": {"context": 8000, "output": 1000}, "reasoning": {"format": "reasoning-effort", "efforts": ["low", "medium", "high", "max"]}}}}}}`)
	engine := openTestEngine(t)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	parentP, childP := &recordingProvider{final: "parent"}, &recordingProvider{final: "child"}
	bindRef(engine, "a", parentP)
	bindRef(engine, "b", childP)
	handle := newTestSession(t, engine, "parent")
	parentOpts := AgentOptions{Effort: "max", MaxSteps: 2}
	if r := run(t, engine, handle.ID, parentOpts); r.Status != "completed" {
		t.Fatalf("parent %+v", r)
	}
	if got := parentP.requests[0].ReasoningEffort; got != "max" {
		t.Fatalf("parent effort %q", got)
	}
	controller := &subagentController{supervisor: supervisor, parentID: handle.ID, parentOpts: parentOpts}
	value, err := controller.Spawn(context.Background(), "inspect", "")
	if err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, value.(SubagentNode).SessionID, 5*time.Second)
	if got := childP.requests[0].ReasoningEffort; got != "low" {
		t.Fatalf("child effort %q, want explore's low", got)
	}
}
