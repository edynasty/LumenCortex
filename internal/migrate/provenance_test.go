package migrate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

// v6Tables are the four tables 0006 adds; their count is frozen (execution plan §3.3, §10).
var v6Tables = []string{"request_envelopes", "derived_origins", "source_fences", "check_approvals"}

var v6EvalColumns = []string{"episode_id", "pre_fingerprint", "post_fingerprint", "spec_digest", "approval_epoch", "episode_status"}

// embeddedUpTo is the embedded migration set cut at version n, as the binary that shipped version n
// had it.
func embeddedFSUpTo(t *testing.T, n int) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	for _, dir := range []string{"sql", "sql/down"} {
		entries, err := embedded.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			var num int
			if _, err := fmt.Sscanf(e.Name(), "%04d_", &num); err != nil || num > n {
				continue
			}
			raw, err := embedded.ReadFile(dir + "/" + e.Name())
			if err != nil {
				t.Fatal(err)
			}
			fsys[strings.TrimPrefix(dir+"/"+e.Name(), "sql/")] = &fstest.MapFile{Data: raw}
		}
	}
	return fsys
}

func embeddedUpTo(t *testing.T, n int) *Set {
	t.Helper()
	set, err := Load(embeddedFSUpTo(t, n))
	if err != nil {
		t.Fatal(err)
	}
	if set.Latest() != n {
		t.Fatalf("cut set Latest=%d, want %d", set.Latest(), n)
	}
	return set
}

var spaceRun = regexp.MustCompile(`\s+`)

// normalizedSchema is every schema object with whitespace collapsed, sorted by type and name.
func normalizedSchema(t *testing.T, path string) string {
	t.Helper()
	rows := qStrings(t, inspect(t, path), `SELECT type || ' ' || name || ': ' || coalesce(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	for i, r := range rows {
		rows[i] = strings.TrimSpace(spaceRun.ReplaceAllString(r, " "))
	}
	return strings.Join(rows, "\n") + "\n"
}

// TestV6SchemaSnapshot pins the DDL of version 6. A later slice that changes it must edit the golden
// file in the same commit, which the change-request rule (plan §10) makes visible. UPDATE_GOLDEN=1
// rewrites it.
func TestV6SchemaSnapshot(t *testing.T) {
	dir := t.TempDir()
	closeWS(t, open(t, dir, embeddedUpTo(t, 6)))
	got := normalizedSchema(t, filepath.Join(dir, dbFile))
	golden := filepath.Join("testdata", "schema_v6.golden.txt")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("version 6 schema changed; review the diff and rerun with UPDATE_GOLDEN=1:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestV6TablesAndForeignKeyDirection checks the frozen shape: exactly four new tables, consumer-owned
// tables cascade from sessions only, and the fence and approval tables have no foreign key.
func TestV6TablesAndForeignKeyDirection(t *testing.T) {
	dir := t.TempDir()
	closeWS(t, open(t, dir, embeddedUpTo(t, 6)))
	db := inspect(t, filepath.Join(dir, dbFile))

	v5 := t.TempDir()
	closeWS(t, open(t, v5, embeddedUpTo(t, 5)))
	added := qStrings(t, db, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	before := qStrings(t, inspect(t, filepath.Join(v5, dbFile)), `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	var diff []string
	for _, name := range added {
		found := false
		for _, b := range before {
			found = found || b == name
		}
		if !found {
			diff = append(diff, name)
		}
	}
	if want := []string{"check_approvals", "derived_origins", "request_envelopes", "source_fences"}; !reflect.DeepEqual(diff, want) {
		t.Fatalf("tables added by 0006 = %v, want %v", diff, want)
	}

	fks := func(table string) []string {
		return qStrings(t, db, `SELECT "table" || '.' || "from" || ' ' || on_delete FROM pragma_foreign_key_list(?)`, table)
	}
	for _, table := range []string{"request_envelopes", "derived_origins"} {
		if got := fks(table); !reflect.DeepEqual(got, []string{"sessions.session_id CASCADE"}) {
			t.Errorf("%s foreign keys = %v, want only session_id -> sessions CASCADE", table, got)
		}
	}
	for _, table := range []string{"source_fences", "check_approvals"} {
		if got := fks(table); len(got) != 0 {
			t.Errorf("%s has foreign keys %v; tombstones and approvals must outlive Sessions", table, got)
		}
	}
	for _, c := range v6EvalColumns {
		if n := qInt(t, db, `SELECT count(*) FROM pragma_table_info('eval_results') WHERE name=?`, c); n != 1 {
			t.Errorf("eval_results.%s missing", c)
		}
	}
	if got := qInt(t, db, `SELECT count(*) FROM pragma_table_info('eval_results')`) - qInt(t, inspect(t, filepath.Join(v5, dbFile)), `SELECT count(*) FROM pragma_table_info('eval_results')`); got != 6 {
		t.Errorf("eval_results gained %d columns, want 6", got)
	}
}

// TestV6ConstraintsRejectInvalidRows exercises the CHECKs that hold the state enumerations and the
// bounded, body-free shape of the rows.
func TestV6ConstraintsRejectInvalidRows(t *testing.T) {
	dir := t.TempDir()
	ws := open(t, dir, embeddedSet(t))
	defer closeWS(t, ws)
	mustExec(t, ws.DB, `INSERT INTO sessions(id, created_at, updated_at, status, goal, metadata_json) VALUES('s','t','t','idle','g','{}')`)
	digest := "'sha256:" + strings.Repeat("0", 64) + "'"
	env := func(state, extra string) string {
		return `INSERT INTO request_envelopes(request_id, session_id, purpose, state, lineage_complete, created_at, updated_at` + strings.SplitN(extra, "|", 2)[0] +
			`) VALUES('r' || abs(random()), 's', 'main', '` + state + `', 0, 't', 't'` + strings.SplitN(extra+"|", "|", 3)[1] + `)`
	}
	bad := map[string]string{
		"prepared is memory-only":        env("prepared", ""),
		"failed needs a reason":          env("failed", ""),
		"unknown needs a reason":         env("unknown", ""),
		"answered_seq needs answered":    env("admitted", ", answered_seq|, 3"),
		"seq range inverted":             env("admitted", ", first_seq, last_seq|, 5, 2"),
		"refs not an array":              env("admitted", ", refs_json|, '{}'"),
		"refs over the byte bound":       env("admitted", ", refs_json|, '[\""+strings.Repeat("x", 16400)+"\"]'"),
		"pending digest not a sha256":    env("admitted", ", pending_input_digest|, 'hello world'"),
		"lineage_complete is boolean":    `INSERT INTO request_envelopes(request_id, session_id, purpose, state, lineage_complete, created_at, updated_at) VALUES('x','s','main','admitted',2,'t','t')`,
		"orphan consumer session":        `INSERT INTO request_envelopes(request_id, session_id, purpose, state, lineage_complete, created_at, updated_at) VALUES('x','nope','main','admitted',0,'t','t')`,
		"derived kind outside the enum":  `INSERT INTO derived_origins(session_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete, created_at, updated_at) VALUES('s','blob','t','k','r','c',0,'t','t')`,
		"execution row without outcome":  `INSERT INTO derived_origins(session_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete, execution_id, tool_name, args_digest, created_at, updated_at) VALUES('s','execution','e','request','r','c',0,'e','shell',` + digest + `,'t','t')`,
		"edge row carrying tool columns": `INSERT INTO derived_origins(session_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete, tool_name, created_at, updated_at) VALUES('s','file','t','k','r','c',0,'shell','t','t')`,
		"outcome outside the enum":       `INSERT INTO derived_origins(session_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete, execution_id, tool_name, args_digest, outcome, created_at, updated_at) VALUES('s','execution','e','request','r','c',0,'e','shell',` + digest + `,'done','t','t')`,
		"pre_ref with pre_unavailable":   `INSERT INTO derived_origins(session_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete, execution_id, tool_name, args_digest, outcome, pre_ref, pre_unavailable, created_at, updated_at) VALUES('s','execution','e','request','r','c',0,'e','shell',` + digest + `,'admitted','p',1,'t','t')`,
		"fence scope outside the enum":   `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at) VALUES('project','x',1,'r','active','t','t')`,
		"fence epoch zero":               `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at) VALUES('session','x',0,'r','active','t','t')`,
		"fence tombstone not an object":  `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, tombstone_json, created_at, updated_at) VALUES('session','x',1,'r','active','[]','t','t')`,
		"fence state outside the enum":   `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at) VALUES('session','x',1,'r','gone','t','t')`,
		"check_set without its digest":   `INSERT INTO check_approvals(project_id, kind, approval_epoch, approved_by, actor, state, created_at, updated_at) VALUES('p','check_set',1,'user','a','active','t','t')`,
		"check_spec without a spec":      `INSERT INTO check_approvals(project_id, kind, check_id, approval_epoch, approved_by, actor, state, created_at, updated_at) VALUES('p','check_spec','c',1,'user','a','active','t','t')`,
		"binding without binding digest": `INSERT INTO check_approvals(project_id, kind, check_id, spec_digest, constraint_ref, constraint_digest, approval_epoch, approved_by, actor, state, created_at, updated_at) VALUES('p','constraint_binding','c','d','k','d',1,'user','a','active','t','t')`,
		"approval state outside enum":    `INSERT INTO check_approvals(project_id, kind, check_set_digest, approval_epoch, approved_by, actor, state, created_at, updated_at) VALUES('p','check_set','d',1,'user','a','pending','t','t')`,
		"episode status outside enum":    `INSERT INTO eval_results(session_id, seq, check_id, kind, status, provenance, fingerprint, artifacts_json, at, episode_status) VALUES('s',1,'c','test','passed','runtime','f','[]','t','finished')`,
	}
	for name, stmt := range bad {
		if _, err := ws.DB.Exec(stmt); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := []string{
		env("admitted", ""),
		env("answered", ", answered_seq|, 3"),
		env("failed", ", reason|, 'fenced'"),
		env("unknown", ", reason|, 'restart'"),
		`INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at) VALUES('source','session:a#3',1,'revoked','active','t','t')`,
		`INSERT INTO check_approvals(project_id, kind, check_set_digest, approval_epoch, approved_by, actor, state, created_at, updated_at) VALUES('p','check_set','d',1,'user','a','active','t','t')`,
		`INSERT INTO eval_results(session_id, seq, check_id, kind, status, provenance, fingerprint, artifacts_json, at, episode_id, episode_status) VALUES('s',1,'c','test','passed','runtime','f','[]','t','ep','open')`,
	}
	for _, stmt := range good {
		if _, err := ws.DB.Exec(stmt); err != nil {
			t.Errorf("rejected a valid row %q: %v", firstLine(stmt), err)
		}
	}
}

// Every pre-runner kind and every shipped intermediate version reaches 6 with its data intact; the new
// tables start empty and legacy evaluator rows have no episode, so nothing old acquires a lineage.
func TestUpgradeEveryVersionToV6(t *testing.T) {
	for from := 1; from <= 5; from++ {
		t.Run(fmt.Sprintf("v%d", from), func(t *testing.T) {
			dir := newFixture(t, fixtureGo)
			closeWS(t, open(t, dir, embeddedUpTo(t, from)))
			path := filepath.Join(dir, dbFile)
			if from == 5 {
				// Rows only a v5 database can hold, so the upgrade has legacy evaluator data to carry.
				db := inspect(t, path)
				mustExec(t, db, `INSERT INTO eval_results(session_id, seq, check_id, kind, status, provenance, fingerprint, artifacts_json, at)
					SELECT id, 1, 'go-test', 'test', 'passed', 'runtime', 'fp', '[]', 't' FROM sessions LIMIT 1`)
			}
			before := take(t, inspect(t, path))
			beforeEval := 0
			if from >= 4 {
				beforeEval = qInt(t, inspect(t, path), `SELECT count(*) FROM eval_results`)
			}

			ws := open(t, dir, embeddedUpTo(t, 6))
			defer closeWS(t, ws)
			if ws.From != from || ws.To != 6 {
				t.Fatalf("From=%d To=%d, want %d -> 6", ws.From, ws.To, from)
			}
			// 0003 rewrites session metadata and adds goal rows by design, so only a database already at
			// 3 or later has to come through 0006 unchanged.
			if !before.equal(take(t, ws.DB)) && from >= 3 {
				t.Error("existing data changed by the upgrade")
			}
			for _, table := range v6Tables {
				if n := qInt(t, ws.DB, `SELECT count(*) FROM `+table); n != 0 {
					t.Errorf("%s holds %d rows after the upgrade; nothing may be backfilled", table, n)
				}
			}
			if from >= 4 {
				if n := qInt(t, ws.DB, `SELECT count(*) FROM eval_results`); n != beforeEval {
					t.Errorf("eval_results %d -> %d rows", beforeEval, n)
				}
				if n := qInt(t, ws.DB, `SELECT count(*) FROM eval_results WHERE episode_id IS NOT NULL OR episode_status IS NOT NULL OR spec_digest IS NOT NULL OR approval_epoch IS NOT NULL`); n != 0 {
					t.Errorf("%d legacy eval_results rows gained episode data", n)
				}
			}
			if ic := qStrings(t, ws.DB, `PRAGMA integrity_check`); !reflect.DeepEqual(ic, []string{"ok"}) {
				t.Errorf("integrity_check=%v", ic)
			}
			if n := qInt(t, ws.DB, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
				t.Errorf("foreign_key_check rows=%d", n)
			}
			names := backups(t, dir)
			if len(names) == 0 || !strings.HasPrefix(names[len(names)-1], fmt.Sprintf("lumencortex.db.v%d.", from)) {
				t.Errorf("backups=%v, want a v%d copy", names, from)
			}
		})
	}
	t.Run("pre-runner kinds", func(t *testing.T) {
		for _, kind := range allFixtures {
			dir := newFixture(t, kind)
			ws := open(t, dir, embeddedSet(t))
			if ws.To != 6 || qInt(t, ws.DB, `PRAGMA user_version`) != 6 {
				t.Errorf("%s: To=%d", kind, ws.To)
			}
			closeWS(t, ws)
		}
	})
}

// 6 -> 5 returns the database to the exact v5 schema, keeps every other table row for row, and 5 -> 6
// works again afterwards.
func TestV6DownRoundTrip(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, embeddedUpTo(t, 5)))
	db := inspect(t, path)
	mustExec(t, db, `INSERT INTO eval_results(session_id, seq, check_id, kind, status, provenance, fingerprint, artifacts_json, at)
		SELECT id, 1, 'go-test', 'test', 'passed', 'runtime', 'fp', '[]', 't' FROM sessions LIMIT 1`)
	v5Schema := normalizedSchema(t, path)
	v5Dump := dumpAll(t, db)
	db.Close()

	set := embeddedUpTo(t, 6)
	closeWS(t, open(t, dir, set))
	// v6-only rows exist while at version 6; they are lost by design, everything else must survive. The
	// fence is a cleaned one: an active fence refuses the downgrade (fence_downgrade_test.go).
	v6 := inspect(t, path)
	mustExec(t, v6, `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at) VALUES('session','x',1,'r','cleaned','t','t')`)
	mustExec(t, v6, `UPDATE eval_results SET episode_id = 'ep', episode_status = 'closed'`)
	v6.Close()

	from, now, err := Downgrade(context.Background(), dir, set, 5)
	if err != nil || from != 6 || now != 5 {
		t.Fatalf("Downgrade: from=%d now=%d err=%v", from, now, err)
	}
	if got := normalizedSchema(t, path); got != v5Schema {
		t.Errorf("schema after 6->5 differs from the original v5 schema:\n%s\nwant\n%s", got, v5Schema)
	}
	after := inspect(t, path)
	if uv := qInt(t, after, `PRAGMA user_version`); uv != 5 {
		t.Errorf("user_version=%d", uv)
	}
	if got := dumpAll(t, after); got != v5Dump {
		t.Errorf("rows of the other tables changed across 6->5:\n%s\nwant\n%s", got, v5Dump)
	}
	if ic := qStrings(t, after, `PRAGMA integrity_check`); !reflect.DeepEqual(ic, []string{"ok"}) {
		t.Errorf("integrity_check=%v", ic)
	}
	after.Close()
	if got := backups(t, dir); len(got) == 0 || !strings.HasPrefix(got[len(got)-1], "lumencortex.db.v6.") {
		t.Errorf("backups=%v, want a v6 copy from the downgrade", got)
	}

	ws := open(t, dir, set)
	defer closeWS(t, ws)
	if ws.From != 5 || ws.To != 6 {
		t.Errorf("re-upgrade From=%d To=%d", ws.From, ws.To)
	}
	if n := qInt(t, ws.DB, `SELECT count(*) FROM eval_results WHERE episode_id IS NOT NULL`); n != 0 {
		t.Errorf("re-upgrade resurrected episode data (%d rows)", n)
	}
}

// A v5 database that already has a table of the same name is not adopted: the migration fails, the
// version stays 5, nothing of 0006 remains and the existing data is untouched.
func TestV6RefusesExistingSameNameObject(t *testing.T) {
	collisions := map[string]string{}
	for _, name := range v6Tables {
		collisions["table "+name] = `CREATE TABLE ` + name + `(keep TEXT); INSERT INTO ` + name + ` VALUES('precious')`
	}
	collisions["index idx_eval_results_episode"] = `CREATE INDEX idx_eval_results_episode ON sessions(id)`
	collisions["column eval_results.episode_id"] = `ALTER TABLE eval_results ADD COLUMN episode_id TEXT`
	for name, setup := range collisions {
		t.Run(name, func(t *testing.T) {
			dir := newFixture(t, fixtureGo)
			path := filepath.Join(dir, dbFile)
			closeWS(t, open(t, dir, embeddedUpTo(t, 5)))
			mustExec(t, inspect(t, path), setup)
			beforeDump := dumpAll(t, inspect(t, path))

			_, err := Open(context.Background(), dir, embeddedUpTo(t, 6))
			if err == nil || !strings.Contains(err.Error(), "0006") {
				t.Fatalf("Open err=%v, want a failure naming 0006", err)
			}
			db := inspect(t, path)
			if uv := qInt(t, db, `PRAGMA user_version`); uv != 5 {
				t.Errorf("user_version=%d, want 5", uv)
			}
			if got := dumpAll(t, db); got != beforeDump {
				t.Errorf("database changed by the refused migration:\n%s\nwant\n%s", got, beforeDump)
			}
		})
	}
}

// A failure after the last statement of 0006 rolls back everything the file did, including the ALTERs
// and indexes, and the next Open with the correct set succeeds.
func TestV6FailureMidMigrationLeavesV5Intact(t *testing.T) {
	body, err := embedded.ReadFile("sql/0006_provenance_and_approvals.sql")
	if err != nil {
		t.Fatal(err)
	}
	cut := embeddedUpTo(t, 5)
	fsys := embeddedFSUpTo(t, 5)
	fsys["0006_provenance_and_approvals.sql"] = &fstest.MapFile{Data: append(body, []byte("\nINSERT INTO no_such_table VALUES (1);\n")...)}
	broken, err := Load(fsys)
	if err != nil {
		t.Fatal(err)
	}

	dir := newFixture(t, fixtureGo)
	path := filepath.Join(dir, dbFile)
	closeWS(t, open(t, dir, cut))
	beforeDump := dumpAll(t, inspect(t, path))

	if _, err := Open(context.Background(), dir, broken); err == nil || !strings.Contains(err.Error(), "0006") {
		t.Fatalf("Open with a failing 0006: %v", err)
	}
	db := inspect(t, path)
	if uv := qInt(t, db, `PRAGMA user_version`); uv != 5 {
		t.Errorf("user_version=%d, want 5", uv)
	}
	if got := dumpAll(t, db); got != beforeDump {
		t.Error("a half-applied 0006 left schema or rows behind")
	}
	ws := open(t, dir, embeddedUpTo(t, 6))
	defer closeWS(t, ws)
	if ws.From != 5 || ws.To != 6 {
		t.Errorf("recovery From=%d To=%d", ws.From, ws.To)
	}
}

// A binary that only knows version 5 refuses a version 6 database and does not touch the file.
func TestV6IsRefusedByAV5Binary(t *testing.T) {
	dir := t.TempDir()
	closeWS(t, open(t, dir, embeddedUpTo(t, 6)))
	hashBefore := fileHashes(t, dir, dbFile)
	_, err := Open(context.Background(), dir, embeddedUpTo(t, 5))
	if !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("a v5 binary on a v6 database: %v, want ErrSchemaTooNew", err)
	}
	if _, _, err := Downgrade(context.Background(), dir, embeddedUpTo(t, 5), 4); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("a v5 binary's Downgrade of a v6 database: %v, want ErrSchemaTooNew", err)
	}
	if got := fileHashes(t, dir, dbFile); !reflect.DeepEqual(got, hashBefore) {
		t.Error("database file changed by the refused opens")
	}
}
