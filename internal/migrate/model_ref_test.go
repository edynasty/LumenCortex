package migrate

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/config"
)

func hasColumn(t *testing.T, path, table, column string) bool {
	t.Helper()
	return qInt(t, inspect(t, path), `SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column) == 1
}

// 0002's column CHECK must accept exactly the references config.ModelRef.Split accepts (plus NULL).
func TestModelRefCheckMatchesSplit(t *testing.T) {
	dir := t.TempDir()
	ws := open(t, dir, embeddedSet(t))
	defer closeWS(t, ws)
	cases := []string{"p/m", "p/", "/m", "p", "pm", "p/m/x", "a/b/c/d", "/", "", "provider-1/model.x:y", "p/ m", "é/ü"}
	for i, ref := range cases {
		_, _, splitOK := config.ModelRef(ref).Split()
		id := "s" + string(rune('a'+i))
		_, err := ws.DB.Exec(`INSERT INTO sessions(id, created_at, updated_at, status, goal, metadata_json, model_ref)
			VALUES(?, 't', 't', 'idle', 'g', '{}', ?)`, id, ref)
		if (err == nil) != splitOK {
			t.Errorf("model_ref %q: check accepted=%v, Split ok=%v (err=%v)", ref, err == nil, splitOK, err)
		}
	}
	if _, err := ws.DB.Exec(`INSERT INTO sessions(id, created_at, updated_at, status, goal, metadata_json, model_ref)
		VALUES('null', 't', 't', 'idle', 'g', '{}', NULL)`); err != nil {
		t.Errorf("NULL model_ref rejected: %v", err)
	}
}

// Every pre-runner database kind reaches the shipped version with its rows intact and model_ref NULL.
func TestUpgradeFixturesReachLatestWithNullModelRef(t *testing.T) {
	for _, kind := range allFixtures {
		t.Run(string(kind), func(t *testing.T) {
			dir := newFixture(t, kind)
			set := embeddedSet(t)
			ws := open(t, dir, set)
			defer closeWS(t, ws)
			if ws.From != 0 || ws.To != set.Latest() {
				t.Fatalf("From=%d To=%d, want 0 -> %d", ws.From, ws.To, set.Latest())
			}
			if !hasColumn(t, filepath.Join(dir, dbFile), "sessions", "model_ref") {
				t.Fatal("sessions.model_ref missing")
			}
			if n := qInt(t, ws.DB, `SELECT count(*) FROM sessions WHERE model_ref IS NOT NULL`); n != 0 {
				t.Errorf("%d legacy rows got a model_ref", n)
			}
			if ic := qStrings(t, ws.DB, `PRAGMA integrity_check`); !reflect.DeepEqual(ic, []string{"ok"}) {
				t.Errorf("integrity_check=%v", ic)
			}
		})
	}
}

// 2 -> 1 removes the column and keeps the database valid; an M2.2-era runner (Latest 1) refuses the
// version-2 database it cannot understand, and opens it again after the downgrade.
func TestModelRefDowngradeToVersionOne(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	set := embeddedSet(t)
	ws := open(t, dir, set)
	if _, err := ws.DB.Exec(`INSERT INTO sessions(id, created_at, updated_at, status, goal, metadata_json, model_ref)
		VALUES('kept', 't', 't', 'idle', 'keep me', '{}', 'p/m')`); err != nil {
		t.Fatal(err)
	}
	closeWS(t, ws)
	path := filepath.Join(dir, dbFile)

	if _, err := Open(context.Background(), dir, v1Set(t)); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("M2.2-era runner on a version-2 database: %v, want ErrSchemaTooNew", err)
	}

	from, now, err := Downgrade(context.Background(), dir, set, 1)
	if err != nil || from != set.Latest() || now != 1 {
		t.Fatalf("Downgrade: from=%d now=%d err=%v", from, now, err)
	}
	db := inspect(t, path)
	if hasColumn(t, path, "sessions", "model_ref") {
		t.Error("model_ref survived the downgrade")
	}
	if uv := qInt(t, db, `PRAGMA user_version`); uv != 1 {
		t.Errorf("user_version=%d", uv)
	}
	if ic := qStrings(t, db, `PRAGMA integrity_check`); !reflect.DeepEqual(ic, []string{"ok"}) {
		t.Errorf("integrity_check=%v", ic)
	}
	if goal := qStrings(t, db, `SELECT goal FROM sessions WHERE id='kept'`); !reflect.DeepEqual(goal, []string{"keep me"}) {
		t.Errorf("session row after downgrade: %v", goal)
	}
	if got := backups(t, dir); len(got) == 0 {
		t.Error("downgrade wrote no backup")
	}
	closeWS(t, open(t, dir, v1Set(t)))

	// Upgrading again re-adds the column, empty: the pins were lost with the downgrade.
	ws = open(t, dir, set)
	defer closeWS(t, ws)
	if ws.From != 1 || ws.To != set.Latest() {
		t.Fatalf("re-upgrade From=%d To=%d", ws.From, ws.To)
	}
	if n := qInt(t, ws.DB, `SELECT count(*) FROM sessions WHERE model_ref IS NOT NULL`); n != 0 {
		t.Errorf("model_ref values survived the downgrade/upgrade cycle: %d", n)
	}
}

// Version 1 has no down migration, so 2 -> 0 is refused before the backup with nothing written.
func TestDowngradeFromLatestToZeroIsRefusedBeforeBackup(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	set := embeddedSet(t)
	closeWS(t, open(t, dir, set))
	hashes := fileHashes(t, dir, dbFile, dbFile+"-wal")
	backupsBefore := backups(t, dir)

	_, _, err := Downgrade(context.Background(), dir, set, 0)
	if !errors.Is(err, ErrNoDownMigration) || !strings.Contains(err.Error(), "0001") {
		t.Fatalf("err=%v, want ErrNoDownMigration for 0001", err)
	}
	if got := fileHashes(t, dir, dbFile, dbFile+"-wal"); !reflect.DeepEqual(got, hashes) {
		t.Error("database or WAL changed")
	}
	if got := backups(t, dir); !reflect.DeepEqual(got, backupsBefore) {
		t.Errorf("backups %v -> %v", backupsBefore, got)
	}
	if uv := qInt(t, inspect(t, filepath.Join(dir, dbFile)), `PRAGMA user_version`); uv != set.Latest() {
		t.Errorf("user_version=%d, want %d", uv, set.Latest())
	}
}

// Concurrent Opens of an old database under the shipped set: every opener reaches the latest version,
// one backup is written, the schema is migrated once.
func TestConcurrentOpenReachesShippedSchema(t *testing.T) {
	for r := range 5 {
		dir := newFixture(t, fixtureGo)
		set := embeddedSet(t)
		const openers = 4
		ws := make([]*Workspace, openers)
		errs := make([]error, openers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range openers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				ws[i], errs[i] = Open(context.Background(), dir, set)
			}()
		}
		close(start)
		wg.Wait()
		for i := range openers {
			if errs[i] != nil {
				t.Fatalf("round %d opener %d: %v", r, i, errs[i])
			}
			if ws[i].To != set.Latest() {
				t.Errorf("round %d opener %d: To=%d", r, i, ws[i].To)
			}
		}
		if got := backups(t, dir); len(got) != 1 {
			t.Errorf("round %d: backups=%v, want one", r, got)
		}
		if !hasColumn(t, filepath.Join(dir, dbFile), "sessions", "model_ref") {
			t.Errorf("round %d: model_ref missing", r)
		}
		for _, w := range ws {
			closeWS(t, w)
		}
	}
}

// A failing 0002 rolls back to version 1; a later run of the real migration then succeeds.
func TestFailedModelRefMigrationRollsBackAndRecovers(t *testing.T) {
	dir := newFixture(t, fixtureGo)
	bad := syntheticSet(t, map[string]string{"0002_model_ref.sql": `ALTER TABLE sessions ADD COLUMN model_ref TEXT; SELECT * FROM no_such_table;`}, nil)
	if _, err := Open(context.Background(), dir, bad); err == nil || !strings.Contains(err.Error(), "0002_model_ref.sql") {
		t.Fatalf("Open with a failing 0002: %v", err)
	}
	path := filepath.Join(dir, dbFile)
	if uv := qInt(t, inspect(t, path), `PRAGMA user_version`); uv != 1 {
		t.Errorf("user_version=%d, want 1", uv)
	}
	if hasColumn(t, path, "sessions", "model_ref") {
		t.Error("the rolled-back migration left model_ref")
	}
	ws := open(t, dir, embeddedSet(t))
	defer closeWS(t, ws)
	if ws.From != 1 || ws.To != embeddedSet(t).Latest() {
		t.Errorf("recovery From=%d To=%d", ws.From, ws.To)
	}
}
