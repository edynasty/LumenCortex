package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// T1-S5 against the real Engine, supervisor and SQLite: the message-level edges a run stores are read
// back by SQL.

type messageEdge struct {
	Kind, Target, SourceKind, Source, Origin, DerivedFrom string
	Lineage                                               int
}

func messageEdges(t *testing.T, engine *Engine, sessionID string) []messageEdge {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT kind, target_ref, source_kind, source_ref, origin_class, COALESCE(derived_from, ''), lineage_complete
		FROM derived_origins WHERE session_id = ? AND kind IN ('message', 'child_session') ORDER BY id`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []messageEdge
	for rows.Next() {
		var e messageEdge
		if err := rows.Scan(&e.Kind, &e.Target, &e.SourceKind, &e.Source, &e.Origin, &e.DerivedFrom, &e.Lineage); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

// spawnWaitProvider: the parent spawns one child, waits for it, and finishes; the child answers.
type spawnWaitProvider struct {
	mu      sync.Mutex
	parent  int
	childID string
}

func (p *spawnWaitProvider) Model() string { return "spawn-wait" }

func (p *spawnWaitProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !hasTool(req, "spawn_subagent") {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child evidence"}, FinishReason: "stop"}, nil
	}
	p.parent++
	switch p.parent {
	case 1:
		return toolCalls(tc("spawn-1", "spawn_subagent", `{"goal":"inspect"}`)), nil
	case 2:
		var node SubagentNode
		_ = json.Unmarshal([]byte(lastToolResult(req, "spawn-1")), &node)
		p.childID = node.SessionID
		return toolCalls(tc("wait-1", "wait_subagent", `{"child_session_id":"`+node.SessionID+`","timeout_seconds":10}`)), nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "parent done"}, FinishReason: "stop"}, nil
}

func hasTool(req protocol.ProviderRequest, name string) bool {
	for _, spec := range req.Tools {
		if spec.Name == name {
			return true
		}
	}
	return false
}

// The spawned child session derives from the parent request that spawned it; the wait_subagent row
// derives from the child as of its newest seq, complete because the child's answered main request is.
func TestRealEngineSubagentEdges(t *testing.T) {
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
	p := &spawnWaitProvider{}
	parent := newParentSession(t, engine, p)
	if _, err := supervisor.Start(context.Background(), parent, AgentOptions{MaxSteps: 6}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, parent)
	if p.childID == "" {
		t.Fatal("the parent never saw a spawned child")
	}
	waitInactive(t, supervisor, p.childID)

	var spawn, wait *messageEdge
	edges := messageEdges(t, engine, parent)
	for i := range edges {
		switch {
		case edges[i].Kind == "child_session" && edges[i].Target == "session:"+p.childID:
			spawn = &edges[i]
		case edges[i].Kind == "message" && strings.HasSuffix(edges[i].Target, ":wait-1"):
			wait = &edges[i]
		}
	}
	if spawn == nil || spawn.SourceKind != "request" || spawn.Origin != "agent_generated" || spawn.Lineage != 1 {
		t.Fatalf("spawn edge %+v in %+v", spawn, edges)
	}
	childSeq := mustNewestSeq(t, engine, p.childID)
	if wait == nil || wait.SourceKind != "session" || wait.Source != p.childID || wait.Lineage != 1 ||
		!strings.HasPrefix(wait.DerivedFrom, "session:"+p.childID+"@") {
		t.Fatalf("wait edge %+v in %+v (child newest seq %d)", wait, edges, childSeq)
	}
	if !strings.HasPrefix(wait.Target, "tool:") {
		t.Fatalf("wait edge target %q", wait.Target)
	}
}

// A child the user starts from a proposal derives from the proposal entry; the proposal entry derives
// from the request whose start_task wrote it, and the delivered task result derives from the child as of
// the delivered seq.
func TestRealEngineProposedTaskEdges(t *testing.T) {
	root, escape := taskRepo(t)
	engine, supervisor := openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	fake := &taskFake{escape: escape}
	parent := newParentSession(t, engine, fake)
	ctx := context.Background()

	startAndWait(t, supervisor, parent, "please start the change", DelegationPropose)
	tasks := taskStateOf(t, engine, parent).Tasks
	if len(tasks) != 1 {
		t.Fatalf("tasks %+v", tasks)
	}
	started, err := supervisor.StartProposedTask(ctx, parent, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, started.SessionID)
	startAndWait(t, supervisor, parent, "", DelegationPropose)

	var proposal, child, result *messageEdge
	edges := messageEdges(t, engine, parent)
	for i := range edges {
		e := &edges[i]
		switch {
		case e.Kind == "message" && strings.HasPrefix(e.Target, "ts:task:"+tasks[0].ID+"@") && e.SourceKind == "request" && proposal == nil:
			proposal = e
		case e.Kind == "child_session" && e.Target == "session:"+started.SessionID:
			child = e
		case e.Kind == "message" && strings.HasPrefix(e.Target, "task_result:"+started.SessionID+"@"):
			result = e
		}
	}
	if proposal == nil || proposal.Origin != "agent_generated" || proposal.Lineage != 1 {
		t.Fatalf("proposal edge %+v in %+v", proposal, edges)
	}
	if child == nil || child.SourceKind != "message" || child.Source != proposal.Target || child.Lineage != 1 {
		t.Fatalf("child edge %+v, want from %s, in %+v", child, proposal.Target, edges)
	}
	seq := mustNewestSeq(t, engine, started.SessionID)
	if result == nil || result.Source != started.SessionID || result.DerivedFrom != "session:"+started.SessionID+"@"+strconv.FormatInt(seq, 10) {
		t.Fatalf("task result edge %+v, want the child as of seq %d, in %+v", result, seq, edges)
	}
}

// A child the model starts directly (start_task under isolated delegation) derives from the request
// whose start_task started it.
func TestRealEngineDirectTaskEdge(t *testing.T) {
	root, escape := taskRepo(t)
	engine, supervisor := openTaskEngine(t, root)
	defer engine.Close()
	defer supervisor.Close()
	parent := newParentSession(t, engine, &taskFake{escape: escape})
	startAndWait(t, supervisor, parent, "please start the change", DelegationIsolated)
	tasks := taskStateOf(t, engine, parent).Tasks
	if len(tasks) != 1 || tasks[0].SessionID == "" {
		t.Fatalf("tasks %+v", tasks)
	}
	waitInactive(t, supervisor, tasks[0].SessionID)
	for _, e := range messageEdges(t, engine, parent) {
		if e.Kind == "child_session" && e.Target == "session:"+tasks[0].SessionID {
			if e.SourceKind != "request" || e.Origin != "agent_generated" || e.Lineage != 1 {
				t.Fatalf("child edge %+v", e)
			}
			return
		}
	}
	t.Fatalf("no child_session edge for %s", tasks[0].SessionID)
}

// The Opus launder sequence (opus-review/round2/launder) rewritten for T1-S5: thread A's model writes a
// note citing cache.go; the user rewrites its wording through the host edit path; a later thread C
// recalls A. C sees the rewrite as the user's wording of agent text that needs review, not as a user
// decision, with the agent's cite kept as a review source that still turns stale when cache.go changes.
// The rewrite's edge derives from the agent version, which derives from A's request, both complete.
func TestRealEngineUserRewriteDoesNotLaunderAgentText(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, root := gitEngine(t)
	if err := os.WriteFile(filepath.Join(root, "cache.go"), []byte("package cache // reentrant loader\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := &threadProvider{responses: []protocol.ProviderResponse{
		toolCalls(tc("n", "note_write", `{"text":"cacheMu is held while the loader runs.","cites":["cache.go"]}`)),
		{Message: protocol.Message{Role: "assistant", Content: "Found the lock order."}, FinishReason: "stop"},
	}}
	bindRef(engine, "a", a)
	threadA := newTestSession(t, engine, "Investigate deadlock in cache refresh")
	if r := run(t, engine, threadA.ID, AgentOptions{MaxSteps: 4}); r.Status != "completed" {
		t.Fatalf("thread A %+v", r)
	}
	info, err := engine.TaskState(ctx, threadA.ID)
	if err != nil || len(info.State.Notes) != 1 {
		t.Fatalf("thread A notes %+v, %v", info.State.Notes, err)
	}
	agentNote := info.State.Notes[0]
	if _, err := engine.EditTaskState(ctx, threadA.ID, TaskStateEdit{Note: &ThreadNoteEdit{ID: agentNote.ID, Text: "cacheMu is held while the loader runs; never call the loader under it."}}); err != nil {
		t.Fatal(err)
	}
	second := &threadProvider{}
	bindRef(engine, "a", second)
	if r := run(t, engine, threadA.ID, AgentOptions{MaxSteps: 2}); r.Status != "completed" {
		t.Fatalf("thread A second run %+v", r)
	}

	threadC := newTestSession(t, engine, "Another cache change")
	hits, err := engine.Recall(ctx, "cacheMu loader", 8, threadC.ID)
	if err != nil {
		t.Fatal(err)
	}
	var note *ThreadNote
	for i := range hits {
		if hits[i].ID == threadA.ID && len(hits[i].Notes) == 1 {
			note = &hits[i].Notes[0]
		}
	}
	if note == nil || !note.NeedsReview || note.By != "user" || len(note.Cites) != 1 || note.Cites[0].Path != "cache.go" || note.Stale {
		t.Fatalf("C recalls %+v, want a fresh user rewrite that needs review with cache.go to review", note)
	}
	var block string
	for _, m := range second.requests[0].Messages {
		if strings.Contains(m.Content, "Notes (") {
			block = m.Content
		}
	}
	if !strings.Contains(block, "[user edit of agent text, needs review]") || strings.Contains(block, "["+"user]: cacheMu") {
		t.Fatalf("Task State block presents the rewrite as a user decision:\n%s", block)
	}

	if err := os.WriteFile(filepath.Join(root, "cache.go"), []byte("package cache // loader now runs outside cacheMu\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := engine.ReadThread(ctx, threadA.ID)
	if err != nil || len(view.Notes) != 1 || !view.Notes[0].Stale {
		t.Fatalf("after cache.go changed the rewrite is not stale: %+v, %v", view.Notes, err)
	}

	edges := messageEdges(t, engine, threadA.ID)
	var agentEdge, userEdge *messageEdge
	for i := range edges {
		e := &edges[i]
		if !strings.HasPrefix(e.Target, "ts:note:"+agentNote.ID+"@") {
			continue
		}
		switch e.SourceKind {
		case "request":
			agentEdge = e
		case "message":
			userEdge = e
		}
	}
	if agentEdge == nil || agentEdge.Origin != "agent_generated" || agentEdge.Lineage != 1 {
		t.Fatalf("agent note edge %+v in %+v", agentEdge, edges)
	}
	if userEdge == nil || userEdge.Origin != "user_decision" || userEdge.Source != agentEdge.Target || userEdge.DerivedFrom != agentEdge.Target || userEdge.Lineage != 1 {
		t.Fatalf("rewrite edge %+v, want from %s, in %+v", userEdge, agentEdge.Target, edges)
	}
}
