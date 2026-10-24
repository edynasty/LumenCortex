package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/edynasty/LumenCortex/internal/migrate"
	"github.com/edynasty/LumenCortex/internal/runlock"
)

// BindingKey is the metadata row that ties a state database to one workspace (ADR-0016). It needs no
// numbered migration: metadata has existed since schema 1.
const BindingKey = "workspace_binding"

// bindingRequiredFromVersion is the first schema version whose databases are always created with a
// binding. A database at this version or above with no binding is not one this Core created, so it is
// refused instead of adopted. An older database without a binding predates the rule and is adopted.
const bindingRequiredFromVersion = 6

// bindLockID names <state>/bind.lock, held from before the schema migration until the binding is
// verified, so a second process never sees a migrated database whose binding is not yet written.
const bindLockID = "bind"

// ErrStateBindingMismatch reports that the state database is bound to another workspace, that its
// binding cannot be read, or that it has none although its schema always carries one.
var ErrStateBindingMismatch = errors.New("state database is not bound to this workspace")

// Binding is the workspace identity a state database must carry.
type Binding struct {
	// CanonicalPath is the workspace's canonical path (runtime.canonicalPath).
	CanonicalPath string
	// CoreVersion is recorded when the binding is first written.
	CoreVersion string
}

type bindingRecord struct {
	CanonicalPath string `json:"canonicalPath"`
	CreatedAt     string `json:"createdAt"`
	CoreVersion   string `json:"coreVersion"`
}

// OpenBound opens the state database in stateDir like OpenContext, then checks its workspace binding
// after the schema migration: a binding for another workspace returns ErrStateBindingMismatch; no
// binding is written when the database predates schema 6, and refused otherwise. The first binding is
// written and read back, under bind.lock, so two processes that open a new workspace together end up
// with one database and one binding.
func OpenBound(ctx context.Context, stateDir string, want Binding) (*Store, error) {
	if want.CanonicalPath == "" {
		return nil, errors.New("state binding needs a workspace path")
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, err
	}
	lock, err := lockBinding(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	store, err := OpenContext(ctx, stateDir)
	if err != nil {
		return nil, err
	}
	if err := ensureBinding(ctx, store.db, store.ws.From, want); err != nil {
		return nil, errors.Join(err, store.Close())
	}
	return store, nil
}

// lockBinding takes bind.lock exclusively, waiting up to migrate.LockWait.
func lockBinding(ctx context.Context, stateDir string) (*runlock.Lock, error) {
	deadline := time.Now().Add(migrate.LockWait)
	for {
		lock, err := runlock.TryAcquire(stateDir, bindLockID)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, runlock.ErrHeld) {
			return nil, err
		}
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("binding lock in %s: %w", stateDir, migrate.ErrDatabaseInUse)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ensureBinding verifies db's workspace binding against want. from is the user_version the database
// had before this open migrated it.
func ensureBinding(ctx context.Context, db *sql.DB, from int, want Binding) error {
	have, found, err := readBinding(ctx, db)
	if err != nil {
		return err
	}
	if !found {
		if from >= bindingRequiredFromVersion {
			return fmt.Errorf("%w: schema %d has no %s row, so this database was not created by LumenCortex for %s",
				ErrStateBindingMismatch, from, BindingKey, want.CanonicalPath)
		}
		record, err := json.Marshal(bindingRecord{
			CanonicalPath: want.CanonicalPath,
			CreatedAt:     time.Now().UTC().Format(time.RFC3339Nano),
			CoreVersion:   want.CoreVersion,
		})
		if err != nil {
			return err
		}
		// A row written since the read, by a process that does not take bind.lock, wins.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO metadata(key, value) VALUES(?, ?) ON CONFLICT(key) DO NOTHING`, BindingKey, string(record)); err != nil {
			return fmt.Errorf("write workspace binding: %w", err)
		}
		if have, found, err = readBinding(ctx, db); err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: the binding row did not persist", ErrStateBindingMismatch)
		}
	}
	if have.CanonicalPath != want.CanonicalPath {
		return fmt.Errorf("%w: it is bound to %s, not %s", ErrStateBindingMismatch, have.CanonicalPath, want.CanonicalPath)
	}
	return nil
}

func readBinding(ctx context.Context, db *sql.DB) (bindingRecord, bool, error) {
	var raw string
	err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = ?`, BindingKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return bindingRecord{}, false, nil
	}
	if err != nil {
		return bindingRecord{}, false, fmt.Errorf("read workspace binding: %w", err)
	}
	var record bindingRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil || record.CanonicalPath == "" {
		return bindingRecord{}, false, fmt.Errorf("%w: the %s row is unreadable", ErrStateBindingMismatch, BindingKey)
	}
	return record, true, nil
}
