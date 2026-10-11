package migrate

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

//go:embed testdata/*.sql
var testdata embed.FS

// sentinel is the hidden-reasoning text planted in every fixture. It must not survive anywhere on
// disk once a database is migrated: not in the live file, the WAL or a durable backup.
const sentinel = "HIDDEN-REASONING-SENTINEL"

// longReasoning spans several pages so that overflow pages carry the sentinel too.
var longReasoning = strings.Repeat("pad-", 6000) + sentinel + "-tail"

// coreTables are the tables core owns; every other table is a foreign Node table the runner must
// leave alone.
var coreTables = map[string]bool{
	"metadata": true, "sessions": true, "session_messages": true, "agent_steps": true, "journal": true,
	"sqlite_sequence": true, "session_relations": true, "session_checkpoints": true,
}

type fixtureKind string

const (
	fixtureGo      fixtureKind = "go"
	fixtureDesktop fixtureKind = "desktop"
	fixtureNode    fixtureKind = "node"
)

var allFixtures = []fixtureKind{fixtureGo, fixtureDesktop, fixtureNode}

func schemaFile(kind fixtureKind) string {
	return map[fixtureKind]string{fixtureGo: "go_schema.sql", fixtureDesktop: "desktop_schema.sql", fixtureNode: "node_schema.sql"}[kind]
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("%s: %v", firstLine(query), err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// newFixture creates a pre-runner database of the given kind in a fresh state directory: its real
// legacy schema (user_version 0, WAL, a schema_version row), sessions with transcripts, and rows that
// store provider reasoning under both key names. It returns the state directory.
func newFixture(t *testing.T, kind fixtureKind) string { return buildFixture(t, kind, false) }

// newFixtureWithWAL is newFixture, but the returned directory is a copy taken while the writer was
// still open: the main file is the empty legacy schema and every row, reasoning included, sits only in
// lumencortex.db-wal, as after a crash of the previous owner.
func newFixtureWithWAL(t *testing.T, kind fixtureKind) string { return buildFixture(t, kind, true) }

func buildFixture(t *testing.T, kind fixtureKind, copyWAL bool) string {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(dir, dbFile))+"?_journal_mode=WAL&_foreign_keys=on&_synchronous=NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	ddl, err := testdata.ReadFile("testdata/" + schemaFile(kind))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, string(ddl))

	now := "2026-10-11T00:00:00Z"
	session := func(id, status, goal string) {
		mustExec(t, db, `INSERT INTO sessions(id, created_at, updated_at, status, provider, model, goal, metadata_json, final, usage_json, error_json)
VALUES(?, ?, ?, ?, 'p', 'm', ?, '{"ui":{"title":"keep"}}', NULL, NULL, NULL)`, id, now, now, status, goal)
	}
	message := func(id string, seq int, role string, payload map[string]any) {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		mustExec(t, db, `INSERT INTO session_messages(session_id, seq, role, json) VALUES(?, ?, ?, ?)`, id, seq, role, string(raw))
	}

	session("s_done", "completed", "read the readme")
	message("s_done", 0, "user", map[string]any{"role": "user", "content": "read the readme"})
	toolCall := map[string]any{"id": "c1", "name": "read_file", "arguments": map[string]any{"path": "README.md"}}
	if kind == fixtureNode {
		// Node stores calls in the OpenAI wire shape; 0001 leaves them alone (a later migration normalizes).
		toolCall = map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"README.md"}`}}
	}
	message("s_done", 1, "assistant", map[string]any{"role": "assistant", "content": "", "tool_calls": []any{toolCall}, "reasoning": "short " + sentinel})
	message("s_done", 2, "tool", map[string]any{"role": "tool", "tool_call_id": "c1", "name": "read_file", "content": "# readme"})
	message("s_done", 3, "assistant", map[string]any{"role": "assistant", "content": "done", "reasoning_content": longReasoning})
	mustExec(t, db, `INSERT INTO agent_steps(session_id, step, json) VALUES('s_done', 1, '{"step":1}')`)
	mustExec(t, db, `INSERT INTO journal(at, event, payload_json) VALUES(?, 'session.create', '{}')`, now)

	session("s_running", "running", "still going")
	message("s_running", 0, "user", map[string]any{"role": "user", "content": "still going"})
	message("s_running", 1, "assistant", map[string]any{"role": "assistant", "content": "working", "reasoning": sentinel, "reasoning_content": sentinel})

	session("s_created", "created", "never ran")

	if kind == fixtureNode {
		mustExec(t, db, `INSERT INTO graph_nodes(id, kind, status, title, body, path, source_kind, updated_at, version, json)
VALUES('n1', 'file', 'active', 'README', 'body', 'README.md', 'ingest', ?, 1, '{}')`, now)
		mustExec(t, db, `INSERT INTO graph_nodes(id, kind, status, title, body, path, source_kind, updated_at, version, json)
VALUES('n2', 'symbol', 'active', 'main', 'func main', 'main.go', 'ingest', ?, 3, '{"k":"v"}')`, now)
		mustExec(t, db, `INSERT INTO graph_edges(id, from_id, to_id, type, weight, json) VALUES('e1', 'n1', 'n2', 'contains', 0.5, '{}')`)
		mustExec(t, db, `INSERT INTO repository_state(key, value) VALUES('head', 'abc123')`)
		mustExec(t, db, `INSERT INTO cognitive_commits(id, format_version, message, parents_json, created_at, graph_hash, diff_json, metadata_json)
VALUES('c0', 1, 'init', '[]', ?, 'h', '{}', '{}')`, now)
	}
	if copyWAL {
		out := t.TempDir()
		for _, name := range []string{dbFile, dbFile + "-wal"} {
			raw, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, name), raw, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	return dir
}

// withoutReasoning is the expected transcript JSON after 0001: the same document minus the two keys.
func withoutReasoning(t *testing.T, raw string) any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("unmarshal %s: %v", raw, err)
	}
	delete(doc, "reasoning")
	delete(doc, "reasoning_content")
	return doc
}

// snapshot is everything a test compares before and after a migration.
type snapshot struct {
	sessions     []string
	messages     map[string]any // "session/seq" -> JSON without reasoning keys
	steps        []string
	journal      []string
	foreignRows  map[string]int // Node-only table -> row count
	foreignDDL   map[string]string
	foreignTexts []string
}

func take(t *testing.T, db *sql.DB) snapshot {
	t.Helper()
	s := snapshot{messages: map[string]any{}, foreignRows: map[string]int{}, foreignDDL: map[string]string{}}
	s.sessions = qStrings(t, db, `SELECT id || '|' || status || '|' || goal || '|' || metadata_json FROM sessions ORDER BY id`)
	rows, err := db.Query(`SELECT session_id, seq, json FROM session_messages ORDER BY session_id, seq`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, raw string
		var seq int
		if err := rows.Scan(&id, &seq, &raw); err != nil {
			t.Fatal(err)
		}
		s.messages[fmt.Sprintf("%s/%d", id, seq)] = withoutReasoning(t, raw)
	}
	rows.Close()
	s.steps = qStrings(t, db, `SELECT session_id || '|' || step || '|' || json FROM agent_steps ORDER BY 1`)
	s.journal = qStrings(t, db, `SELECT event || '|' || payload_json FROM journal ORDER BY seq`)
	names := qStrings(t, db, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	for _, name := range names {
		if coreTables[name] {
			continue
		}
		s.foreignRows[name] = qInt(t, db, `SELECT count(*) FROM "`+name+`"`)
		s.foreignDDL[name] = qStrings(t, db, `SELECT sql FROM sqlite_master WHERE name = ?`, name)[0]
		s.foreignTexts = append(s.foreignTexts, foreignContent(t, db, name))
	}
	return s
}

// foreignContent renders every row of a table the migration must leave alone, whatever its columns.
func foreignContent(t *testing.T, db *sql.DB, table string) string {
	t.Helper()
	cols := qStrings(t, db, `SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = `quote("` + c + `")`
	}
	rows := qStrings(t, db, `SELECT `+strings.Join(parts, `||'|'||`)+` FROM "`+table+`" ORDER BY 1`)
	return table + ":" + strings.Join(rows, "\n")
}

func (s snapshot) equal(o snapshot) bool { return reflect.DeepEqual(s, o) }

func qInt(t *testing.T, q interface {
	QueryRow(string, ...any) *sql.Row
}, query string, args ...any) int {
	t.Helper()
	var n int
	if err := q.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", firstLine(query), err)
	}
	return n
}

func qStrings(t *testing.T, db *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("%s: %v", firstLine(query), err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s.String)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// readOnlyDB opens a file for inspection without changing its journal mode or schema.
func inspect(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func backups(t *testing.T, stateDir string) []string {
	return listDir(t, filepath.Join(stateDir, "backups"))
}

// fileHashes fingerprints every regular file in dir (the database, its WAL and shm, markers, locks'
// content excluded by name), so a test can prove nothing was written.
func fileHashes(t *testing.T, dir string, names ...string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, n := range names {
		raw, err := os.ReadFile(filepath.Join(dir, n))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		out[n] = sha256.Sum256(raw)
	}
	return out
}

// filesWithSentinel lists the files of the state directory (database, WAL, backups) whose bytes hold
// the reasoning sentinel.
func filesWithSentinel(t *testing.T, stateDir string) []string {
	t.Helper()
	var hits []string
	candidates := []string{dbFile, dbFile + "-wal"}
	for _, b := range backups(t, stateDir) {
		candidates = append(candidates, filepath.Join("backups", b))
	}
	for _, rel := range candidates {
		raw, err := os.ReadFile(filepath.Join(stateDir, rel))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(sentinel)) {
			hits = append(hits, rel)
		}
	}
	return hits
}

func embeddedSet(t *testing.T) *Set {
	t.Helper()
	set, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// syntheticSet is 0001 plus extra migrations, for tests of the runner itself.
func syntheticSet(t *testing.T, up map[string]string, down map[string]string) *Set {
	t.Helper()
	real, err := testdataEmbedded("0001_v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"0001_v1.sql": {Data: real}}
	for name, body := range up {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	for name, body := range down {
		fsys["down/"+name] = &fstest.MapFile{Data: []byte(body)}
	}
	set, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func testdataEmbedded(name string) ([]byte, error) { return embedded.ReadFile("sql/" + name) }

// shorten makes lock waits fail fast in tests that expect ErrDatabaseInUse; a non-zero checkpoint
// shortens the wait for a reader that blocks the post-change checkpoint.
func shorten(t *testing.T, wait, checkpoint time.Duration) {
	t.Helper()
	oldWait, oldCheckpoint := LockWait, CheckpointWait
	LockWait = wait
	if checkpoint > 0 {
		CheckpointWait = checkpoint
	}
	t.Cleanup(func() { LockWait, CheckpointWait = oldWait, oldCheckpoint })
}
