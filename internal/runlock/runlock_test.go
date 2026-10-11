package runlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSameProcessExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	first, err := TryAcquire(dir, "session_a")
	if err != nil {
		t.Fatal(err)
	}
	// The lock belongs to the open file description, so a second open in the SAME
	// process (another Engine) conflicts. fcntl record locks would not.
	if second, err := TryAcquire(dir, "session_a"); !errors.Is(err, ErrHeld) {
		t.Fatalf("second TryAcquire while held: lock=%v err=%v, want ErrHeld", second, err)
	}
	other, err := TryAcquire(dir, "session_b")
	if err != nil {
		t.Fatalf("another session must be independent: %v", err)
	}
	content, _ := os.ReadFile(filepath.Join(dir, "session_a.lock"))
	if !strings.HasPrefix(string(content), fmt.Sprintf("pid=%d at=", os.Getpid())) {
		t.Fatalf("diagnostic content %q", content)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("Release must be idempotent: %v", err)
	}
	again, err := TryAcquire(dir, "session_a")
	if err != nil {
		t.Fatalf("TryAcquire after Release: %v", err)
	}
	if err := errors.Join(again.Release(), other.Release()); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidIDCreatesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	for _, id := range []string{"../x", "", strings.Repeat("a", 129), "a/b", "a.b", "a b"} {
		if lock, err := TryAcquire(dir, id); err == nil || errors.Is(err, ErrHeld) {
			t.Fatalf("id %q: lock=%v err=%v, want a validation error", id, lock, err)
		}
		if err := Remove(dir, id); err == nil {
			t.Fatalf("Remove accepted id %q", id)
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("an invalid id created the lock directory")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "x.lock")); err == nil {
		t.Fatal("path traversal created ../x.lock")
	}
}

func TestRemoveDeletesTheLockFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "runs")
	lock, err := TryAcquire(dir, "session_a")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, "session_a"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "session_a.lock")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lock file still exists: %v", err)
	}
	if err := Remove(dir, "session_a"); err != nil {
		t.Fatalf("Remove of a missing file: %v", err)
	}
}
