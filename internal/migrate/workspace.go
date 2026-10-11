package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/runlock"
)

// pendingMarker marks a schema change whose post-change WAL checkpoint did not complete, so the main
// file or its WAL may still hold stripped bytes. Every Open that holds db.lock shared retries the
// checkpoint and removes the marker once it completes.
const pendingMarker = "checkpoint.pending"

// Workspace is an open workspace database that holds <stateDir>/db.lock shared. Close releases both.
type Workspace struct {
	DB *sql.DB
	// From is the user_version observed when Open started; To is the version it returned with (the
	// latest this binary knows). From == To when no migration was needed.
	From, To int

	lock *runlock.Lock
}

// Close closes the database and releases the shared schema lock.
func (w *Workspace) Close() error {
	return errors.Join(w.DB.Close(), w.lock.Release())
}

// Open opens <stateDir>/lumencortex.db and keeps <stateDir>/db.lock shared for as long as it is open.
// It loops until LockWait has passed or ctx is done:
//
//   - take db.lock shared (non-blocking); if taken, read user_version:
//     == latest -> finish a pending checkpoint, drop a foreign schema_version row, return holding shared;
//     >  latest -> release, ErrSchemaTooNew, nothing written;
//     <  latest -> release shared, try db.lock exclusive (non-blocking); if taken, migrate
//     (re-reading user_version first), release, and loop to take shared again;
//   - a current database whose checkpoint.pending still owes the rebuild (a run stopped after the
//     migrations committed) is finished the same way under the exclusive lock; if the lock is
//     busy, Open continues with the shared lock and leaves the marker for a later Open;
//   - if no lock could be kept, sleep retryDelay and retry. Reaching the deadline while other
//     processes kept db.lock is ErrDatabaseInUse.
//
// Concurrent opens of one old database all succeed: one migrates under the exclusive lock, the
// others then see the latest version under the shared lock. The stateDir must exist.
func Open(ctx context.Context, stateDir string, set *Set) (*Workspace, error) {
	path := filepath.Join(stateDir, dbFile)
	deadline := time.Now().Add(LockWait)
	from := -1
	scrubDeferred := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		need := needNothing
		shared, err := runlock.TryAcquireShared(stateDir, dbLockID)
		switch {
		case err == nil:
			var ws *Workspace
			var v int
			ws, v, need, err = openShared(ctx, path, stateDir, set, shared, scrubDeferred)
			if from < 0 {
				from = v
			}
			if err != nil {
				return nil, err
			}
			if ws != nil {
				ws.From = from
				return ws, nil
			}
			// The shared lock was released: exclusive work (migration or rebuild) comes next.
		case !errors.Is(err, runlock.ErrHeld):
			return nil, err
		}

		ex, err := runlock.TryAcquire(stateDir, dbLockID)
		if err == nil {
			mfrom, _, merr := upgradeLocked(ctx, path, stateDir, set)
			if from < 0 {
				from = mfrom
			}
			if rerr := ex.Release(); merr == nil {
				merr = rerr
			}
			if merr != nil {
				return nil, merr
			}
			continue
		}
		if !errors.Is(err, runlock.ErrHeld) {
			return nil, err
		}
		if need == needScrub {
			// The schema is current, so other processes may keep working; the rebuild waits.
			scrubDeferred = true
			continue
		}

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("open %s: %w", path, ErrDatabaseInUse)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}

// exclusiveWork says what a database that cannot be opened under the shared lock needs.
type exclusiveWork int

const (
	needNothing exclusiveWork = iota
	needMigrate
	needScrub
)

// openShared opens the database under a shared lock that the caller just took. For a current
// database it returns a Workspace that owns the lock. When exclusive work is needed it releases the
// lock and returns a nil Workspace and the work; for a newer database or on error it releases the
// lock and returns the error. The version read is returned in every case. skipScrub ignores a marker
// that owes the rebuild, for an Open that found the exclusive lock busy.
func openShared(ctx context.Context, path, stateDir string, set *Set, shared *runlock.Lock, skipScrub bool) (*Workspace, int, exclusiveWork, error) {
	fail := func(db *sql.DB, v int, need exclusiveWork, err error) (*Workspace, int, exclusiveWork, error) {
		if db != nil {
			db.Close()
		}
		shared.Release()
		return nil, v, need, err
	}
	db, err := openDB(path)
	if err != nil {
		return fail(nil, 0, needNothing, err)
	}
	v, err := userVersion(ctx, db)
	if err != nil {
		return fail(db, 0, needNothing, err)
	}
	switch {
	case v > set.Latest():
		return fail(db, v, needNothing, fmt.Errorf("%w: have user_version %d, binary knows %d", ErrSchemaTooNew, v, set.Latest()))
	case v < set.Latest():
		return fail(db, v, needMigrate, nil)
	}
	kind, err := readMarker(stateDir)
	if err != nil {
		return fail(db, v, needNothing, err)
	}
	if kind == markerVacuum && !skipScrub {
		return fail(db, v, needScrub, nil)
	}
	if err := useWAL(ctx, db); err != nil {
		return fail(db, v, needNothing, err)
	}
	if err := completePending(ctx, db, stateDir); err != nil {
		return fail(db, v, needNothing, err)
	}
	if err := dropForeignSchemaVersion(ctx, db, path); err != nil {
		return fail(db, v, needNothing, err)
	}
	return &Workspace{DB: db, To: v, lock: shared}, v, needNothing, nil
}

// dropForeignSchemaVersion deletes a metadata.schema_version row. Migration 0001 removed it, so a row
// that is back was written by a pre-runner binary (Node or an older core) that opened the migrated
// database (R3). The row is never read as a version.
func dropForeignSchemaVersion(ctx context.Context, db *sql.DB, path string) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM metadata WHERE key = 'schema_version'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM metadata WHERE key = 'schema_version'`); err != nil {
		return err
	}
	slog.Warn("a pre-runner LumenCortex binary opened this migrated database and wrote metadata.schema_version; removed it. Run the same release for every tool that uses this workspace",
		"database", path)
	return nil
}

// lockExclusive waits up to LockWait, or until ctx is done, for the exclusive schema lock on
// <stateDir>/db.lock. A shared holder that never leaves makes it fail with ErrDatabaseInUse.
func lockExclusive(ctx context.Context, stateDir string) (*runlock.Lock, error) {
	deadline := time.Now().Add(LockWait)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		lock, err := runlock.TryAcquire(stateDir, dbLockID)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, runlock.ErrHeld) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("schema lock %s: %w", filepath.Join(stateDir, dbLockID+".lock"), ErrDatabaseInUse)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelay):
		}
	}
}

func statExisting(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("no workspace database: %w", err)
	}
	return nil
}

// markerKind says what checkpoint.pending still owes.
type markerKind string

const (
	markerNone markerKind = ""
	// markerCheckpoint: only the WAL truncation is missing. Any Open under the shared lock retries it.
	markerCheckpoint markerKind = "checkpoint"
	// markerVacuum: reasoning was stripped from a database whose free space may still hold older
	// copies; a rebuild (VACUUM) and the checkpoint are owed, which needs the exclusive lock. An
	// unreadable marker counts as this kind, the conservative choice.
	markerVacuum markerKind = "vacuum"
)

// completePending retries a post-change WAL checkpoint when an earlier change left a checkpoint
// marker behind. The marker is removed only when the checkpoint reports completion. A marker that
// owes the rebuild is left to the exclusive path.
func completePending(ctx context.Context, q querier, stateDir string) error {
	kind, err := readMarker(stateDir)
	if err != nil || kind != markerCheckpoint {
		return err
	}
	return checkpointAndClearMarker(ctx, q, stateDir)
}

// checkpointAndClearMarker runs the checkpoint after a schema change and clears the marker only when
// the checkpoint completes. An incomplete checkpoint (a reader in a process that ignores db.lock)
// keeps the marker; the schema change itself still succeeded.
func checkpointAndClearMarker(ctx context.Context, q querier, stateDir string) error {
	complete, err := truncateCheckpoint(ctx, q)
	if err != nil {
		return err
	}
	if complete {
		return removeMarker(stateDir)
	}
	return nil
}

// truncateCheckpoint runs PRAGMA wal_checkpoint(TRUNCATE) and reads its result row
// (busy, log, checkpointed). The checkpoint is complete when busy == 0, so no reader blocked it,
// and log == checkpointed, so every frame in the WAL reached the main file and the WAL was
// truncated: no stripped bytes survive in it. Observed: clean database (0, 0, 0); a reader holding a
// snapshot (1, n, 0). A database that is not in WAL mode reports (0, -1, -1), which is complete.
func truncateCheckpoint(ctx context.Context, q querier) (bool, error) {
	wait := CheckpointWait.Milliseconds()
	if _, err := q.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, wait)); err != nil {
		return false, fmt.Errorf("checkpoint after schema change: %w", err)
	}
	defer q.ExecContext(context.WithoutCancel(ctx), fmt.Sprintf(`PRAGMA busy_timeout = %d`, busyTimeoutMS))
	var busy, log, checkpointed int
	if err := q.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &log, &checkpointed); err != nil {
		return false, fmt.Errorf("checkpoint after schema change: %w", err)
	}
	return busy == 0 && log == checkpointed, nil
}

// readMarker returns what <stateDir>/checkpoint.pending owes, or markerNone when it does not exist.
func readMarker(stateDir string) (markerKind, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, pendingMarker))
	if os.IsNotExist(err) {
		return markerNone, nil
	}
	if err != nil {
		return markerNone, err
	}
	if strings.TrimSpace(string(raw)) == string(markerCheckpoint) {
		return markerCheckpoint, nil
	}
	return markerVacuum, nil
}

// createMarker durably writes <stateDir>/checkpoint.pending with the given kind: the content goes to
// a temporary file that is fsynced and renamed over the marker, then the directory is fsynced. The
// replacement is atomic, so a crash never leaves a half-written marker, and the marker survives a
// crash after the first migration transaction.
func createMarker(stateDir string, kind markerKind) error {
	final := filepath.Join(stateDir, pendingMarker)
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(string(kind) + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return err
	}
	return syncDir(stateDir)
}

// removeMarker deletes <stateDir>/checkpoint.pending and fsyncs the directory.
func removeMarker(stateDir string) error {
	if err := os.Remove(filepath.Join(stateDir, pendingMarker)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(stateDir)
}
