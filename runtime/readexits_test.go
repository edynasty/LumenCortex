package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

const (
	exitGoal  = "SENTINEL-CHILD-GOAL-5e1a"
	exitFinal = "SENTINEL-CHILD-FINAL-5e1a"
)

// exitsProvider answers a read-only child with exitFinal and its parent with one step that lists and
// waits for the child, then a text answer. It records the parent's requests.
type exitsProvider struct {
	mu       sync.Mutex
	child    string
	requests []protocol.ProviderRequest
}

func (p *exitsProvider) Model() string { return "exits" }

func (p *exitsProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	for _, m := range req.Messages {
		if m.Role == "user" && m.Content == exitGoal {
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: exitFinal}, FinishReason: "stop"}, nil
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "parent answer"}, FinishReason: "stop"}, nil
		}
	}
	return toolCalls(tc("l1", "list_subagents", `{}`), tc("w1", "wait_subagent", `{"child_session_id":"`+p.child+`","timeout_seconds":1}`)), nil
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func hasSentinel(s string) bool {
	return strings.Contains(s, exitGoal) || strings.Contains(s, exitFinal)
}

// Plan T3-S3 through the real Engine and SQLite. After a child subagent is revoked, every read exit
// that carried its content returns the same number of entries with no content of it, or marks it: the
// host's SubagentTree and SessionCheckpoints, the host's MessagePage and RecentMessages of the child
// (every row marked withheld) and of the parent (none marked), and the parent agent's list_subagents and
// wait_subagent, whose results then cite nothing - the parent's run completes instead of being refused
// for citing a fenced session.
func TestReadExitsAfterRevokingASubagent(t *testing.T) {
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
	ctx := context.Background()
	provider := &exitsProvider{}
	parent := newParentSession(t, engine, provider)
	controller := &subagentController{supervisor: supervisor, parentID: parent}
	value, err := controller.Spawn(ctx, exitGoal, "")
	if err != nil {
		t.Fatal(err)
	}
	child := value.(SubagentNode).SessionID
	waitInactive(t, supervisor, child)

	tree, err := supervisor.SubagentTree(ctx, parent)
	if err != nil || len(tree) != 1 || tree[0].Goal != exitGoal || tree[0].Final != exitFinal || tree[0].Withdrawn {
		t.Fatalf("tree before the revocation %+v %v", tree, err)
	}
	checkpoints, err := engine.SessionCheckpoints(ctx, parent, 50)
	if err != nil {
		t.Fatal(err)
	}
	childPage, err := engine.MessagePage(ctx, child, -1, 100)
	if err != nil || len(childPage.Messages) == 0 {
		t.Fatalf("child page %+v %v", childPage, err)
	}

	if _, err := engine.RevokeSource(ctx, SourceSession, child, "user_revoked"); err != nil {
		t.Fatal(err)
	}

	tree, err = supervisor.SubagentTree(ctx, parent)
	if err != nil || len(tree) != 1 || !tree[0].Withdrawn || tree[0].SessionID != child || tree[0].Status != "completed" || hasSentinel(jsonOf(t, tree)) {
		t.Fatalf("tree after the revocation %+v %v", tree, err)
	}
	after, err := engine.SessionCheckpoints(ctx, parent, 50)
	if err != nil || len(after) != len(checkpoints) || hasSentinel(jsonOf(t, after)) {
		t.Fatalf("checkpoints after the revocation: %d (before %d), %v: %s", len(after), len(checkpoints), err, jsonOf(t, after))
	}
	page, err := engine.MessagePage(ctx, child, -1, 100)
	if err != nil || len(page.Messages) != len(childPage.Messages) || page.HasMore != childPage.HasMore {
		t.Fatalf("child page after the revocation: %d rows (before %d), %v", len(page.Messages), len(childPage.Messages), err)
	}
	for _, m := range page.Messages {
		if !m.Withheld {
			t.Fatalf("child row %d not marked withheld", m.Seq)
		}
	}
	handle, _, err := engine.Session(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := handle.RecentMessages(ctx, 100)
	if err != nil || len(recent) != len(page.Messages) || !recent[0].Withheld {
		t.Fatalf("child recent messages %+v %v", recent, err)
	}
	parentPage, err := engine.MessagePage(ctx, parent, -1, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range parentPage.Messages {
		if m.Withheld {
			t.Fatalf("parent row %d marked withheld; the parent read nothing of the child", m.Seq)
		}
	}

	// The parent's agent reads the child through list_subagents and wait_subagent.
	provider.child = child
	if _, err := supervisor.Start(ctx, parent, AgentOptions{Input: "check on the child", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, parent)
	if info, cause := sessionState(t, engine, parent); info.Status != "completed" {
		t.Fatalf("parent run %s, cause %+v", info.Status, cause)
	}
	provider.mu.Lock()
	requests := provider.requests
	provider.mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("parent requests %d, want 2", len(requests))
	}
	tools := 0
	for _, m := range requests[1].Messages {
		if hasSentinel(m.Content) {
			t.Fatalf("the parent's request carries the revoked child in a %s row: %q", m.Role, m.Content)
		}
		if m.Role == "tool" {
			tools++
			if !strings.Contains(m.Content, `"withdrawn":true`) {
				t.Fatalf("tool row %s does not mark the child withdrawn: %q", m.ToolCallID, m.Content)
			}
		}
	}
	if tools != 2 {
		t.Fatalf("tool rows %d, want 2", tools)
	}
	if n := countRows(t, engine, `SELECT count(*) FROM derived_origins WHERE session_id = ? AND source_kind = 'session' AND source_ref = ?`, parent, child); n != 0 {
		t.Fatalf("the parent stored %d edges citing the withdrawn child", n)
	}
}

func countRows(t *testing.T, engine *Engine, query string, args ...any) int {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Plan T3-S3, OriginTaskResult (the T3-S1 OPEN item). An execution child is revoked after it ended and
// before its parent's next Start: the walk does not reach the parent, which cites the child only when
// the result is delivered. That Start delivers the result withdrawn - the task, the child and its
// status, nothing the child produced - stores no edge citing the child, and the parent's run completes.
func TestARevokedTaskIsDeliveredWithdrawn(t *testing.T) {
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
		t.Fatalf("proposal = %+v", tasks)
	}
	started, err := supervisor.StartProposedTask(ctx, parent, tasks[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	child := started.SessionID
	waitInactive(t, supervisor, child)
	rep, err := engine.RevokeSource(ctx, SourceSession, child, "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range rep.Sessions {
		if id == parent {
			t.Fatalf("the revocation reached the parent: %v", rep.Sessions)
		}
	}

	seen := len(fake.seenTaskResult)
	startAndWait(t, supervisor, parent, "", DelegationPropose)
	if info, cause := sessionState(t, engine, parent); info.Status != "completed" {
		t.Fatalf("parent run %s, cause %+v", info.Status, cause)
	}
	rows := taskResultRows(t, engine, parent)
	if len(rows) != 1 {
		t.Fatalf("task_result rows %+v", rows)
	}
	got := rows[0]
	if !got.Withdrawn || got.SessionID != child || got.TaskID != tasks[0].ID || got.Status == "" ||
		got.Final != "" || got.Objective != "" || got.Error != "" || len(got.OpenUnits) != 0 || len(got.Checks) != 0 || got.Handoff != nil {
		t.Fatalf("withdrawn task_result %+v", got)
	}
	if len(fake.seenTaskResult) == seen {
		t.Fatal("the parent model never read the task_result row")
	}
	for _, c := range fake.seenTaskResult[seen:] {
		if strings.Contains(c, "child done") {
			t.Fatalf("the parent's request carries the child's final: %q", c)
		}
	}
	if n := countRows(t, engine, `SELECT count(*) FROM derived_origins WHERE session_id = ? AND source_kind = 'session' AND source_ref = ?`, parent, child); n != 0 {
		t.Fatalf("the parent stored %d edges citing the withdrawn child", n)
	}
	// Delivered once: the next Start delivers nothing more.
	startAndWait(t, supervisor, parent, "", DelegationPropose)
	if rows := taskResultRows(t, engine, parent); len(rows) != 1 {
		t.Fatalf("task_result rows after another Start: %d", len(rows))
	}
}
