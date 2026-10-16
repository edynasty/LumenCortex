package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
	_ "github.com/mattn/go-sqlite3"
)

// migrationStore opens a fresh Store on a minimal pre-M3.1 Go database that is then upgraded.
func migrationStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if _, err := os.Create(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	statedir := filepath.Join(dir, ".lumencortex")
	if err := os.MkdirAll(statedir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Build a pre-migration database
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ddl := `-- Minimal Go pre-M3.1 schema (version 2: model_ref)
CREATE TABLE metadata (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
) STRICT;
CREATE TABLE sessions(
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  status TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  model_ref TEXT,
  goal TEXT NOT NULL,
  metadata_json TEXT NOT NULL,
  final INTEGER,
  usage_json TEXT,
  error_json TEXT
) STRICT;
CREATE TABLE session_messages(
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json)),
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE TABLE agent_steps(
  session_id TEXT NOT NULL,
  step INTEGER NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json)),
  PRIMARY KEY (session_id, step),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE TABLE journal(
  seq INTEGER PRIMARY KEY,
  at TEXT NOT NULL,
  event TEXT NOT NULL,
  payload_json TEXT NOT NULL CHECK (json_valid(payload_json))
) STRICT;
PRAGMA user_version = 2;`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Open upgrades to latest
	store, err := Open(statedir)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func migrationSession(t *testing.T, store *Store, id, status, errorJSON string) {
	t.Helper()
	v := Session{ID: id, Goal: "goal " + id, Status: status, Provider: "p", Model: "m"}
	if errorJSON != "" {
		v.Error = json.RawMessage(errorJSON)
	}
	if err := store.Create(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func migrationRow(t *testing.T, store *Store, id, role string, m protocol.Message) {
	t.Helper()
	m.Role = role
	if _, err := store.AppendMessage(context.Background(), id, role, m); err != nil {
		t.Fatal(err)
	}
}

func qInt(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("qInt %s: %v", query, err)
	}
	return n
}

// TestWindowStartSeqGapRepair checks the 0003 repair: windowStartSeq moves past the last non-tail gap
// (an assistant row with calls that are partly unanswered before the next non-tool row), while a
// session without gaps and a tail gap (closed by Reconcile at the next Start) stay 0.
func TestWindowStartSeqGapRepair(t *testing.T) {
	// Build a pre-migration database with sessions and transcript gaps
	dir := t.TempDir()
	if _, err := os.Create(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	statedir := filepath.Join(dir, ".lumencortex")
	if err := os.MkdirAll(statedir, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	// Schema at user_version=2
	ddl := `CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL) STRICT;
CREATE TABLE sessions(
  id TEXT PRIMARY KEY,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  status TEXT NOT NULL,
  provider TEXT NOT NULL,
  model TEXT NOT NULL,
  model_ref TEXT,
  goal TEXT NOT NULL,
  metadata_json TEXT NOT NULL,
  final INTEGER,
  usage_json TEXT,
  error_json TEXT
) STRICT;
CREATE TABLE session_messages(
  session_id TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json)),
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE TABLE agent_steps(
  session_id TEXT NOT NULL,
  step INTEGER NOT NULL,
  json TEXT NOT NULL CHECK (json_valid(json)),
  PRIMARY KEY (session_id, step),
  FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE TABLE journal(
  seq INTEGER PRIMARY KEY,
  at TEXT NOT NULL,
  event TEXT NOT NULL,
  payload_json TEXT NOT NULL CHECK (json_valid(payload_json))
) STRICT;
PRAGMA user_version = 2;`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Format(time.RFC3339)
	// Three sessions with different gap patterns
	gapID, noGapID, tailID := "gap", "no-gap", "tail"
	for _, id := range []string{gapID, noGapID, tailID} {
		_, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at, status, provider, model, goal, metadata_json) VALUES (?, ?, ?, 'idle', 'p', 'm', 'goal', '{}')`, id, now, now)
		if err != nil {
			t.Fatal(err)
		}
	}

	// gapID: assistant with calls at seq=0, follow-up at seq=2, no tool row for call "c1" → gap, windowStartSeq should move to 2
	// noGapID: all calls have results → no gap
	// tailID: assistant with calls at end, no stop → tail gap, stays 0
	msgs := []struct {
		sid  string
		seq  int
		role string
		js   string
	}{
		{gapID, 0, "assistant", `{"role":"assistant","tool_calls":[{"id":"c1","name":"shell","arguments":"{}"}]}`},
		{gapID, 1, "tool", `{"role":"tool","tool_call_id":"c2","content":"ok"}`}, // c2, not c1 → orphan
		{gapID, 2, "user", `{"role":"user","content":"follow-up"}`},
		{noGapID, 0, "assistant", `{"role":"assistant","tool_calls":[{"id":"c3","name":"shell","arguments":"{}"}]}`},
		{noGapID, 1, "tool", `{"role":"tool","tool_call_id":"c3","content":"ok"}`},
		{noGapID, 2, "assistant", `{"role":"assistant","content":"text"}`},
		{tailID, 0, "user", `{"role":"user","content":"goal"}`},
		{tailID, 1, "assistant", `{"role":"assistant","tool_calls":[{"id":"c9","name":"read_file","arguments":"{}"}]}`},
	}
	for _, m := range msgs {
		_, err := db.Exec(`INSERT INTO session_messages (session_id, seq, role, json) VALUES (?, ?, ?, ?)`, m.sid, m.seq, m.role, m.js)
		if err != nil {
			t.Fatal(err)
		}
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Open runs migration 0003, which builds Task State v1 and repairs gaps
	store, err := Open(statedir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	start := func(id string) int {
		t.Helper()
		row, err := store.LatestTaskState(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		var ts struct {
			WindowStartSeq int `json:"windowStartSeq"`
		}
		if err := json.Unmarshal(row.JSON, &ts); err != nil {
			t.Fatal(err)
		}
		return ts.WindowStartSeq
	}
	if got := start(gapID); got != 2 {
		t.Errorf("gap session windowStartSeq=%d, want 2 (the follow-up row)", got)
	}
	if got := start(noGapID); got != 0 {
		t.Errorf("no-gap session windowStartSeq=%d, want 0 (no repair needed)", got)
	}
	if got := start(tailID); got != 0 {
		t.Errorf("tail-gap session windowStartSeq=%d, want 0 (tail gaps stay)", got)
	}
}

// TestReconcileInterruptedSession: Reconcile closes a dangling call on a session that is already
// interrupted without touching its status or error_json (only a 'running' row changes status).
func TestReconcileInterruptedSession(t *testing.T) {
	store := migrationStore(t)
	const id = "session_interrupted_dangling"
	const wantErr = `{"kind":"previous_failure","message":"kept"}`
	migrationSession(t, store, id, "interrupted", wantErr)
	migrationRow(t, store, id, "user", protocol.Message{Content: "goal"})
	migrationRow(t, store, id, "assistant", protocol.Message{ToolCalls: []protocol.ToolCall{{ID: "d1", Name: "shell", Arguments: json.RawMessage(`{"cmd":"ls"}`)}}})

	n, err := store.Reconcile(context.Background(), id)
	if err != nil || n != 1 {
		t.Fatalf("Reconcile inserted=%d err=%v, want 1", n, err)
	}
	state, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "interrupted" || string(state.Error) != wantErr {
		t.Errorf("Reconcile changed status/error_json: status=%s error_json=%s", state.Status, state.Error)
	}
	rows, err := store.RecentMessages(context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("expected 3 rows after Reconcile, got %d", len(rows))
	}
	var m protocol.Message
	if err := json.Unmarshal(rows[2].JSON, &m); err != nil {
		t.Fatal(err)
	}
	if m.Role != "tool" || m.ToolCallID != "d1" {
		t.Errorf("dangling call not closed: %s", rows[2].JSON)
	}
	if n2, err := store.Reconcile(context.Background(), id); err != nil || n2 != 0 {
		t.Errorf("second Reconcile inserted=%d err=%v", n2, err)
	}
	t.Logf("Reconcile closed the dangling call (%s); status stayed %s and error_json %s", rows[2].JSON, state.Status, state.Error)
}

// TestGoalRowFollowsTrimSpace checks the rule 0003 uses to give a legacy session that never ran its goal
// row: the row is written exactly when strings.TrimSpace leaves the goal non-empty, which is the rule of the
// old loop's ensureInitialMessage and of NewSession from M1.2.
// TestGoalRowFollowsTrimSpace checks that migration 0003 inserts a goal row (role=user,
// content=session.Goal) only when strings.TrimSpace(goal) != "".
func TestGoalRowFollowsTrimSpace(t *testing.T) {
	dir := t.TempDir()
	if _, err := os.Create(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	statedir := filepath.Join(dir, ".lumencortex")
	if err := os.MkdirAll(statedir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Build a pre-migration database
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ddl := `-- Minimal Go pre-M3.1 schema (version 2: model_ref)
CREATE TABLE metadata (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
) STRICT;
CREATE TABLE sessions(
	id TEXT PRIMARY KEY,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	status TEXT NOT NULL,
	provider TEXT NOT NULL,
	model TEXT NOT NULL,
	model_ref TEXT,
	goal TEXT NOT NULL,
	metadata_json TEXT NOT NULL,
	final INTEGER,
	usage_json TEXT,
	error_json TEXT
) STRICT;
CREATE TABLE session_messages (
	session_id TEXT NOT NULL,
	seq INTEGER NOT NULL,
	role TEXT NOT NULL,
	json TEXT NOT NULL CHECK (json_valid(json)),
	PRIMARY KEY (session_id, seq),
	FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
) STRICT;
CREATE TABLE journal(
	seq INTEGER PRIMARY KEY,
	at TEXT NOT NULL,
	event TEXT NOT NULL,
	payload_json TEXT NOT NULL CHECK (json_valid(payload_json))
) STRICT;
PRAGMA user_version = 2;`
	if _, err := db.Exec(ddl); err != nil {
		t.Fatal(err)
	}

	goals := []struct {
		text string
		want bool
	}{
		{"goal", true},
		{" x ", true},
		{"\u3000目标\u3000", true},
		{" 	\n\r", false},
		{"\v\f", false},
		{"\u0085\u00a0", false},
		{"\u1680", false},
		{"\u2000\u200a", false},
		{"\u2028\u2029", false},
		{"\u202f\u205f", false},
		{"\u3000", false},
		{"\u200b", true},  // not white space for unicode.IsSpace
		{"\ufeff", true},  // not white space for unicode.IsSpace
	}
	for i, g := range goals {
		id := fmt.Sprintf("session_goal_%02d", i)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		_, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at, status, provider, model, goal, metadata_json)
			VALUES (?, ?, ?, 'created', 'p', 'm', ?, '{}')`, id, now, now, g.text)
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Open upgrades to latest
	store, err := Open(statedir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	for i, g := range goals {
		id := fmt.Sprintf("session_goal_%02d", i)
		got := qInt(t, store.db, `SELECT count(*) FROM session_messages
			WHERE session_id = ? AND seq = 0 AND role = 'user' AND json ->> '$.content' = ?`, id, g.text) == 1
		if got != g.want {
			t.Errorf("goal %q: goal row %v, want %v (strings.TrimSpace)", g.text, got, g.want)
		}
	}
}
