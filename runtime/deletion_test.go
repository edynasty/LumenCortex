package runtime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/runlock"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// privacySentinel is the content a deleted session holds; no byte of the database or its WAL may hold
// it after the deletion.
const privacySentinel = "PRIVACY-SENTINEL-7d3e"

// deletionProvider answers a subagent child with a final holding the sentinel, and its parent with a
// wait_subagent of the child, then a text answer.
type deletionProvider struct {
	mu    sync.Mutex
	child string
}

func (p *deletionProvider) Model() string { return "deletion" }

func (p *deletionProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	for _, m := range req.Messages {
		if m.Role == "user" && m.Content == "child task" {
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child final " + privacySentinel}, FinishReason: "stop"}, nil
		}
	}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "parent answer"}, FinishReason: "stop"}, nil
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return toolCalls(tc("w1", "wait_subagent", `{"child_session_id":"`+p.child+`","timeout_seconds":1}`)), nil
}

// deletionFixture is a parent P with a subagent child C whose final holds the sentinel, a reader R
// that read C with read_thread, and C's own temporary child C2. The sentinel is then placed, as rows
// written before this change could hold it, in every table that stores C's content, in a checkpoint
// and a task result of P, and in journal rows with and without a session id.
type deletionFixture struct {
	engine     *Engine
	supervisor *RunSupervisor
	parent     string
	child      string
	grandchild string
	reader     string
	workspace  string
}

func newDeletionFixture(t *testing.T) deletionFixture {
	t.Helper()
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace, Budget: Budget{MaxAgents: 4}})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		engine.Close()
		t.Fatal(err)
	}
	ctx := context.Background()
	provider := &deletionProvider{}
	parent := newParentSession(t, engine, provider)
	value, err := (&subagentController{supervisor: supervisor, parentID: parent}).Spawn(ctx, "child task", "")
	if err != nil {
		t.Fatal(err)
	}
	child := value.(SubagentNode).SessionID
	waitInactive(t, supervisor, child)
	provider.mu.Lock()
	provider.child = child
	provider.mu.Unlock()
	if _, err := supervisor.Start(ctx, parent, AgentOptions{Input: "check on the child", MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, supervisor, parent)

	reader := newTestSession(t, engine, "a reader")
	read := &threadProvider{responses: []protocol.ProviderResponse{
		toolCalls(tc("r1", "read_thread", `{"id":"`+child+`"}`)),
		{Message: protocol.Message{Role: "assistant", Content: "read"}, FinishReason: "stop"},
	}}
	if r, err := engine.runAgentWith(ctx, reader.ID, read, AgentOptions{Input: "read", MaxSteps: 3}); err != nil || r.Status != "completed" {
		t.Fatalf("reader run %+v %v", r, err)
	}

	grandchild := "session_" + strings.Repeat("c2", 12)
	now := time.Now().UTC()
	if err := engine.store.CreateWithMessage(ctx, session.Session{ID: grandchild, CreatedAt: now, UpdatedAt: now, Status: "completed",
		Goal: "grandchild " + privacySentinel, Metadata: json.RawMessage("{}")}, protocol.Message{Role: "user", Content: "grandchild " + privacySentinel}); err != nil {
		t.Fatal(err)
	}
	if err := engine.store.AddRelation(ctx, session.Relation{ParentSessionID: child, ChildSessionID: grandchild, Kind: RelationSubagent, CreatedAt: now, Metadata: json.RawMessage("{}")}); err != nil {
		t.Fatal(err)
	}

	newest, err := engine.store.NewestMessageSeq(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := json.Marshal(map[string]any{"sessionId": child, "status": "completed", "final": privacySentinel})
	taskResult, _ := json.Marshal(protocol.Message{Role: "user", Origin: protocol.OriginTaskResult, Content: protocol.TaskResultHeader + "\n" + string(result)})
	at := now.Format(time.RFC3339Nano)
	hash := "sha256:" + strings.Repeat("ab", 32)
	seedSQL(t, engine,
		[]any{`INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, 'user', ?)`, parent, newest + 1, string(taskResult)},
		[]any{`INSERT INTO session_checkpoints(session_id, seq, at, reason, json) VALUES(?, 900, ?, 'subagent.stopped', ?)`, parent, at,
			`{"childSessionId":"` + child + `","status":"completed","goal":"child task","final":"` + privacySentinel + `"}`},
		[]any{`INSERT INTO session_checkpoints(session_id, seq, at, reason, json) VALUES(?, 900, ?, 'legacy', ?)`, child, at, `{"note":"` + privacySentinel + `"}`},
		[]any{`INSERT INTO agent_steps(session_id, step, json) VALUES(?, 900, ?)`, child, `{"args":"` + privacySentinel + `"}`},
		[]any{`INSERT INTO task_states(session_id, version, created_at, reason, json) VALUES(?, 900, ?, 'legacy', ?)`, child, at, `{"brief":"` + privacySentinel + `"}`},
		[]any{`INSERT INTO task_state_inbox(session_id, created_at, json) VALUES(?, ?, ?)`, child, at, `{"note":"` + privacySentinel + `"}`},
		[]any{`INSERT INTO artifacts(session_id, hash, kind, size, created_at, data) VALUES(?, ?, 'log', 21, ?, ?)`, child, hash, at, []byte(privacySentinel)},
		[]any{`INSERT INTO eval_results(session_id, seq, check_id, kind, status, provenance, fingerprint, artifacts_json, at) VALUES(?, 1, 'c', 'test', 'failed', 'runtime', 'f', ?, ?)`,
			child, `["` + privacySentinel + `"]`, at},
		[]any{`UPDATE sessions SET error_json = ? WHERE id = ?`, `{"message":"` + privacySentinel + `"}`, child},
		[]any{`INSERT INTO journal(at, event, payload_json) VALUES(?, 'tool.shell', ?)`, at, `{"command":"echo ` + privacySentinel + `"}`},
		[]any{`INSERT INTO journal(at, event, payload_json) VALUES(?, 'tool.start', ?)`, at, `{"sessionId":"` + child + `","args":{"x":"` + privacySentinel + `"}}`},
		[]any{`INSERT INTO journal(at, event, payload_json) VALUES(?, 'subagent.event', ?)`, at, `{"sessionId":"` + parent + `","data":{"text":"` + privacySentinel + `"}}`},
	)
	return deletionFixture{engine: engine, supervisor: supervisor, parent: parent, child: child, grandchild: grandchild, reader: reader.ID, workspace: workspace}
}

func seedSQL(t *testing.T, engine *Engine, statements ...[]any) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, s := range statements {
		if _, err := db.Exec(s[0].(string), s[1:]...); err != nil {
			t.Fatalf("%s: %v", s[0], err)
		}
	}
}

// sentinelAtRest reports the files of the database (main file and WAL) whose bytes hold the sentinel.
func sentinelAtRest(t *testing.T, dbPath string) []string {
	t.Helper()
	var found []string
	for _, path := range []string{dbPath, dbPath + "-wal"} {
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(privacySentinel)) {
			found = append(found, path)
		}
	}
	return found
}

// Plan T3-S4 through the real Engine and SQLite: the preview counts what the deletion removes, the
// deletion removes every copy of the content from the database and its WAL bytes, keeps the other
// sessions with their row counts, their edges (now citing a missing source) and their fences, and
// the deleted id stays withdrawn as a source.
func TestDeletingASessionRemovesEveryCopyOfItsContent(t *testing.T) {
	f := newDeletionFixture(t)
	defer f.engine.Close()
	defer f.supervisor.Close()
	ctx := context.Background()
	path := f.engine.store.Path()
	if len(sentinelAtRest(t, path)) == 0 {
		t.Fatal("fixture: the sentinel is not in the database")
	}
	parentRows := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ?`, f.parent)
	readerRows := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ?`, f.reader)
	readerEdges := countRows(t, f.engine, `SELECT count(*) FROM derived_origins WHERE session_id = ? AND source_ref = ?`, f.reader, f.child)
	if readerEdges == 0 {
		t.Fatal("fixture: the reader has no edge citing the child")
	}

	p, err := f.engine.DeletionPreview(ctx, f.child)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.Deleted, []string{f.child, f.grandchild}) || len(p.Dependents) != 0 {
		t.Fatalf("preview deleted %v dependents %v", p.Deleted, p.Dependents)
	}
	if !slices.Contains(p.Readers, f.parent) || !slices.Contains(p.Readers, f.reader) || p.Copies != 3 || p.ParentCheckpoints != 3 ||
		p.Rows["agent_steps"] == 0 || p.Rows["artifacts"] != 1 || p.Rows["sessions"] != 2 || p.JournalRows < 3 {
		t.Fatalf("preview %+v", p.DeletionPreview)
	}
	kinds := map[string]bool{}
	for _, o := range p.Outside {
		kinds[o.Kind] = true
	}
	if !kinds["workspace"] || !kinds["provider"] {
		t.Fatalf("preview outside %+v", p.Outside)
	}

	res, err := f.engine.DeleteSession(ctx, f.child, WorktreeCleanup{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.DeletedChildren, []string{f.grandchild}) {
		t.Fatalf("deleted children %v", res.DeletedChildren)
	}
	if found := sentinelAtRest(t, path); len(found) != 0 {
		t.Fatalf("the sentinel is still in %v", found)
	}
	// Writes after the truncation (the delete's own journal row) may refill the WAL; the marker that
	// owed the truncation is gone.
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "checkpoint.pending")); !os.IsNotExist(err) {
		t.Fatalf("checkpoint marker left: %v", err)
	}

	for _, id := range []string{f.child, f.grandchild} {
		if _, err := f.engine.store.Get(ctx, id); !errors.Is(err, session.ErrNotFound) {
			t.Fatalf("%s still exists: %v", id, err)
		}
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ?`, f.parent); n != parentRows {
		t.Fatalf("parent rows %d -> %d", parentRows, n)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ?`, f.reader); n != readerRows {
		t.Fatalf("reader rows %d -> %d", readerRows, n)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ? AND json_extract(json, '$.content') = ?`, f.parent, session.RemovedContent); n != 1 {
		t.Fatalf("parent tool rows rewritten: %d, want the wait_subagent row", n)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_messages WHERE session_id = ? AND json_extract(json, '$.origin') = 'task_result'
		AND json_extract(json, '$.content') = ?`, f.parent, protocol.TaskResultHeader+"\n"+`{"sessionId":"`+f.child+`","withdrawn":true}`); n != 1 {
		t.Fatalf("parent task result not withdrawn: %d", n)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM derived_origins WHERE session_id = ? AND source_ref = ?`, f.reader, f.child); n != readerEdges {
		t.Fatalf("reader edges citing the deleted child %d -> %d", readerEdges, n)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM session_checkpoints WHERE session_id = ? AND seq = 900
		AND json = ?`, f.parent, `{"childSessionId":"`+f.child+`","status":"completed"}`); n != 1 {
		t.Fatalf("parent checkpoint not reduced to the whitelist")
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM source_fences WHERE scope_kind = 'privacy' AND state = 'cleaned' AND scope_ref IN (?, ?)`, f.child, f.grandchild); n != 2 {
		t.Fatalf("privacy fences cleaned: %d", n)
	}
	if n := countRows(t, f.engine, `SELECT count(*) FROM source_fences WHERE scope_kind = 'session' AND state = 'active' AND scope_ref IN (?, ?)`, f.parent, f.reader); n != 2 {
		t.Fatalf("readers' fences active: %d", n)
	}
	// The deleted id stays withdrawn as a source: an edge committed now that cites it is refused.
	if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, f.child); err != nil || !withdrawn {
		t.Fatalf("deleted child withdrawn %v %v", withdrawn, err)
	}
	if _, err := f.engine.DeleteSession(ctx, f.child, WorktreeCleanup{}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("deleting again: %v", err)
	}
	// Nothing was left to resume, and a reopen finds the same database.
	f.supervisor.Close()
	f.engine.Close()
	if found := sentinelAtRest(t, path); len(found) != 0 {
		t.Fatalf("the sentinel is in %v after close", found)
	}
}

// A deletion whose cleanup is interrupted after its fences (here: the caller's ctx is cancelled after
// the first batch, or once the session rows are gone and only the rebuild is owed) is
// cleanup_pending: the session cannot run, a DeleteSession while another caller holds one of its
// sessions fails and keeps it pending, and the next DeleteSession resumes and finishes it.
func TestAnInterruptedDeletionIsResumedByTheNextDelete(t *testing.T) {
	for _, point := range []string{"rows", "rebuild"} {
		t.Run(point, func(t *testing.T) {
			f := newDeletionFixture(t)
			defer f.engine.Close()
			defer f.supervisor.Close()
			bg := context.Background()
			ctx, cancel := context.WithCancel(bg)
			testHookPurgeStep = func() {
				_, err := f.engine.store.Get(bg, f.child)
				if point == "rows" || errors.Is(err, session.ErrNotFound) {
					cancel()
				}
			}
			defer func() { testHookPurgeStep = nil }()
			if _, err := f.engine.DeleteSession(ctx, f.child, WorktreeCleanup{}); !errors.Is(err, context.Canceled) {
				t.Fatalf("interrupted delete: %v", err)
			}
			testHookPurgeStep = nil
			if pending, err := f.engine.store.PendingPrivacyDeletion(bg, f.child); err != nil || pending == nil {
				t.Fatalf("pending deletion %v %v", pending, err)
			}
			if point == "rows" {
				if _, err := f.supervisor.Start(bg, f.child, AgentOptions{Input: "again", MaxSteps: 1}); !errors.Is(err, session.ErrFenced) {
					t.Fatalf("Start of a session being deleted: %v", err)
				}
				held, err := runlock.TryAcquire(f.engine.runDir(), f.grandchild)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.engine.DeleteSession(bg, f.child, WorktreeCleanup{}); !errors.Is(err, ErrRunHeld) {
					t.Fatalf("delete while a deleted session is held: %v", err)
				}
				held.Release()
				if pending, err := f.engine.store.PendingPrivacyDeletion(bg, f.child); err != nil || pending == nil {
					t.Fatalf("a refused resume ended the deletion: %v %v", pending, err)
				}
			}
			res, err := f.engine.DeleteSession(bg, f.child, WorktreeCleanup{})
			if err != nil || !slices.Equal(res.DeletedChildren, []string{f.grandchild}) {
				t.Fatalf("resumed delete %+v %v", res, err)
			}
			if found := sentinelAtRest(t, f.engine.store.Path()); len(found) != 0 {
				t.Fatalf("the sentinel is still in %v", found)
			}
			if pending, err := f.engine.store.PendingPrivacyDeletion(bg, f.child); err != nil || pending != nil {
				t.Fatalf("deletion still pending: %v %v", pending, err)
			}
		})
	}
}
