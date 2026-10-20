package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// childMarker leads every execution task objective in these tests, so the fake tells the child's
// requests (whose goal is the objective) from the parent's.
const childMarker = "CHILD:"

// taskFake answers both the parent thread and its execution children (ADR-0006 D4, C2).
type taskFake struct {
	mu sync.Mutex
	// child decides the child's response; nil writes inside, tries every escape, completes the unit.
	child func(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error)
	// escape is the directory outside the child's worktree that the escape writes target.
	escape string

	parentTools    [][]string
	childTools     []string
	childResults   []protocol.Message
	startResults   []string
	seenTaskResult []string
}

func (p *taskFake) Model() string { return "task-fake" }

func toolNames(req protocol.ProviderRequest) []string {
	names := make([]string, 0, len(req.Tools))
	for _, spec := range req.Tools {
		names = append(names, spec.Name)
	}
	return names
}

func isChildRequest(req protocol.ProviderRequest) bool {
	for _, m := range req.Messages {
		// The parent's Task State block lists its tasks' objectives; only the child's goal row counts.
		if m.Role == "user" && strings.Contains(m.Content, childMarker) && m.Origin != protocol.OriginTaskResult &&
			!strings.HasPrefix(m.Content, taskstate.BlockHeader) {
			return true
		}
	}
	return false
}

func call(id, name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

// childReasoning is opaque provider reasoning the child's model returns with every answer. It lives
// only in the child's run memory; no task_result row or table may hold it (AGENTS.md).
const childReasoning = "SECRET-CHILD-REASONING-91c2"

func childEcho() protocol.Echo {
	return protocol.NewEcho("reasoning_content", []byte(childReasoning), 8)
}

func (p *taskFake) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if isChildRequest(req) {
		p.mu.Lock()
		p.childTools = toolNames(req)
		child := p.child
		p.mu.Unlock()
		if child != nil {
			return child(ctx, req)
		}
		var results []protocol.Message
		for _, m := range req.Messages {
			if m.Role == "tool" {
				results = append(results, m)
			}
		}
		if len(results) > 0 {
			p.mu.Lock()
			p.childResults = results
			p.mu.Unlock()
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child done"}, FinishReason: "stop", Echo: childEcho()}, nil
		}
		outside, _ := json.Marshal(filepath.Join(p.escape, "absolute.txt"))
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
			call("w-in", "write_file", `{"path":"out.txt","content":"child work\n"}`),
			call("w-dotdot", "write_file", `{"path":"../dotdot.txt","content":"x"}`),
			call("w-abs", "write_file", `{"path":`+string(outside)+`,"content":"x"}`),
			call("w-link", "write_file", `{"path":"link/symlinked.txt","content":"x"}`),
			call("u", "work_unit_update", `{"id":"task","status":"completed","summary":"wrote out.txt"}`),
		}}, FinishReason: "tool_calls", Echo: childEcho()}, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.parentTools = append(p.parentTools, toolNames(req))
	var sawTool bool
	for _, m := range req.Messages {
		switch {
		case m.Origin == protocol.OriginTaskResult:
			p.seenTaskResult = append(p.seenTaskResult, m.Content)
		case m.Role == "tool" && m.ToolCallID == "start":
			p.startResults = append(p.startResults, m.Content)
		}
		if m.Role == "tool" {
			sawTool = true
		}
	}
	last := req.Messages[len(req.Messages)-1]
	if !sawTool && last.Role == "user" && strings.Contains(last.Content, "please start") {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
			call("start", "start_task", `{"objective":"`+childMarker+` write out.txt"}`),
		}}, FinishReason: "tool_calls"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "parent answer"}, FinishReason: "stop"}, nil
}

func (p *taskFake) lastParentTools() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.parentTools) == 0 {
		return nil
	}
	return p.parentTools[len(p.parentTools)-1]
}

func contains(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// taskRepo is a git workspace with one commit that holds link, a symlink to escape outside it.
func taskRepo(t *testing.T) (root, escape string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root = t.TempDir()
	escape = t.TempDir()
	initGitRepo(t, root)
	if err := os.Symlink(escape, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "link"}, {"commit", "-m", "link"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return root, escape
}

func gitHead(t *testing.T, root string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func openTaskEngine(t *testing.T, root string) (*Engine, *RunSupervisor) {
	t.Helper()
	engine, err := Open(Options{Workspace: root, Budget: Budget{MaxAgents: 8}})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		engine.Close()
		t.Fatal(err)
	}
	return engine, supervisor
}

func waitInactive(t *testing.T, s *RunSupervisor, ids ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, id := range ids {
		for s.Active(id) {
			if time.Now().After(deadline) {
				t.Fatalf("session %s is still running", id)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// startAndWait starts the parent with input (none when empty) and waits for its run to end.
func startAndWait(t *testing.T, s *RunSupervisor, id, input, delegation string) {
	t.Helper()
	if _, err := s.Start(context.Background(), id, AgentOptions{Input: input, Interactive: true, Delegation: delegation, MaxSteps: 4}); err != nil {
		t.Fatalf("Start(%s): %v", id, err)
	}
	waitInactive(t, s, id)
}

func taskResultRows(t *testing.T, e *Engine, id string) []TaskResult {
	t.Helper()
	var out []TaskResult
	err := e.store.EachMessageFrom(context.Background(), id, 0, func(row session.Message) error {
		var m protocol.Message
		if err := json.Unmarshal(row.JSON, &m); err != nil {
			return err
		}
		if m.Origin != protocol.OriginTaskResult {
			return nil
		}
		header, body, ok := strings.Cut(m.Content, "\n")
		if !ok || header != protocol.TaskResultHeader {
			t.Fatalf("task_result content = %q", m.Content)
		}
		var r TaskResult
		decodeJSON(t, []byte(body), &r)
		out = append(out, r)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func taskStateOf(t *testing.T, e *Engine, id string) taskstate.State {
	t.Helper()
	info, err := e.TaskState(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return info.State
}

// Without a grant start_task is not offered and a call to it is denied; nothing is recorded or
// started. isolated without the supervisor is refused before the Start writes anything.
func TestStartTaskNeedsADelegationGrant(t *testing.T) {
	root, _ := taskRepo(t)
	engine, supervisor := openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	fake := &taskFake{}
	parent := newParentSession(t, engine, fake)

	startAndWait(t, supervisor, parent, "please start the change", "")
	if names := fake.lastParentTools(); contains(names, "start_task") {
		t.Fatalf("start_task offered without a grant: %v", names)
	}
	// The loop denies a call outside the run's tool set before any tool runs.
	if len(fake.startResults) != 1 || !strings.Contains(fake.startResults[0], "not allowed") {
		t.Fatalf("start_task result without a grant = %v", fake.startResults)
	}
	if tasks := taskStateOf(t, engine, parent).Tasks; len(tasks) != 0 {
		t.Fatalf("tasks recorded without a grant: %+v", tasks)
	}
	if tree, err := supervisor.SubagentTree(context.Background(), parent); err != nil || len(tree) != 0 {
		t.Fatalf("children without a grant: %+v, %v", tree, err)
	}

	before, err := engine.store.NewestMessageSeq(context.Background(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.RunAgent(context.Background(), parent, AgentOptions{Input: "x", Delegation: DelegationIsolated}); err == nil {
		t.Fatal("RunAgent with delegation isolated was accepted")
	}
	if _, err := supervisor.Start(context.Background(), parent, AgentOptions{Input: "x", Delegation: "everything"}); err == nil {
		t.Fatal("unknown delegation was accepted")
	}
	if after, _ := engine.store.NewestMessageSeq(context.Background(), parent); after != before {
		t.Fatalf("a refused Start wrote rows: %d -> %d", before, after)
	}
}

// The whole propose path: the proposal waits for the user; StartProposedTask starts exactly it once in
// its own worktree at HEAD, confined to that worktree and without shell; deleting the parent is
// refused; after a restart the result is delivered once, on the next Start, and only once.
func TestProposedTaskRunsConfinedAndDeliversItsResultOnce(t *testing.T) {
	root, escape := taskRepo(t)
	head := gitHead(t, root)
	engine, supervisor := openTaskEngine(t, root)
	fake := &taskFake{escape: escape}
	parent := newParentSession(t, engine, fake)
	ctx := context.Background()

	startAndWait(t, supervisor, parent, "please start the change", DelegationPropose)
	if !contains(fake.lastParentTools(), "start_task") {
		t.Fatalf("start_task not offered under propose: %v", fake.lastParentTools())
	}
	if len(fake.startResults) != 1 || !strings.Contains(fake.startResults[0], `"approval_required"`) {
		t.Fatalf("propose result = %v", fake.startResults)
	}
	tasks := taskStateOf(t, engine, parent).Tasks
	if len(tasks) != 1 || tasks[0].State != taskstate.TaskProposed || tasks[0].SessionID != "" {
		t.Fatalf("proposal = %+v", tasks)
	}
	if tree, _ := supervisor.SubagentTree(ctx, parent); len(tree) != 0 {
		t.Fatalf("a proposal started a child: %+v", tree)
	}

	if _, err := supervisor.StartProposedTask(ctx, parent, "nope"); !errors.Is(err, ErrTaskProposalNotFound) {
		t.Fatalf("unknown proposal: %v", err)
	}
	started, err := supervisor.StartProposedTask(ctx, parent, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.StartProposedTask(ctx, parent, tasks[0].ID); !errors.Is(err, ErrTaskProposalStarted) {
		t.Fatalf("second start of one proposal: %v", err)
	}
	child := started.SessionID
	if started.Base != head || started.Runtime.Kind != RuntimeWorktree || started.Runtime.Owner != child {
		t.Fatalf("started = %+v, want an owned worktree at %s", started, head)
	}
	relation, err := engine.store.ParentRelation(ctx, child)
	if err != nil || relation.Kind != RelationExecution || relation.ParentSessionID != parent {
		t.Fatalf("relation = %+v, %v", relation, err)
	}
	if row, _ := engine.store.Get(ctx, child); row.Goal != tasks[0].Objective {
		t.Fatalf("child goal %q, proposal %q", row.Goal, tasks[0].Objective)
	}
	waitInactive(t, supervisor, child)

	tools := fake.childTools
	for _, forbidden := range []string{"shell", "spawn_subagent", "start_task"} {
		if contains(tools, forbidden) {
			t.Fatalf("child has %s: %v", forbidden, tools)
		}
	}
	if !contains(tools, "write_file") {
		t.Fatalf("child cannot write: %v", tools)
	}
	results := map[string]protocol.Message{}
	for _, m := range fake.childResults {
		results[m.ToolCallID] = m
	}
	if strings.Contains(results["w-in"].Content, "error") {
		t.Fatalf("write inside the worktree failed: %s", results["w-in"].Content)
	}
	for _, id := range []string{"w-dotdot", "w-abs", "w-link"} {
		if !strings.Contains(results[id].Content, "error") {
			t.Fatalf("escape %s was not refused: %q", id, results[id].Content)
		}
	}
	for _, path := range []string{filepath.Join(filepath.Dir(started.Runtime.Path), "dotdot.txt"), filepath.Join(escape, "absolute.txt"), filepath.Join(escape, "symlinked.txt")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s exists after a refused escape (%v)", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(started.Runtime.Path, "out.txt")); err != nil {
		t.Fatalf("the child's write is not in its worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("the child wrote the main checkout (%v)", err)
	}

	if _, err := engine.DeleteSession(ctx, parent, WorktreeCleanup{}); !errors.Is(err, ErrSessionHasDependents) {
		t.Fatalf("deleting the parent of an execution task: %v", err)
	}

	// Crash and restart between the child's terminal transition and the parent's next Start.
	supervisor.Close()
	engine.Close()
	engine, supervisor = openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	bind(engine, fake)

	childRow, err := engine.store.Get(ctx, child)
	if err != nil || !terminalStatus(childRow.Status) {
		t.Fatalf("child = %+v, %v", childRow, err)
	}
	startAndWait(t, supervisor, parent, "", DelegationPropose)
	startAndWait(t, supervisor, parent, "", DelegationPropose)
	rows := taskResultRows(t, engine, parent)
	if len(rows) != 1 {
		t.Fatalf("task_result rows = %d, want 1: %+v", len(rows), rows)
	}
	got := rows[0]
	if got.TaskID != tasks[0].ID || got.SessionID != child || got.Status != childRow.Status || got.Final != "child done" || got.Handoff == nil ||
		got.Handoff.Branch != started.Runtime.Branch || got.Handoff.Base != head || got.Handoff.DirtyCount != 1 {
		t.Fatalf("task_result = %+v", got)
	}
	if len(fake.seenTaskResult) == 0 {
		t.Fatal("the parent model never read the task_result row")
	}
	if !taskStateOf(t, engine, parent).Delivered(child, taskstate.TaskMark{Seq: mustNewestSeq(t, engine, child), Status: childRow.Status}) {
		t.Fatal("the delivery cursor did not advance")
	}
	// The child's visible answer reached the parent; its reasoning echo reached nothing durable.
	for _, seen := range fake.seenTaskResult {
		if strings.Contains(seen, childReasoning) {
			t.Fatal("child reasoning reached a task_result row")
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		raw, err := os.ReadFile(engine.store.Path() + suffix)
		if err == nil && strings.Contains(string(raw), childReasoning) {
			t.Fatalf("child reasoning is in %s", filepath.Base(engine.store.Path()+suffix))
		}
	}
}

func mustNewestSeq(t *testing.T, e *Engine, id string) int64 {
	t.Helper()
	seq, err := e.store.NewestMessageSeq(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// isolated starts the child from the tool call. It outlives the parent's run, Cancel(parent) does not
// reach it, CancelSubtree does; its interrupted transition is delivered, and an explicit resume of the
// child keeps its fixed authority and yields a second, newer transition that is delivered too.
func TestIsolatedTaskOutlivesTheParentAndStopsOnlyWithTheSubtree(t *testing.T) {
	root, _ := taskRepo(t)
	engine, supervisor := openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	ctx := context.Background()

	block := make(chan struct{})
	var blockOnce sync.Once
	fake := &taskFake{}
	fake.child = func(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		select {
		case <-block:
			if last := req.Messages[len(req.Messages)-1]; last.Role == "tool" {
				return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "resumed"}, FinishReason: "stop"}, nil
			}
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
				call("u", "work_unit_update", `{"id":"task","status":"completed","summary":"done"}`),
			}}, FinishReason: "tool_calls"}, nil
		default:
		}
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}
	parent := newParentSession(t, engine, fake)

	startAndWait(t, supervisor, parent, "please start the change", DelegationIsolated)
	if len(fake.startResults) != 1 || !strings.Contains(fake.startResults[0], `"started"`) {
		t.Fatalf("isolated result = %v", fake.startResults)
	}
	tasks := taskStateOf(t, engine, parent).Tasks
	if len(tasks) != 1 || tasks[0].State != taskstate.TaskStarted || tasks[0].SessionID == "" {
		t.Fatalf("tasks = %+v", tasks)
	}
	child := tasks[0].SessionID
	if !supervisor.Active(child) {
		t.Fatal("the child did not outlive the parent's run")
	}
	if supervisor.Cancel(parent) {
		t.Fatal("Cancel of an ended parent reported a run")
	}
	if !supervisor.Active(child) {
		t.Fatal("Cancel(parent) reached the child")
	}
	// A child that runs is not delivered.
	startAndWait(t, supervisor, parent, "status?", DelegationIsolated)
	if rows := taskResultRows(t, engine, parent); len(rows) != 0 {
		t.Fatalf("a running child was delivered: %+v", rows)
	}
	// An execution child that runs keeps the parent open (ADR-0006 D2) and is not "ready for review".
	if info, err := engine.TaskState(ctx, parent); err != nil || !contains(info.Obligations, taskstate.ObligationTasks) || info.ReadyForReview {
		t.Fatalf("parent view while its task runs = %+v, %v", info, err)
	}

	cancelled, err := supervisor.CancelSubtree(ctx, parent)
	if err != nil || len(cancelled) != 1 || cancelled[0] != child {
		t.Fatalf("CancelSubtree = %v, %v", cancelled, err)
	}
	waitInactive(t, supervisor, child)
	startAndWait(t, supervisor, parent, "", DelegationIsolated)
	rows := taskResultRows(t, engine, parent)
	if len(rows) != 1 || rows[0].Status != "interrupted" || rows[0].TaskID != tasks[0].ID {
		t.Fatalf("rows after stop = %+v", rows)
	}
	// An interrupted child is no longer an obligation, but it blocks acceptance: ready for review.
	if info, err := engine.TaskState(ctx, parent); err != nil || contains(info.Obligations, taskstate.ObligationTasks) || !info.ReadyForReview {
		t.Fatalf("parent view after its task stopped = %+v, %v", info, err)
	}

	// The host resumes the child explicitly, asking for full authority and delegation.
	blockOnce.Do(func() { close(block) })
	if _, err := supervisor.Start(ctx, child, AgentOptions{Policy: "full", Delegation: DelegationIsolated, Interactive: true}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, child)
	if tools := fake.childTools; contains(tools, "shell") || contains(tools, "start_task") || contains(tools, "spawn_subagent") {
		t.Fatalf("a resumed child widened its authority: %v", tools)
	}
	startAndWait(t, supervisor, parent, "", DelegationIsolated)
	rows = taskResultRows(t, engine, parent)
	if len(rows) != 2 || rows[1].Status == "interrupted" || rows[1].Final != "resumed" {
		t.Fatalf("rows after resume = %+v", rows)
	}
}

// limitFake proposes one task per "propose" turn, spawns a read-only subagent on a "spawn" turn and
// keeps every execution child running until its run is cancelled.
type limitFake struct {
	mu      sync.Mutex
	turns   int
	results map[string]string
}

func (p *limitFake) Model() string { return "limit-fake" }
func (p *limitFake) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if isChildRequest(req) {
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		p.results[last.ToolCallID] = last.Content
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "noted"}, FinishReason: "stop"}, nil
	}
	switch {
	case last.Role == "user" && last.Content == "propose":
		p.turns++
		id := fmt.Sprintf("start-%d", p.turns)
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
			call(id, "start_task", fmt.Sprintf(`{"objective":"%s task %d"}`, childMarker, p.turns)),
		}}, FinishReason: "tool_calls"}, nil
	case last.Role == "user" && last.Content == "spawn":
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
			call("spawn", "spawn_subagent", `{"goal":"read the README"}`),
		}}, FinishReason: "tool_calls"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "parent answer"}, FinishReason: "stop"}, nil
}

func (p *limitFake) result(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.results[id]
}

// Approved decision 5: read-only subagents and execution children share the per-parent limit
// (concurrency.maxChildrenPerParent, default 3). A start beyond it is refused without leaving a child,
// worktree or started record behind, and a slot freed by a subtree Stop admits the waiting proposal.
func TestExecutionChildrenAndSubagentsShareTheParentLimit(t *testing.T) {
	const limit = config.DefaultMaxChildrenPerParent
	root, _ := taskRepo(t)
	engine, supervisor := openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	ctx := context.Background()
	fake := &limitFake{results: map[string]string{}}
	parent := newParentSession(t, engine, fake)

	for i := 0; i < limit+1; i++ {
		startAndWait(t, supervisor, parent, "propose", DelegationPropose)
	}
	tasks := taskStateOf(t, engine, parent).Tasks
	if len(tasks) != limit+1 {
		t.Fatalf("proposals = %+v", tasks)
	}
	children := make([]string, 0, limit)
	for _, task := range tasks[:limit] {
		started, err := supervisor.StartProposedTask(ctx, parent, task.ID)
		if err != nil {
			t.Fatalf("StartProposedTask(%s): %v", task.ID, err)
		}
		children = append(children, started.SessionID)
	}
	for _, child := range children {
		if !supervisor.Active(child) {
			t.Fatalf("child %s is not running", child)
		}
	}

	waiting := tasks[limit]
	if _, err := supervisor.StartProposedTask(ctx, parent, waiting.ID); !errors.Is(err, ErrSubagentLimitExceeded) {
		t.Fatalf("execution child beyond the limit err = %v", err)
	}
	// A read-only subagent counts against the same running execution children.
	startAndWait(t, supervisor, parent, "spawn", DelegationPropose)
	if got := fake.result("spawn"); !strings.Contains(got, ErrSubagentLimitExceeded.Error()) {
		t.Fatalf("spawn_subagent beyond the limit = %q", got)
	}
	tree, err := supervisor.SubagentTree(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != limit {
		t.Fatalf("tree after refused starts = %+v", tree)
	}
	for _, node := range tree {
		if node.Kind != RelationExecution {
			t.Fatalf("refused start left a %s child: %+v", node.Kind, node)
		}
	}
	for _, task := range taskStateOf(t, engine, parent).Tasks {
		if task.ID == waiting.ID && (task.State != taskstate.TaskProposed || task.SessionID != "") {
			t.Fatalf("refused proposal changed: %+v", task)
		}
	}

	// Stopping one child's subtree frees exactly one slot.
	if cancelled, err := supervisor.CancelSubtree(ctx, children[0]); err != nil || len(cancelled) != 1 || cancelled[0] != children[0] {
		t.Fatalf("CancelSubtree(child) = %v, %v", cancelled, err)
	}
	waitInactive(t, supervisor, children[0])
	for _, child := range children[1:] {
		if !supervisor.Active(child) {
			t.Fatalf("stopping one child stopped sibling %s", child)
		}
	}
	started, err := supervisor.StartProposedTask(ctx, parent, waiting.ID)
	if err != nil {
		t.Fatalf("proposal after a freed slot: %v", err)
	}
	if _, err := supervisor.CancelSubtree(ctx, parent); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, append(children, started.SessionID)...)
}
