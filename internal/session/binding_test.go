package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
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
