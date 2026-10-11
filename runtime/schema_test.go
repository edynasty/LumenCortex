package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/edynasty/LumenCortex/internal/runlock"
)

// M2.1: every Engine holds <state>/db.lock shared from Open to Close; a database a newer binary
// migrated is refused without a write.

func setUserVersion(t *testing.T, stateDir string, v int) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(stateDir, "lumencortex.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		t.Fatal(err)
	}
}

func TestEngineHoldsSchemaLockSharedUntilClose(t *testing.T) {
	workspace := t.TempDir()
	stateDir := filepath.Join(workspace, ".lumencortex")
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runlock.TryAcquire(stateDir, "db"); !errors.Is(err, runlock.ErrHeld) {
		t.Fatalf("exclusive db.lock while an Engine is open: %v, want ErrHeld", err)
	}
	// A second Engine shares the lock.
	second, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	second.Close()
	if _, err := runlock.TryAcquire(stateDir, "db"); !errors.Is(err, runlock.ErrHeld) {
		t.Fatalf("closing one of two Engines released db.lock: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	lock, err := runlock.TryAcquire(stateDir, "db")
	if err != nil {
		t.Fatalf("exclusive db.lock after every Engine closed: %v", err)
	}
	lock.Release()
}

func TestOpenRefusesNewerSchemaWithoutWriting(t *testing.T) {
	workspace := t.TempDir()
	stateDir := filepath.Join(workspace, ".lumencortex")
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	engine.Close()
	setUserVersion(t, stateDir, 9)
	dbFile := filepath.Join(stateDir, "lumencortex.db")
	before, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Open(Options{Workspace: workspace}); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("Open of a version-9 database: %v, want ErrSchemaTooNew", err)
	}
	after, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Error("the refused Open wrote to the database")
	}
	if _, err := os.Stat(dbFile + "-wal"); err == nil {
		t.Error("the refused Open left a WAL behind")
	}
	if lock, err := runlock.TryAcquire(stateDir, "db"); err != nil {
		t.Errorf("the refused Open kept db.lock: %v", err)
	} else {
		lock.Release()
	}
}

func TestOpenContextCancelsWaitForSchemaLock(t *testing.T) {
	workspace := t.TempDir()
	stateDir := filepath.Join(workspace, ".lumencortex")
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	engine.Close()

	// Another process (here: this test) holds db.lock exclusively, as a migration would.
	lock, err := runlock.TryAcquire(stateDir, "db")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := OpenContext(ctx, Options{Workspace: workspace}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("OpenContext while db.lock is exclusive: %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("waited %v, the context allowed 150ms", elapsed)
	}
}

func TestDowngradeSchemaRefusesWhileEngineIsOpen(t *testing.T) {
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, _, err := DowngradeSchema(ctx, Options{Workspace: workspace}, 0); err == nil {
		t.Fatal("DowngradeSchema succeeded while an Engine holds the workspace")
	}
}
