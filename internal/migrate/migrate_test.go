package migrate

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func open(t *testing.T, stateDir string, set *Set) *Workspace {
	t.Helper()
	ws, err := Open(context.Background(), stateDir, set)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return ws
}

func closeWS(t *testing.T, ws *Workspace) {
	t.Helper()
	if err := ws.Close(); err != nil {
		t.Fatal(err)
	}
}

func readBefore(t *testing.T, stateDir string) snapshot {
	t.Helper()
	db := inspect(t, filepath.Join(stateDir, dbFile))
	return take(t, db)
}

// TestUpgradeFixtures migrates the three pre-runner database kinds from version 0 to 1.
func TestUpgradeFixtures(t *testing.T) {
	for _, kind := range allFixtures {
		t.Run(string(kind), func(t *testing.T) {
			dir := newFixture(t, kind)
			before := readBefore(t, dir)
			beforeTables := qStrings(t, inspect(t, filepath.Join(dir, dbFile)), `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)

			ws := open(t, dir, embeddedSet(t))
			defer closeWS(t, ws)
			if ws.From != 0 || ws.To != 1 {
				t.Fatalf("From=%d To=%d, want 0 -> 1", ws.From, ws.To)
			}
			db := ws.DB
			if uv := qInt(t, db, `PRAGMA user_version`); uv != 1 {
				t.Fatalf("user_version=%d", uv)
			}
			if ic := qStrings(t, db, `PRAGMA integrity_check`); !reflect.DeepEqual(ic, []string{"ok"}) {
				t.Fatalf("integrity_check=%v", ic)
			}
			if n := qInt(t, db, `SELECT count(*) FROM pragma_foreign_key_check`); n != 0 {
				t.Fatalf("foreign_key_check rows=%d", n)
			}
			if mode := qStrings(t, db, `PRAGMA journal_mode`); mode[0] != "wal" {
				t.Fatalf("journal_mode=%v", mode)
			}
			for _, tbl := range []string{"metadata", "sessions", "session_messages", "agent_steps", "journal", "session_relations", "session_checkpoints"} {
				if qInt(t, db, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, tbl) != 1 {
					t.Errorf("table %s missing", tbl)
				}
			}
			if n := qInt(t, db, `SELECT count(*) FROM metadata WHERE key='schema_version'`); n != 0 {
				t.Errorf("schema_version rows=%d", n)
			}
			// Existing data, with only the reasoning keys removed, plus every unrelated table, intact.
			after := take(t, db)
			if !before.equal(after) {
				t.Errorf("data changed beyond reasoning removal:\nbefore=%+v\nafter=%+v", before, after)
			}
			if kind == fixtureNode && len(before.foreignRows) < 10 {
				t.Errorf("Node fixture has only %d foreign tables", len(before.foreignRows))
			}
			if n := qInt(t, db, `SELECT count(*) FROM session_messages WHERE json_type(json,'$.reasoning') IS NOT NULL OR json_type(json,'$.reasoning_content') IS NOT NULL`); n != 0 {
				t.Errorf("%d rows keep reasoning keys", n)
			}
			afterTables := qStrings(t, db, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
			if kind != fixtureGo && reflect.DeepEqual(beforeTables, afterTables) {
				t.Errorf("%s fixture gained no tables; session_relations/session_checkpoints expected", kind)
			}

			// Backup: exactly one durable v0 copy, scrubbed, with the old schema_version row.
			names := backups(t, dir)
			if len(names) != 1 || !strings.HasPrefix(names[0], "lumencortex.db.v0.") || strings.HasSuffix(names[0], ".partial") {
				t.Fatalf("backups=%v", names)
			}
			b := inspect(t, filepath.Join(dir, "backups", names[0]))
			if uv := qInt(t, b, `PRAGMA user_version`); uv != 0 {
				t.Errorf("backup user_version=%d", uv)
			}
			if n := qInt(t, b, `SELECT count(*) FROM sessions`); n != len(before.sessions) {
				t.Errorf("backup sessions=%d", n)
			}
			if n := qInt(t, b, `SELECT count(*) FROM metadata WHERE key='schema_version'`); n != 1 {
				t.Errorf("backup lost the pre-migration metadata row")
			}
			if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
				t.Errorf("checkpoint.pending left after a clean upgrade (stat: %v)", err)
			}
		})
	}
}

// TestNoReasoningAtRest is S1 test 14: after the upgrade no byte of historical provider reasoning
// remains in the live database, its WAL or the durable backup, while the workspace is open and after
// it is closed. Row-level queries alone would not prove this; the files are searched for the bytes.
func TestNoReasoningAtRest(t *testing.T) {
	for _, kind := range allFixtures {
		t.Run(string(kind), func(t *testing.T) {
			dir := newFixture(t, kind)
			// The fixture holds the sentinel; without the migration it is on disk.
			if hits := filesWithSentinel(t, dir); len(hits) == 0 {
				t.Fatal("fixture does not contain the sentinel; the test would prove nothing")
			}
			ws := open(t, dir, embeddedSet(t))
			if hits := filesWithSentinel(t, dir); len(hits) != 0 {
				t.Errorf("reasoning bytes in %v while the workspace is open", hits)
			}
			for _, name := range append([]string{"live"}, backups(t, dir)...) {
				db := ws.DB
				if name != "live" {
					db = inspect(t, filepath.Join(dir, "backups", name))
				}
				if n := qInt(t, db, `SELECT count(*) FROM sqlite_master WHERE type='table' AND lower(name) LIKE '%reasoning%'`); n != 0 {
					t.Errorf("%s: %d reasoning tables", name, n)
				}
				if n := qInt(t, db, `SELECT count(*) FROM session_messages WHERE json_type(json,'$.reasoning') IS NOT NULL OR json_type(json,'$.reasoning_content') IS NOT NULL`); n != 0 {
					t.Errorf("%s: %d transcript rows keep reasoning", name, n)
				}
				for _, hit := range columnsContaining(t, db, sentinel) {
					t.Errorf("%s: sentinel in %s", name, hit)
				}
			}
			closeWS(t, ws)
			if hits := filesWithSentinel(t, dir); len(hits) != 0 {
				t.Errorf("reasoning bytes in %v after Close", hits)
			}
			if got := backups(t, dir); len(got) != 1 || strings.HasSuffix(got[0], ".partial") {
				t.Errorf("backups=%v, want one durable backup and no partial", got)
			}
			if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
				t.Errorf("checkpoint.pending left after a clean run (stat: %v)", err)
			}
		})
	}
}

// columnsContaining lists table.column=count for every core-table column whose text holds needle.
func columnsContaining(t *testing.T, db *sql.DB, needle string) []string {
	t.Helper()
	var hits []string
	for _, tbl := range qStrings(t, db, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`) {
		for _, col := range qStrings(t, db, `SELECT name FROM pragma_table_info(?)`, tbl) {
			q := fmt.Sprintf(`SELECT count(*) FROM "%s" WHERE instr(CAST("%s" AS TEXT), ?) > 0`, tbl, col)
			if n := qInt(t, db, q, needle); n > 0 {
				hits = append(hits, fmt.Sprintf("%s.%s=%d", tbl, col, n))
			}
		}
	}
	return hits
}

// TestScrubStatementMatches0001 keeps the backup scrub and migration 0001 on the same statement.
func TestScrubStatementMatches0001(t *testing.T) {
	body, err := testdataEmbedded("0001_v1.sql")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(scrubStatement)) {
		t.Fatalf("0001_v1.sql does not contain scrubStatement:\n%s", scrubStatement)
	}
}

// TestReopenPerformsNoMigration: an up-to-date database opens without a backup, marker or write.
func TestReopenPerformsNoMigration(t *testing.T) {
	dir := newFixture(t, fixtureNode)
	closeWS(t, open(t, dir, embeddedSet(t)))
	before := fileHashes(t, dir, dbFile, dbFile+"-wal")
	backupsBefore := backups(t, dir)

	ws := open(t, dir, embeddedSet(t))
	if ws.From != 1 || ws.To != 1 {
		t.Errorf("From=%d To=%d, want 1 -> 1", ws.From, ws.To)
	}
	closeWS(t, ws)

	if after := fileHashes(t, dir, dbFile, dbFile+"-wal"); !reflect.DeepEqual(before, after) {
		t.Error("reopening an up-to-date database changed its files")
	}
	if got := backups(t, dir); !reflect.DeepEqual(got, backupsBefore) {
		t.Errorf("backups %v -> %v", backupsBefore, got)
	}
}

// TestFreshDatabaseMigratesWithoutBackup: an empty state directory gets schema 1, nothing to back up.
func TestFreshDatabaseMigratesWithoutBackup(t *testing.T) {
	dir := t.TempDir()
	ws := open(t, dir, embeddedSet(t))
	defer closeWS(t, ws)
	if ws.From != 0 || ws.To != 1 || qInt(t, ws.DB, `PRAGMA user_version`) != 1 {
		t.Fatalf("From=%d To=%d", ws.From, ws.To)
	}
	if got := backups(t, dir); len(got) != 0 {
		t.Errorf("backups=%v for an empty database", got)
	}
}

// TestSchemaTooNewWritesNothing: a database newer than the binary is refused, and not even the
// journal mode or the directory contents change.
func TestSchemaTooNewWritesNothing(t *testing.T) {
	for _, mode := range []string{"wal", "delete"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, dbFile)
			db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?_journal_mode="+mode)
			if err != nil {
				t.Fatal(err)
			}
			mustExec(t, db, `CREATE TABLE sessions(id TEXT) STRICT; INSERT INTO sessions VALUES('x'); PRAGMA user_version = 5`)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			filesBefore := listDir(t, dir)
			hashBefore := fileHashes(t, dir, dbFile)

			_, err = Open(context.Background(), dir, embeddedSet(t))
			if !errors.Is(err, ErrSchemaTooNew) {
				t.Fatalf("Open err=%v, want ErrSchemaTooNew", err)
			}
			if got := fileHashes(t, dir, dbFile); !reflect.DeepEqual(got, hashBefore) {
				t.Error("database file changed")
			}
			// Only the lock file and, for WAL, the WAL index may appear; no backup, marker or partial.
			for _, name := range listDir(t, dir) {
				switch name {
				case dbFile, dbFile + "-wal", dbFile + "-shm", "db.lock":
				default:
					t.Errorf("unexpected file %s (before: %v)", name, filesBefore)
				}
			}
			// The lock was released: a downgrade attempt reaches the version check instead of timing out.
			if _, _, err := Downgrade(context.Background(), dir, embeddedSet(t), 0); !errors.Is(err, ErrSchemaTooNew) {
				t.Errorf("Downgrade of a too-new database: %v, want ErrSchemaTooNew", err)
			}
			if got := fileHashes(t, dir, dbFile); !reflect.DeepEqual(got, hashBefore) {
				t.Error("database file changed by the refused Downgrade")
			}
			if uv := qInt(t, inspect(t, path), `PRAGMA user_version`); uv != 5 {
				t.Errorf("user_version=%d", uv)
			}
		})
	}
}

// TestLoadRejectsGapsAndOrphans: the numbering must run 0001..N without gaps and every down file
// needs its up file; a Set that fails to load opens no database.
func TestLoadRejectsGapsAndOrphans(t *testing.T) {
	ok := "CREATE TABLE a(x INTEGER) STRICT;"
	cases := map[string]fstest.MapFS{
		"gap":              {"0001_a.sql": {Data: []byte(ok)}, "0003_c.sql": {Data: []byte(ok)}},
		"starts at 0002":   {"0002_b.sql": {Data: []byte(ok)}},
		"down without up":  {"0001_a.sql": {Data: []byte(ok)}, "down/0002_b.down.sql": {Data: []byte(ok)}},
		"duplicate number": {"0001_a.sql": {Data: []byte(ok)}, "0001_b.sql": {Data: []byte(ok)}},
	}
	for name, fsys := range cases {
		t.Run(name, func(t *testing.T) {
			if set, err := Load(fsys); err == nil {
				t.Fatalf("Load accepted %s: latest=%d", name, set.Latest())
			}
		})
	}
	if set := embeddedSet(t); set.Latest() != 1 {
		t.Errorf("embedded Latest=%d, M2.1 ships exactly 0001", set.Latest())
	}
}

// TestMigrationAndVersionAreAtomic: a failing migration leaves neither its tables nor its version;
// versions committed before it stay and the next Open continues from there.
func TestMigrationAndVersionAreAtomic(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	bad := syntheticSet(t, map[string]string{"0002_bad.sql": `CREATE TABLE t2(x INTEGER) STRICT; INSERT INTO no_such_table VALUES (1);`}, nil)

	_, err := Open(context.Background(), dir, bad)
	if err == nil || !strings.Contains(err.Error(), "0002_bad.sql") {
		t.Fatalf("Open with a failing 0002: %v", err)
	}
	db := inspect(t, filepath.Join(dir, dbFile))
	if uv := qInt(t, db, `PRAGMA user_version`); uv != 1 {
		t.Errorf("user_version=%d, want 1 (0001 committed, 0002 rolled back)", uv)
	}
	if n := qInt(t, db, `SELECT count(*) FROM sqlite_master WHERE name='t2'`); n != 0 {
		t.Errorf("t2 survived the rolled-back migration")
	}
	if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
		t.Errorf("marker left after a failed run whose checkpoint completed (stat: %v)", err)
	}
	if filesBad := filesWithSentinel(t, dir); len(filesBad) != 0 {
		t.Errorf("reasoning bytes in %v after a failed migration", filesBad)
	}

	good := syntheticSet(t, map[string]string{"0002_ok.sql": `CREATE TABLE t2(x INTEGER) STRICT;`}, nil)
	ws := open(t, dir, good)
	defer closeWS(t, ws)
	if ws.From != 1 || ws.To != 2 || qInt(t, ws.DB, `SELECT count(*) FROM sqlite_master WHERE name='t2'`) != 1 {
		t.Errorf("continuing from 1: From=%d To=%d", ws.From, ws.To)
	}
	if got := backups(t, dir); len(got) != 2 {
		t.Errorf("backups=%v, want one per schema change", got)
	}
}

// TestCancelRollsBackMigrationInProgress: cancelling a long migration rolls it back; the version
// committed before it stays.
func TestCancelRollsBackMigrationInProgress(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	slow := syntheticSet(t, map[string]string{"0002_slow.sql": `CREATE TABLE big(x INTEGER) STRICT;
WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 400000000) INSERT INTO big SELECT i FROM c;`}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	start := time.Now()
	_, err := Open(ctx, dir, slow)
	if !errors.Is(err, context.Canceled) && (err == nil || !strings.Contains(err.Error(), "interrupt")) {
		t.Fatalf("Open cancelled mid-migration: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("cancellation took %v", d)
	}
	db := inspect(t, filepath.Join(dir, dbFile))
	if uv := qInt(t, db, `PRAGMA user_version`); uv != 1 {
		t.Errorf("user_version=%d, want 1", uv)
	}
	if n := qInt(t, db, `SELECT count(*) FROM sqlite_master WHERE name='big'`); n != 0 {
		t.Error("the cancelled migration's table survived")
	}
	// The lock was released, so a plain Open at the embedded version works at once.
	closeWS(t, open(t, dir, embeddedSet(t)))
}

// TestReappearedSchemaVersionIsRemovedAndNeverAuthority: a pre-runner binary that opens a migrated
// database writes metadata.schema_version again (R3). Open deletes the row, warns, and the row never
// changes the schema version the runner uses.
func TestReappearedSchemaVersionIsRemovedAndNeverAuthority(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	closeWS(t, open(t, dir, embeddedSet(t)))

	db := inspect(t, filepath.Join(dir, dbFile))
	mustExec(t, db, `INSERT INTO metadata(key, value) VALUES('schema_version', '99')`)

	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	ws := open(t, dir, embeddedSet(t))
	if ws.From != 1 || ws.To != 1 {
		t.Errorf("a schema_version=99 row changed the version: From=%d To=%d", ws.From, ws.To)
	}
	if n := qInt(t, ws.DB, `SELECT count(*) FROM metadata WHERE key='schema_version'`); n != 0 {
		t.Error("schema_version row not removed")
	}
	closeWS(t, ws)
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "schema_version") {
		t.Errorf("no warning logged: %q", logs.String())
	}

	logs.Reset()
	closeWS(t, open(t, dir, embeddedSet(t)))
	if logs.Len() != 0 {
		t.Errorf("warning repeated without a new foreign write: %q", logs.String())
	}
	// A foreign row never triggers a migration or a backup.
	if got := backups(t, dir); len(got) != 1 {
		t.Errorf("backups=%v", got)
	}
}

// TestDowngradeMechanism exercises the numbered down-migration path with synthetic versions: M2.1
// ships no down file (version 1 has none), but later migrations will rely on this mechanism.
func TestDowngradeMechanism(t *testing.T) {
	up := map[string]string{
		"0002_col.sql": `ALTER TABLE sessions ADD COLUMN model_ref TEXT;`,
		"0003_tbl.sql": `CREATE TABLE t3(x INTEGER) STRICT; INSERT INTO t3 VALUES (7);`,
	}
	down := map[string]string{
		"0002_col.down.sql": `ALTER TABLE sessions DROP COLUMN model_ref;`,
		"0003_tbl.down.sql": `DROP TABLE t3;`,
	}
	set := syntheticSet(t, up, down)

	setup := func(t *testing.T) string {
		dir := newFixture(t, fixtureNode)
		closeWS(t, open(t, dir, set))
		return dir
	}

	t.Run("two steps in one transaction", func(t *testing.T) {
		dir := setup(t)
		before := backups(t, dir)
		from, now, err := Downgrade(context.Background(), dir, set, 1)
		if err != nil || from != 3 || now != 1 {
			t.Fatalf("Downgrade: from=%d now=%d err=%v", from, now, err)
		}
		db := inspect(t, filepath.Join(dir, dbFile))
		if uv := qInt(t, db, `PRAGMA user_version`); uv != 1 {
			t.Errorf("user_version=%d", uv)
		}
		if qInt(t, db, `SELECT count(*) FROM pragma_table_info('sessions') WHERE name='model_ref'`) != 0 || qInt(t, db, `SELECT count(*) FROM sqlite_master WHERE name='t3'`) != 0 {
			t.Error("down files did not run")
		}
		if n := qInt(t, db, `SELECT count(*) FROM sessions`); n != 3 {
			t.Errorf("sessions=%d", n)
		}
		if got := backups(t, dir); len(got) != len(before)+1 || !strings.Contains(strings.Join(got, ","), ".v3.") {
			t.Errorf("backups %v -> %v, want a new v3 backup", before, got)
		}
		if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
			t.Errorf("marker left (stat: %v)", err)
		}
		if hits := filesWithSentinel(t, dir); len(hits) != 0 {
			t.Errorf("reasoning bytes in %v", hits)
		}
		if ic := qStrings(t, db, `PRAGMA integrity_check`); !reflect.DeepEqual(ic, []string{"ok"}) {
			t.Errorf("integrity_check=%v", ic)
		}
	})

	t.Run("failure rolls every step back", func(t *testing.T) {
		dir := setup(t)
		broken := syntheticSet(t, up, map[string]string{
			"0002_col.down.sql": `ALTER TABLE sessions DROP COLUMN no_such_column;`,
			"0003_tbl.down.sql": `DROP TABLE t3;`,
		})
		_, _, err := Downgrade(context.Background(), dir, broken, 1)
		if err == nil || !strings.Contains(err.Error(), "0002_col.down.sql") {
			t.Fatalf("Downgrade: %v", err)
		}
		db := inspect(t, filepath.Join(dir, dbFile))
		if uv := qInt(t, db, `PRAGMA user_version`); uv != 3 {
			t.Errorf("user_version=%d, want 3", uv)
		}
		if qInt(t, db, `SELECT count(*) FROM sqlite_master WHERE name='t3'`) != 1 || qInt(t, db, `SELECT count(*) FROM t3`) != 1 {
			t.Error("the newest down file's effect was not rolled back")
		}
	})

	t.Run("missing down file is refused before any write", func(t *testing.T) {
		dir := setup(t)
		hashes := fileHashes(t, dir, dbFile, dbFile+"-wal")
		backupsBefore := backups(t, dir)
		partial := syntheticSet(t, up, map[string]string{"0003_tbl.down.sql": `DROP TABLE t3;`})
		_, _, err := Downgrade(context.Background(), dir, partial, 1)
		if !errors.Is(err, ErrNoDownMigration) {
			t.Fatalf("err=%v, want ErrNoDownMigration", err)
		}
		// Version 1 has no down file: --to 0 is refused in every Set.
		_, _, err = Downgrade(context.Background(), dir, set, 0)
		if !errors.Is(err, ErrNoDownMigration) {
			t.Fatalf("--to 0: err=%v, want ErrNoDownMigration", err)
		}
		if got := fileHashes(t, dir, dbFile, dbFile+"-wal"); !reflect.DeepEqual(got, hashes) {
			t.Error("a refused downgrade wrote to the database")
		}
		if got := backups(t, dir); !reflect.DeepEqual(got, backupsBefore) {
			t.Errorf("a refused downgrade wrote backups: %v -> %v", backupsBefore, got)
		}
		if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
			t.Error("a refused downgrade left a marker")
		}
	})

	t.Run("targets outside the range", func(t *testing.T) {
		dir := setup(t)
		hashes := fileHashes(t, dir, dbFile, dbFile+"-wal")
		if from, now, err := Downgrade(context.Background(), dir, set, 3); err != nil || from != 3 || now != 3 {
			t.Errorf("--to current: from=%d now=%d err=%v, want a no-op", from, now, err)
		}
		for _, to := range []int{4, -1} {
			if _, _, err := Downgrade(context.Background(), dir, set, to); err == nil {
				t.Errorf("--to %d accepted", to)
			}
		}
		if got := fileHashes(t, dir, dbFile, dbFile+"-wal"); !reflect.DeepEqual(got, hashes) {
			t.Error("a no-op or refused downgrade wrote to the database")
		}
	})

	t.Run("no database", func(t *testing.T) {
		dir := t.TempDir()
		if _, _, err := Downgrade(context.Background(), dir, set, 0); err == nil {
			t.Fatal("Downgrade without a database succeeded")
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("Downgrade created %v", got)
		}
	})
}

// TestDowngradeVersionOneToZeroIsRefused is the shipped M2.1 case: version 1 has no down migration.
func TestDowngradeVersionOneToZeroIsRefused(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	closeWS(t, open(t, dir, embeddedSet(t)))
	hashes := fileHashes(t, dir, dbFile, dbFile+"-wal")
	backupsBefore := backups(t, dir)

	_, _, err := Downgrade(context.Background(), dir, embeddedSet(t), 0)
	if !errors.Is(err, ErrNoDownMigration) {
		t.Fatalf("err=%v, want ErrNoDownMigration", err)
	}
	if got := fileHashes(t, dir, dbFile, dbFile+"-wal"); !reflect.DeepEqual(got, hashes) {
		t.Error("database changed")
	}
	if got := backups(t, dir); !reflect.DeepEqual(got, backupsBefore) {
		t.Errorf("backups %v -> %v", backupsBefore, got)
	}
	if uv := qInt(t, inspect(t, filepath.Join(dir, dbFile)), `PRAGMA user_version`); uv != 1 {
		t.Errorf("user_version=%d", uv)
	}
}
