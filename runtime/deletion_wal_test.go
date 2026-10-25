package runtime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/migrate"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// walReader is a second SQLite connection to the state database holding a read snapshot, as another
// process's reader would: while it holds it, no checkpoint can truncate the WAL.
type walReader struct {
	db *sql.DB
	tx *sql.Tx
}

func holdWALReader(t *testing.T, path string) *walReader {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM session_messages`).Scan(&n); err != nil {
		tx.Rollback()
		db.Close()
		t.Fatal(err)
	}
	r := &walReader{db: db, tx: tx}
	t.Cleanup(r.release)
	return r
}

func (r *walReader) release() {
	if r.tx != nil {
		_ = r.tx.Rollback()
		r.tx = nil
	}
	if r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
}

// shortCheckpointWait bounds how long one truncation attempt waits for a reader.
func shortCheckpointWait(t *testing.T) {
	old := migrate.CheckpointWait
	migrate.CheckpointWait = 100 * time.Millisecond
	t.Cleanup(func() { migrate.CheckpointWait = old })
}

// privacyStates returns the states of the privacy rows of ids.
func privacyStates(t *testing.T, engine *Engine, ids ...string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var out []string
	for _, id := range ids {
		var state string
		if err := db.QueryRow(`SELECT state FROM source_fences WHERE scope_kind = 'privacy' AND scope_ref = ?
			ORDER BY epoch DESC LIMIT 1`, id).Scan(&state); err != nil {
			t.Fatalf("privacy row of %s: %v", id, err)
		}
		out = append(out, state)
	}
	return out
}

func markerExists(t *testing.T, dbPath string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "checkpoint.pending"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

// A deletion whose rows are gone and whose file was rebuilt, but whose WAL another connection's
// reader keeps from being truncated, is not finished: the WAL and the main file can still hold the
// content. DeleteSession reports ErrDeletionPending, not success; the fences stay cleanup_pending
// and the checkpoint marker stays. Once the reader is gone the next DeleteSession finishes it - no
// byte of the content is left - and a further one finds nothing (ErrNotFound).
func TestDeletionBlockedByAWALReaderIsNotReportedComplete(t *testing.T) {
	shortCheckpointWait(t)
	f := newDeletionFixture(t)
	defer f.engine.Close()
	defer f.supervisor.Close()
	ctx := context.Background()
	path := f.engine.store.Path()

	reader := holdWALReader(t, path)
	began := time.Now()
	_, err := f.engine.DeleteSession(ctx, f.child, WorktreeCleanup{})
	if !errors.Is(err, ErrDeletionPending) {
		t.Fatalf("delete while a reader blocks the WAL truncation: %v, want ErrDeletionPending", err)
	}
	if took := time.Since(began); took > 30*time.Second {
		t.Fatalf("the blocked delete waited %v for the reader", took)
	}
	if states := privacyStates(t, f.engine, f.child, f.grandchild); !slices.Equal(states, []string{"cleanup_pending", "cleanup_pending"}) {
		t.Fatalf("privacy rows %v before the WAL is truncated", states)
	}
	if pending, err := f.engine.store.PendingPrivacyDeletion(ctx, f.child); err != nil || pending == nil {
		t.Fatalf("pending deletion %v %v", pending, err)
	}
	if !markerExists(t, path) {
		t.Fatal("checkpoint marker removed before the truncation")
	}
	if len(sentinelAtRest(t, path)) == 0 {
		t.Fatal("fixture: the reader did not keep the content at rest; nothing was blocked")
	}
	if _, err := f.engine.store.Get(ctx, f.child); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the rows of the deleted session: %v", err)
	}
	// Still blocked: a retry stays pending and does not wait for the reader without bound.
	if _, err := f.engine.DeleteSession(ctx, f.child, WorktreeCleanup{}); !errors.Is(err, ErrDeletionPending) {
		t.Fatalf("retry while still blocked: %v", err)
	}
	// The rows are gone but the deletion is not finished: importing the id again is refused.
	reimport := []session.ImportedSession{{Session: session.Session{ID: f.child, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		Status: "completed", Goal: "imported again", Metadata: []byte("{}")}, Messages: []protocol.Message{{Role: "user", Content: "imported again"}}}}
	if err := f.engine.store.ImportSessions(ctx, reimport); !errors.Is(err, session.ErrCleanupPending) {
		t.Fatalf("import while the deletion is pending: %v", err)
	}

	reader.release()
	res, err := f.engine.DeleteSession(ctx, f.child, WorktreeCleanup{})
	if err != nil || !slices.Equal(res.DeletedChildren, []string{f.grandchild}) {
		t.Fatalf("resumed delete %+v %v", res, err)
	}
	if found := sentinelAtRest(t, path); len(found) != 0 {
		t.Fatalf("the sentinel is still in %v", found)
	}
	if states := privacyStates(t, f.engine, f.child, f.grandchild); !slices.Equal(states, []string{"cleaned", "cleaned"}) {
		t.Fatalf("privacy rows %v after the truncation", states)
	}
	if markerExists(t, path) {
		t.Fatal("checkpoint marker left after the truncation")
	}
	if _, err := f.engine.DeleteSession(ctx, f.child, WorktreeCleanup{}); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("deleting again: %v", err)
	}
	if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, f.child); err != nil || !withdrawn {
		t.Fatalf("deleted child withdrawn %v %v", withdrawn, err)
	}
	// A finished deletion is superseded by an explicit re-import; its body-free privacy record stays.
	if err := f.engine.store.ImportSessions(ctx, reimport); err != nil {
		t.Fatalf("import after the deletion finished: %v", err)
	}
	if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, f.child); err != nil || withdrawn {
		t.Fatalf("re-imported session withdrawn %v %v", withdrawn, err)
	}
	if states := privacyStates(t, f.engine, f.child); !slices.Equal(states, []string{"cleaned"}) {
		t.Fatalf("privacy record after the re-import: %v", states)
	}
}
