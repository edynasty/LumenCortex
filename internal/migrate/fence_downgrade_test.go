package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A downgrade below version 6 drops source_fences. While a fence is active or awaiting cleanup that
// would undo a committed revocation or an unfinished privacy cleanup (DATA §7), so it is refused
// before the backup with the database, its WAL and every fence row unchanged; a database whose fences
// are all cleaned, or that has none, still downgrades.

const insertFence = `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at)
	VALUES(?, ?, 1, 'test', ?, '2026-10-25T00:00:00Z', '2026-10-25T00:00:00Z')`

// fencedV6 is a latest-schema database holding one fence of the given kind and state, written last so
// its frames stay in a hot WAL.
func fencedV6(t *testing.T, kind, state string) string {
	t.Helper()
	dir := newFixture(t, fixtureGo)
	closeWS(t, open(t, dir, embeddedSet(t)))
	db := inspect(t, filepath.Join(dir, dbFile))
	if _, err := db.Exec(insertFence, kind, "ref-1", state); err != nil {
		t.Fatal(err)
	}
	return hotWALCopy(t, dir, `SELECT 1`)
}

func TestDowngradeBelowSixIsRefusedWhileAFenceIsLive(t *testing.T) {
	for _, tc := range []struct{ kind, state string }{
		{"source", "active"},
		{"session", "active"},
		{"privacy", "active"},
		{"privacy", "cleanup_pending"},
	} {
		t.Run(tc.kind+"/"+tc.state, func(t *testing.T) {
			dir := fencedV6(t, tc.kind, tc.state)
			if st, err := os.Stat(filepath.Join(dir, dbFile+"-wal")); err != nil || st.Size() == 0 {
				t.Fatalf("fixture has no WAL frames (stat: %v); the test would prove nothing", err)
			}
			set := embeddedSet(t)
			hashes := fileHashes(t, dir, stateFiles...)
			backupsBefore := backups(t, dir)

			from, now, err := Downgrade(context.Background(), dir, set, 5)
			if !errors.Is(err, ErrLiveFences) || from != set.Latest() || now != set.Latest() {
				t.Fatalf("Downgrade = %d, %d, %v; want ErrLiveFences at %d", from, now, err, set.Latest())
			}
			if got := fileHashes(t, dir, stateFiles...); !reflect.DeepEqual(got, hashes) {
				t.Error("the refusal changed the database or its WAL")
			}
			if got := backups(t, dir); !reflect.DeepEqual(got, backupsBefore) {
				t.Errorf("backups %v -> %v for a refused downgrade", backupsBefore, got)
			}
			if _, err := os.Stat(filepath.Join(dir, pendingMarker)); !os.IsNotExist(err) {
				t.Errorf("marker written for a refused downgrade (stat: %v)", err)
			}
			db := inspect(t, filepath.Join(dir, dbFile))
			if uv := qInt(t, db, `PRAGMA user_version`); uv != set.Latest() {
				t.Errorf("user_version=%d, want %d", uv, set.Latest())
			}
			if n := qInt(t, db, `SELECT count(*) FROM source_fences WHERE scope_kind = ? AND state = ?`, tc.kind, tc.state); n != 1 {
				t.Errorf("fence rows after the refusal = %d, want 1", n)
			}
			// Staying on the same version is still allowed: it changes nothing.
			if _, _, err := Downgrade(context.Background(), dir, set, set.Latest()); err != nil {
				t.Errorf("a no-op downgrade with a live fence: %v", err)
			}
		})
	}
}

func TestDowngradeBelowSixProceedsWithoutLiveFences(t *testing.T) {
	for name, state := range map[string]string{"no fence": "", "only cleaned fences": "cleaned"} {
		t.Run(name, func(t *testing.T) {
			var dir string
			if state == "" {
				dir = newFixture(t, fixtureGo)
				closeWS(t, open(t, dir, embeddedSet(t)))
			} else {
				dir = fencedV6(t, "privacy", state)
			}
			from, now, err := Downgrade(context.Background(), dir, embeddedSet(t), 5)
			if err != nil || from != embeddedSet(t).Latest() || now != 5 {
				t.Fatalf("Downgrade = %d, %d, %v; want 5", from, now, err)
			}
			if uv := qInt(t, inspect(t, filepath.Join(dir, dbFile)), `PRAGMA user_version`); uv != 5 {
				t.Errorf("user_version=%d, want 5", uv)
			}
		})
	}
}
