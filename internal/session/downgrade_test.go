package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/migrate"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// TestDowngradeRoundTrip: upgrade → progress → downgrade → core reads legacy metadata → upgrade → still valid
func TestDowngradeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := os.Create(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	statedir := filepath.Join(dir, ".lumencortex")
	if err := os.MkdirAll(statedir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Start from user_version=2
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
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
	if _, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at, status, provider, model, goal, metadata_json) VALUES ('s1', ?, ?, 'running', 'p', 'm', 'goal', '{}')`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Open → upgrades to user_version=3
	store, err := Open(statedir)
	if err != nil {
		t.Fatal(err)
	}

	// Record progress: work unit completed, sameFailureCount=2
	ts, err := store.LatestTaskState(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	var state taskstate.State
	if err := json.Unmarshal(ts.JSON, &state); err != nil {
		t.Fatal(err)
	}
	if state.WorkUnits == nil {
		state.WorkUnits = &taskstate.WorkUnitState{Version: 1, Items: map[string]taskstate.WorkUnit{}, Order: []string{}}
	}
	state.WorkUnits.Items["wu_tests"] = taskstate.WorkUnit{
		ID:     "wu_tests",
		Status: "completed",
	}
	state.WorkUnits.Order = append(state.WorkUnits.Order, "wu_tests")
	state.Progress.SameFailureCount = 2
	next, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutTaskState(context.Background(), "s1", TaskStateWrite{
		Base:   ts.Version,
		Reason: "test_progress",
		JSON:   next,
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Downgrade to version 2
	db, err = sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	set, err := migrate.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrate.Downgrade(context.Background(), statedir, set, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Check that legacy metadata_json preserved progress
	db, err = sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var mdJSON string
	if err := db.QueryRow(`SELECT metadata_json FROM sessions WHERE id = 's1'`).Scan(&mdJSON); err != nil {
		t.Fatal(err)
	}
	var md map[string]any
	if err := json.Unmarshal([]byte(mdJSON), &md); err != nil {
		t.Fatal(err)
	}
	if wu, ok := md["workUnits"].(map[string]any); !ok || wu["items"] == nil {
		t.Fatalf("workUnits not preserved: %v", md)
	}
	cognition, ok := md["cognition"].(map[string]any)
	if !ok {
		t.Fatalf("cognition not preserved: %v", md)
	}
	progress, ok := cognition["progress"].(map[string]any)
	if !ok {
		t.Fatalf("cognition.progress not preserved: %v", cognition)
	}
	if fc, ok := progress["sameFailureCount"].(float64); !ok || fc != 2 {
		t.Fatalf("sameFailureCount = %v, want 2", progress)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Upgrade again
	store, err = Open(statedir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Check that Task State is valid and progress is retained
	ts2, err := store.LatestTaskState(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	var state2 taskstate.State
	if err := json.Unmarshal(ts2.JSON, &state2); err != nil {
		t.Fatal(err)
	}
	if state2.WorkUnits == nil || len(state2.WorkUnits.Items) == 0 {
		t.Errorf("work unit not preserved after re-upgrade: %+v", state2.WorkUnits)
	} else if wu, ok := state2.WorkUnits.Items["wu_tests"]; !ok || wu.Status != "completed" {
		t.Errorf("work unit wu_tests not correct: %+v", wu)
	}
	if state2.Progress.SameFailureCount != 2 {
		t.Errorf("sameFailureCount = %d, want 2", state2.Progress.SameFailureCount)
	}
}

// TestDowngradeEveryBoundary: 4→3→2→1→2→3→4, check tables and integrity at each step
func TestDowngradeEveryBoundary(t *testing.T) {
	dir := t.TempDir()
	if _, err := os.Create(filepath.Join(dir, ".git")); err != nil {
		t.Fatal(err)
	}
	statedir := filepath.Join(dir, ".lumencortex")
	if err := os.MkdirAll(statedir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Start from user_version=0 (empty database; 0001 will create metadata and the schema)
	// Don't create any tables—let the migrations do it
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Upgrade to latest (5)
	store, err := Open(statedir)
	if err != nil {
		t.Fatal(err)
	}
	// Create a session with progress for downgrade tests
	if err := store.Create(context.Background(), Session{
		ID:       "s1",
		Goal:     "goal",
		Status:   "running",
		Provider: "p",
		Model:    "m",
		AgentID:  "explore",
		Metadata: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendMessage(context.Background(), "s1", "user", protocol.Message{Role: "user", Content: "goal"}); err != nil {
		t.Fatal(err)
	}
	// Build Task State version 1 directly (Start doesn't exist in package session)
	state1 := taskstate.State{
		Progress: taskstate.Progress{SameFailureCount: 1},
	}
	state1JSON, err := json.Marshal(state1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := store.db.Exec(`INSERT INTO task_states (session_id, version, created_at, reason, json) VALUES (?, 1, ?, 'test', ?)`, "s1", now, string(state1JSON)); err != nil {
		t.Fatal(err)
	}
	// An evaluator result and its artifact (0004): the down migration drops both tables and nothing else.
	if _, err := store.db.Exec(`INSERT INTO artifacts (session_id, hash, kind, size, created_at, data) VALUES ('s1', 'sha256:' || printf('%064d', 0), 'log', 2, ?, X'6f6b')`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO eval_results (session_id, seq, check_id, kind, status, provenance, fingerprint, artifacts_json, at) VALUES ('s1', 1, 'go-test', 'test', 'passed', 'runtime', 'h+@local#00', '[]', ?)`, now); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	checkVersion := func(want int) {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var v int
		if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v != want {
			t.Fatalf("user_version = %d, want %d", v, want)
		}
		if _, err := db.Exec(`PRAGMA integrity_check`); err != nil {
			t.Fatal(err)
		}
	}

	checkVersion(5)
	hasTable := func(name string) bool {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	hasColumn := func(table, column string) bool {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n == 1
	}

	// Downgrade 5→4: the session agent column goes, the session row and everything else stays.
	set4, err := migrate.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrate.Downgrade(context.Background(), statedir, set4, 4); err != nil {
		t.Fatal(err)
	}
	checkVersion(4)
	if hasColumn("sessions", "agent_id") || !hasColumn("sessions", "model_ref") || !hasTable("eval_results") {
		t.Fatalf("after 5→4: agent_id=%v model_ref=%v eval_results=%v", hasColumn("sessions", "agent_id"), hasColumn("sessions", "model_ref"), hasTable("eval_results"))
	}

	// Downgrade 4→3: evaluator tables go, Task State stays.
	if _, _, err := migrate.Downgrade(context.Background(), statedir, set4, 3); err != nil {
		t.Fatal(err)
	}
	checkVersion(3)
	if hasTable("eval_results") || hasTable("artifacts") || !hasTable("task_states") {
		t.Fatalf("after 4→3: eval_results=%v artifacts=%v task_states=%v", hasTable("eval_results"), hasTable("artifacts"), hasTable("task_states"))
	}

	// Downgrade 3→2

	db, err = sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	set, err := migrate.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrate.Downgrade(context.Background(), statedir, set, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	checkVersion(2)

	// Downgrade 2→1
	db, err = sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrate.Downgrade(context.Background(), statedir, set, 1); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	checkVersion(1)

	// Check that metadata_json preserved progress
	db, err = sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	var mdJSON string
	if err := db.QueryRow(`SELECT metadata_json FROM sessions WHERE id = 's1'`).Scan(&mdJSON); err != nil {
		t.Fatal(err)
	}
	var md map[string]any
	if err := json.Unmarshal([]byte(mdJSON), &md); err != nil {
		t.Fatal(err)
	}
	cognition, ok := md["cognition"].(map[string]any)
	if !ok {
		t.Fatalf("cognition missing or not a map: %v", md)
	}
	progress, ok := cognition["progress"].(map[string]any)
	if !ok {
		t.Fatalf("cognition.progress missing or not a map: %v", cognition)
	}
	if fc, ok := progress["sameFailureCount"].(float64); !ok || fc != 1 {
		t.Fatalf("cognition.progress.sameFailureCount = %v, want 1", progress)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Upgrade 1→2→3→4→5
	store, err = Open(statedir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	checkVersion(5)
	if !hasTable("eval_results") || !hasTable("artifacts") || !hasColumn("sessions", "agent_id") {
		t.Fatal("re-upgrade did not recreate the evaluator tables and the agent column")
	}
	// The re-added column starts empty: the session gets the default agent on its next Start.
	if s1, err := store.Get(context.Background(), "s1"); err != nil || s1.AgentID != "" {
		t.Fatalf("s1 after re-upgrade: %+v, %v", s1, err)
	}

	// Check that progress is still valid
	ts, err := store.LatestTaskState(context.Background(), "s1")
	if err != nil {
		t.Fatal(err)
	}
	var state taskstate.State
	if err := json.Unmarshal(ts.JSON, &state); err != nil {
		t.Fatal(err)
	}
	if state.Progress.SameFailureCount != 1 {
		t.Errorf("sameFailureCount = %d, want 1", state.Progress.SameFailureCount)
	}

	// Check that --to 0 writes nothing
	db2, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(statedir, "lumencortex.db"))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	set, err = migrate.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrate.Downgrade(context.Background(), statedir, set, 0); err == nil {
		t.Fatal("downgrade to 0 should reject")
	}
}
