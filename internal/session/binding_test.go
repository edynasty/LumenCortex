package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/migrate"
)

func openBound(t *testing.T, dir, workspace string) (*Store, error) {
	t.Helper()
	return OpenBound(context.Background(), dir, Binding{CanonicalPath: workspace, CoreVersion: "test"})
}

func TestOpenBoundWritesOneBindingAndRefusesAnotherWorkspace(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := openBound(t, dir, "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := readBinding(context.Background(), store.db)
	if err != nil || !found || record.CanonicalPath != "/ws/a" || record.CoreVersion != "test" || record.CreatedAt == "" {
		t.Fatalf("binding = %+v found=%v err=%v", record, found, err)
	}
	store.Close()

	again, err := openBound(t, dir, "/ws/a")
	if err != nil {
		t.Fatalf("reopen with the same workspace: %v", err)
	}
	again.Close()

	if _, err := openBound(t, dir, "/ws/b"); !errors.Is(err, ErrStateBindingMismatch) {
		t.Fatalf("open for another workspace: %v, want ErrStateBindingMismatch", err)
	}
	// The refusal released the database: the right workspace still opens.
	back, err := openBound(t, dir, "/ws/a")
	if err != nil {
		t.Fatalf("open after a refusal: %v", err)
	}
	back.Close()
}

// From schema 6 on every database this Core creates carries a binding, so one without is a
// constructed file: refused and left without a binding. Older ones are adopted.
func TestMissingBindingByCreationSchema(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	store, err := openBound(t, dir, "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	want := Binding{CanonicalPath: "/ws/a", CoreVersion: "test"}

	remove := func() {
		if _, err := store.db.Exec(`DELETE FROM metadata WHERE key = ?`, BindingKey); err != nil {
			t.Fatal(err)
		}
	}
	remove()
	if err := ensureBinding(ctx, store.db, bindingRequiredFromVersion, want); !errors.Is(err, ErrStateBindingMismatch) {
		t.Fatalf("schema %d without a binding: %v, want ErrStateBindingMismatch", bindingRequiredFromVersion, err)
	}
	if _, found, _ := readBinding(ctx, store.db); found {
		t.Error("the refusal wrote a binding")
	}
	if err := ensureBinding(ctx, store.db, bindingRequiredFromVersion-1, want); err != nil {
		t.Fatalf("schema %d without a binding: %v, want adoption", bindingRequiredFromVersion-1, err)
	}
	if record, found, _ := readBinding(ctx, store.db); !found || record.CanonicalPath != "/ws/a" {
		t.Errorf("adoption wrote %+v found=%v", record, found)
	}
	// Idempotent: a second pass neither fails nor changes the row.
	first, _, _ := readBinding(ctx, store.db)
	if err := ensureBinding(ctx, store.db, 0, want); err != nil {
		t.Fatal(err)
	}
	if second, _, _ := readBinding(ctx, store.db); second != first {
		t.Errorf("binding changed on re-verification: %+v -> %+v", first, second)
	}
	// A schema-6 database with a binding passes.
	if err := ensureBinding(ctx, store.db, bindingRequiredFromVersion, want); err != nil {
		t.Errorf("schema %d with a binding: %v", bindingRequiredFromVersion, err)
	}
}

// An open that stops after the schema reached 6 but before it returned (a crash, a cancelled
// context, a failed read) must not leave a database that every later open refuses as unbound: the
// binding is part of the transaction that creates schema 6. Both a new database and an adopted older
// one are covered.
func TestInterruptedOpenAfterMigrationStillBinds(t *testing.T) {
	ctx := context.Background()
	stop := errors.New("stopped after the migration")
	interrupt := func(t *testing.T, dir, workspace string) {
		t.Helper()
		testHookAfterMigrate = func() error { return stop }
		defer func() { testHookAfterMigrate = nil }()
		if _, err := openBound(t, dir, workspace); !errors.Is(err, stop) {
			t.Fatalf("interrupted open: %v", err)
		}
	}
	check := func(t *testing.T, dir, workspace string) {
		t.Helper()
		store, err := openBound(t, dir, workspace)
		if err != nil {
			t.Fatalf("open after the interruption: %v", err)
		}
		defer store.Close()
		if record, found, err := readBinding(ctx, store.db); err != nil || !found || record.CanonicalPath != workspace {
			t.Fatalf("binding = %+v found=%v err=%v", record, found, err)
		}
		if _, err := openBound(t, dir, "/ws/other"); !errors.Is(err, ErrStateBindingMismatch) {
			t.Fatalf("another workspace after recovery: %v, want ErrStateBindingMismatch", err)
		}
	}

	t.Run("new database", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		interrupt(t, dir, "/ws/a")
		check(t, dir, "/ws/a")
	})

	t.Run("adopted older database", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state")
		store, err := openBound(t, dir, "/ws/a")
		if err != nil {
			t.Fatal(err)
		}
		store.Close()
		set, err := migrate.Embedded()
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := migrate.Downgrade(ctx, dir, set, bindingRequiredFromVersion-1); err != nil {
			t.Fatal(err)
		}
		// A database of an earlier build: the schema before 6 and no binding row.
		db, err := sql.Open("sqlite3", filepath.Join(dir, "lumencortex.db"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`DELETE FROM metadata WHERE key = ?`, BindingKey); err != nil {
			t.Fatal(err)
		}
		db.Close()
		interrupt(t, dir, "/ws/a")
		check(t, dir, "/ws/a")
	})
}
