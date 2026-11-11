package editing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestReviewSnapshotBudget(t *testing.T) {
	for _, samePath := range []bool{true, false} {
		s := NewSnapshotStore()
		for i := 0; i < 24; i++ {
			path := "/w/a"
			if !samePath {
				path = fmt.Sprintf("/w/%d", i)
			}
			s.Register("a", path, DecodeDoc([]byte(fmt.Sprintf("%02d", i)+strings.Repeat("x", (3<<20)-2))), 1, 1)
			ss := s.sessions["a"]
			actual := 0
			for _, entry := range ss.entries {
				actual += len(entry.Doc.Raw)
			}
			if actual != ss.bytes || actual > snapshotBytesPerSession {
				t.Fatalf("samePath=%v iteration=%d accounted=%d actual=%d limit=%d", samePath, i, ss.bytes, actual, snapshotBytesPerSession)
			}
			for _, entry := range ss.entries[len(ss.entries):cap(ss.entries)] {
				if entry != nil {
					t.Fatal("evicted snapshot retained in backing array")
				}
			}
		}
	}
}

func TestReviewEvictedReadRequiresFreshRevision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a", "old")
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), filepath.Join(root, "a"), RevOf([]byte("old")))
	for i := 0; i < 65; i++ {
		e.RegisterRead(testActor(), filepath.Join(root, fmt.Sprintf("f%d", i)), "known")
	}
	writeFile(t, root, "a", "new user content")
	next := []byte("from old")
	plan := Plan{Actor: testActor(), Changes: []FileChange{{Path: "a", Content: &next, RejectStaleRead: true}}}
	if _, err := e.Commit(context.Background(), plan); err == nil {
		t.Error("evicted stale read silently accepted")
	}
	if got := readFileContent(t, root, "a"); got != "new user content" {
		t.Fatalf("new content lost: %q", got)
	}
	e.RegisterRead(testActor(), filepath.Join(root, "a"), RevOf([]byte("new user content")))
	if _, err := e.Commit(context.Background(), plan); err != nil {
		t.Fatalf("fresh reread must unblock write: %v", err)
	}
}

func TestReviewCancelledValidationDoesNotMutate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a", "original")
	e := newTestEngine(t, root)
	next := []byte("cancelled")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := e.Commit(ctx, Plan{Actor: testActor(), Changes: []FileChange{{Path: "a", Content: &next}}, Validate: []Validator{func(map[string][]byte) []Diagnostic { cancel(); return nil }}})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want cancellation, got %v", err)
	}
	if got := readFileContent(t, root, "a"); got != "original" {
		t.Fatalf("cancelled commit mutated file: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); err != nil {
		t.Fatal(err)
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
