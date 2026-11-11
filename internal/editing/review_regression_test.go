package editing

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewDeletePreservesChangedFile(t *testing.T) {
	for _, when := range []string{"before", "validation", "write"} {
		t.Run(when, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a", "original")
			e := newTestEngine(t, root)
			change := func() { writeFile(t, root, "a", "new user content") }
			plan := Plan{Actor: testActor(), Changes: []FileChange{{Path: "a", Delete: true, BaseRev: RevOf([]byte("original"))}}}
			switch when {
			case "before":
				change()
			case "validation":
				plan.Validate = []Validator{func(map[string][]byte) []Diagnostic { change(); return nil }}
			case "write":
				e.beforeWrite = func(string) { change() }
			}
			if _, err := e.Commit(context.Background(), plan); err == nil {
				t.Error("delete accepted a changed revision")
			}
			if got := readFileContent(t, root, "a"); got != "new user content" {
				t.Fatalf("new content lost: %q", got)
			}
		})
	}
}

func TestReviewLeaseAcquiredDuringValidation(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a", "original")
	e := newTestEngine(t, root)
	next := []byte("writer A")
	_, err := e.Commit(context.Background(), Plan{Actor: testActor(), Changes: []FileChange{{Path: "a", Content: &next}}, Validate: []Validator{func(map[string][]byte) []Diagnostic {
		if _, err := e.Leases().Acquire(root, CanonicalPath(filepath.Join(root, "a")), "other", false, time.Minute); err != nil {
			t.Fatal(err)
		}
		return nil
	}}})
	if err == nil {
		t.Error("write admitted after other owner acquired lease")
	}
	if got := readFileContent(t, root, "a"); got != "original" {
		t.Fatalf("lease owner's file changed: %q", got)
	}
}

func TestReviewLeaseAcquisitionWaitsForCommit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a", "original")
	e := newTestEngine(t, root)
	entered, finish := make(chan struct{}), make(chan struct{})
	e.beforeWrite = func(string) { close(entered); <-finish }
	committed := make(chan error, 1)
	go func() {
		next := []byte("committed")
		_, err := e.Commit(context.Background(), Plan{Actor: testActor(), Changes: []FileChange{{Path: "a", Content: &next}}})
		committed <- err
	}()
	<-entered
	acquired := make(chan error, 1)
	go func() {
		_, err := e.Leases().Acquire(root, CanonicalPath(filepath.Join(root, "a")), "other", false, time.Minute)
		acquired <- err
	}()
	select {
	case err := <-acquired:
		close(finish)
		<-committed
		t.Fatalf("lease completed while commit was inside its mutation window: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(finish)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	if err := <-acquired; err != nil {
		t.Fatal(err)
	}
	if got := readFileContent(t, root, "a"); got != "committed" {
		t.Fatalf("commit lost: %q", got)
	}
}
