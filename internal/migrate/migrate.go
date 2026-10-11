// Package migrate owns the workspace database schema (design README §6.1).
//
// PRAGMA user_version is the only schema version. Numbered files NNNN_name.sql in sql/ are applied in
// order, each in one transaction that also sets user_version; a gap in the numbering opens no database.
//
// Cross-process ownership: <stateDir>/db.lock is the run-lock primitive in shared mode. Every open
// Workspace holds it shared from Open to Close; changing the schema (upgrade or downgrade) holds it
// exclusive, so the schema never changes under an open process. Lock order: db.lock before any
// per-session run lock.
//
// Before any change the runner makes a versioned backup with VACUUM INTO, removes provider reasoning
// from the copy, and only then renames it into place. During the change it keeps SQLite's
// secure_delete on and a durable checkpoint.pending marker; after the last transaction it truncates
// the WAL and removes the marker only when SQLite reports the checkpoint complete.
package migrate

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// ErrSchemaTooNew means the database was migrated by a newer binary; nothing is written.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

// ErrDatabaseInUse means a lock could not be taken because another process kept db.lock for the whole
// LockWait. Opening needs it shared while a schema change runs; a schema change needs it exclusive
// while any process has the workspace open. The caller closes the other processes and retries.
var ErrDatabaseInUse = errors.New("database is open in another process; close it to change the schema")

// ErrNoDownMigration means a requested downgrade crosses a version that has no down file.
var ErrNoDownMigration = errors.New("no down migration")

// LockWait bounds how long Open and Downgrade wait for db.lock. Tests shorten it.
var LockWait = 30 * time.Second

// CheckpointWait bounds how long the post-change WAL checkpoint waits for readers in processes that
// do not respect db.lock. An incomplete checkpoint keeps checkpoint.pending for the next Open.
var CheckpointWait = time.Second

const (
	retryDelay    = 50 * time.Millisecond
	busyTimeoutMS = 5000
	dbFile        = "lumencortex.db"
	dbLockID      = "db"
)

//go:embed sql
var embedded embed.FS

type step struct {
	version int
	name    string
	sql     string
}

// Set is a validated list of numbered migrations and their down files.
type Set struct {
	up   []step
	down map[int]step
}

// Latest is the schema version this binary produces.
func (s *Set) Latest() int { return len(s.up) }

var (
	upRE   = regexp.MustCompile(`^(\d{4})_[A-Za-z0-9_]+\.sql$`)
	downRE = regexp.MustCompile(`^(\d{4})_[A-Za-z0-9_]+\.down\.sql$`)
)

// Embedded loads the migrations compiled into the binary.
func Embedded() (*Set, error) {
	sub, err := fs.Sub(embedded, "sql")
	if err != nil {
		return nil, err
	}
	return Load(sub)
}

// Load reads NNNN_name.sql from the root of fsys and NNNN_name.down.sql from its down/ directory. The
// up numbers must run 1..N without a gap; every down number must belong to an up file.
func Load(fsys fs.FS) (*Set, error) {
	ups, err := readSteps(fsys, ".", upRE)
	if err != nil {
		return nil, err
	}
	for i, m := range ups {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations not contiguous from 0001: %s", m.name)
		}
	}
	downs, err := readSteps(fsys, "down", downRE)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	set := &Set{up: ups, down: map[int]step{}}
	for _, d := range downs {
		if d.version > len(ups) {
			return nil, fmt.Errorf("down migration %s has no up migration", d.name)
		}
		if _, dup := set.down[d.version]; dup {
			return nil, fmt.Errorf("two down migrations for version %04d", d.version)
		}
		set.down[d.version] = d
	}
	return set, nil
}

func readSteps(fsys fs.FS, dir string, re *regexp.Regexp) ([]step, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	var out []step
	for _, e := range entries {
		m := re.FindStringSubmatch(e.Name())
		if e.IsDir() || m == nil {
			continue
		}
		body, err := fs.ReadFile(fsys, filepath.ToSlash(filepath.Join(dir, e.Name())))
		if err != nil {
			return nil, err
		}
		v, _ := strconv.Atoi(m[1])
		out = append(out, step{version: v, name: e.Name(), sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// dsn is the connection string core has always used, minus journal_mode: switching a database to WAL
// writes its header, which must not happen before the schema version has been accepted.
func dsn(path string) string {
	return fmt.Sprintf("file:%s?_busy_timeout=%d&_foreign_keys=on&_synchronous=NORMAL", filepath.ToSlash(path), busyTimeoutMS)
}

// openDB opens path with core's single-connection pool (one connection bounds SQLite memory and
// avoids writer contention). The returned pool has not changed the journal mode.
func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", dsn(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// querier is *sql.DB or *sql.Conn.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// userVersion reads the only schema authority.
func userVersion(ctx context.Context, q querier) (int, error) {
	var v int
	if err := q.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// useWAL puts the database in WAL mode, as core always has. It writes the header only when the file
// is not WAL yet.
func useWAL(ctx context.Context, q querier) error {
	var mode string
	return q.QueryRowContext(ctx, `PRAGMA journal_mode = WAL`).Scan(&mode)
}

// rollback aborts the current transaction under a short background-derived context: ctx may already
// be cancelled, which must not stop the ROLLBACK.
func rollback(conn *sql.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = conn.ExecContext(ctx, `ROLLBACK`)
}

// testHookAfterCommit, when non-nil, runs after a migration transaction committed and before the
// next step. Tests use it to stop a helper process mid-upgrade.
var testHookAfterCommit func(version int)

// upgradeLocked migrates the database at path to the latest version. The caller holds db.lock
// exclusively. It re-reads user_version, so a migration another process committed while this one
// waited for the lock is seen and skipped. It returns the version it started from and the version
// reached; after an error the second value is the last version that committed.
//
// A database that is already current is only touched when checkpoint.pending says an earlier run
// stopped before the rebuild that removes stripped reasoning from free space (see scrubKind).
func upgradeLocked(ctx context.Context, path, stateDir string, set *Set) (from, to int, err error) {
	db, err := openDB(path)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()

	v, err := userVersion(ctx, conn)
	if err != nil {
		return 0, 0, err
	}
	if v > set.Latest() {
		return v, v, fmt.Errorf("%w: have user_version %d, binary knows %d", ErrSchemaTooNew, v, set.Latest())
	}
	pending, err := readMarker(stateDir)
	if err != nil {
		return v, v, err
	}
	if v == set.Latest() {
		if pending == markerVacuum {
			err = finishScrub(ctx, conn, stateDir, pending)
		}
		return v, v, err
	}
	if err := useWAL(ctx, conn); err != nil {
		return v, v, err
	}
	// Zero the bytes of the rows 0001 rewrites (stored provider reasoning) instead of leaving them in
	// free space.
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return v, v, err
	}
	if err := backup(ctx, conn, stateDir, v); err != nil {
		return v, v, err
	}
	kind, err := scrubKind(ctx, conn, pending)
	if err != nil {
		return v, v, err
	}
	// The main file or WAL may hold stripped bytes until the work after the last transaction
	// completes; the marker makes every later Open retry it.
	if err := createMarker(stateDir, kind); err != nil {
		return v, v, err
	}
	cur := v
	defer func() {
		if err != nil {
			finishAfterFailure(conn, stateDir, kind)
		}
	}()
	for _, m := range set.up[v:] {
		if err := runStep(ctx, conn, m.sql, m.name, m.version); err != nil {
			return v, cur, err
		}
		cur = m.version
		if testHookAfterCommit != nil {
			testHookAfterCommit(cur)
		}
	}
	if err := finishScrub(ctx, conn, stateDir, kind); err != nil {
		return v, cur, err
	}
	return v, cur, nil
}

// scrubKind decides what must happen after the last transaction. Stripping reasoning with UPDATE and
// secure_delete zeroes the rewritten cells, but page splits in the legacy file can have left older
// copies of those rows in unallocated space of pages the UPDATE never touches (a table's root page
// after it split keeps the bytes of its former leaf content). Only a rebuild of the file removes
// those, so a database that holds reasoning is vacuumed once it is stripped. A marker left by an
// earlier run that stopped before the rebuild keeps that duty even when no reasoning is left.
func scrubKind(ctx context.Context, conn *sql.Conn, pending markerKind) (markerKind, error) {
	if pending == markerVacuum {
		return markerVacuum, nil
	}
	var tables int
	if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='session_messages'`).Scan(&tables); err != nil || tables == 0 {
		return markerCheckpoint, err
	}
	var found int
	if err := conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_messages WHERE `+reasoningPredicate+`)`).Scan(&found); err != nil {
		return markerCheckpoint, err
	}
	if found == 1 {
		return markerVacuum, nil
	}
	return markerCheckpoint, nil
}

// finishScrub completes a schema change: rebuild the file when the marker says stripped bytes may
// sit in free space, then truncate the WAL. The marker is removed only when the checkpoint is
// complete; after the rebuild it is downgraded to a plain checkpoint marker so that a blocked
// checkpoint is retried without rebuilding again.
func finishScrub(ctx context.Context, conn *sql.Conn, stateDir string, kind markerKind) error {
	if kind == markerVacuum {
		if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `VACUUM`); err != nil {
			return fmt.Errorf("rebuild database after removing reasoning: %w", err)
		}
		if err := createMarker(stateDir, markerCheckpoint); err != nil {
			return err
		}
	}
	return checkpointAndClearMarker(ctx, conn, stateDir)
}

// runStep executes one migration body and sets user_version in a single transaction.
func runStep(ctx context.Context, conn *sql.Conn, body, name string, version int) error {
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, body); err != nil {
		rollback(conn)
		return fmt.Errorf("migration %s: %w", name, err)
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
		rollback(conn)
		return err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		rollback(conn)
		return err
	}
	return nil
}

// finishAfterFailure completes the scrub after an error. A failed run can still have committed
// earlier versions whose rewritten rows are not yet rebuilt or checkpointed. Best effort: the
// original error is returned and the marker stays when the work cannot finish (a reader blocks the
// checkpoint, or the bounded wait ran out); the next Open under the exclusive lock retries it.
func finishAfterFailure(conn *sql.Conn, stateDir string, kind markerKind) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = finishScrub(ctx, conn, stateDir, kind)
}

// Downgrade undoes every migration newer than `to` with the down files, newest first, in one
// transaction, after the same exclusive db.lock and backup as an upgrade. It returns the version the
// database had and the one it has now. `to` equal to the current version changes nothing. A version
// without a down file cannot be undone: the request is refused before the backup, with the database
// untouched. A database newer than this binary is refused with ErrSchemaTooNew.
func Downgrade(ctx context.Context, stateDir string, set *Set, to int) (from, now int, err error) {
	path := filepath.Join(stateDir, dbFile)
	if err := statExisting(path); err != nil {
		return 0, 0, err
	}
	lock, err := lockExclusive(ctx, stateDir)
	if err != nil {
		return 0, 0, err
	}
	defer lock.Release()

	db, err := openDB(path)
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer conn.Close()

	v, err := userVersion(ctx, conn)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case v > set.Latest():
		return v, v, fmt.Errorf("%w: have user_version %d, binary knows %d", ErrSchemaTooNew, v, set.Latest())
	case to < 0 || to > v:
		return v, v, fmt.Errorf("cannot downgrade to version %d: database is at version %d", to, v)
	case to == v:
		return v, v, nil
	}
	for i := v; i > to; i-- {
		if _, ok := set.down[i]; !ok {
			return v, v, fmt.Errorf("%w for version %04d (%s): database stays at version %d", ErrNoDownMigration, i, set.up[i-1].name, v)
		}
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA secure_delete = ON`); err != nil {
		return v, v, err
	}
	if err := backup(ctx, conn, stateDir, v); err != nil {
		return v, v, err
	}
	if err := createMarker(stateDir, markerCheckpoint); err != nil {
		return v, v, err
	}
	defer func() {
		if err != nil {
			finishAfterFailure(conn, stateDir, markerCheckpoint)
		}
	}()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return v, v, err
	}
	for i := v; i > to; i-- {
		if _, err := conn.ExecContext(ctx, set.down[i].sql); err != nil {
			rollback(conn)
			return v, v, fmt.Errorf("down migration %s: %w", set.down[i].name, err)
		}
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, to)); err != nil {
		rollback(conn)
		return v, v, err
	}
	if _, err := conn.ExecContext(ctx, `COMMIT`); err != nil {
		rollback(conn)
		return v, v, err
	}
	if err := checkpointAndClearMarker(ctx, conn, stateDir); err != nil {
		return v, to, err
	}
	return v, to, nil
}
