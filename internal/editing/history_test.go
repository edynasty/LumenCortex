package editing

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestDiffHistoryIsBounded: the diff history keeps the last revisions of a file and refuses an evicted
// one instead of approximating it; a delete is diffable against the absent file.
func TestDiffHistoryIsBounded(t *testing.T) {
	root := t.TempDir()
	engine := newTestEngine(t, root)
	ctx := context.Background()
	var revs []string
	for i := 0; i <= historyRevsPerPath; i++ {
		content := []byte(fmt.Sprintf("line %d\n", i))
		change := FileChange{Path: "f.txt", Content: &content, AllowCreate: i == 0}
		if i > 0 {
			change.BaseRev = revs[len(revs)-1]
		}
		result, err := engine.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{change}})
		if err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
		revs = append(revs, result.Files[0].NewRev)
	}
	var editErr *Error
	if _, err := engine.Diff(nil, "f.txt", revs[0], revs[1]); !errors.As(err, &editErr) || editErr.Code != CodeUnknownRev {
		t.Fatalf("evicted revision err = %v", err)
	}
	last := len(revs) - 1
	diff, err := engine.Diff(nil, "f.txt", revs[last-1], revs[last])
	if err != nil || !strings.Contains(diff, fmt.Sprintf("+line %d", last)) {
		t.Fatalf("recent diff = %q, %v", diff, err)
	}
	if _, err := engine.Commit(ctx, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{{Path: "f.txt", BaseRev: revs[last], Delete: true}}}); err != nil {
		t.Fatal(err)
	}
	deleted, err := engine.Diff(nil, "f.txt", revs[last], "")
	if err != nil || !strings.Contains(deleted, fmt.Sprintf("-line %d", last)) {
		t.Fatalf("delete diff = %q, %v", deleted, err)
	}
}

// TestEvictedRevisionIsNotRetained pins the memory bound: content evicted from a path's history becomes
// unreachable, so the bytes the global cap counts are the bytes the history keeps alive. Reslicing the
// head off (entries[1:]) left the evicted content in the backing array, uncounted.
func TestEvictedRevisionIsNotRetained(t *testing.T) {
	h := newRevisionHistory()
	h.add("f", historyEntry{rev: "first", content: make([]byte, 1<<10)})
	freed := make(chan struct{})
	// add stores its own copy; the finalizer watches that copy's allocation.
	runtime.SetFinalizer(&h.paths["f"][0].content[0], func(*byte) { close(freed) })
	for i := 0; i < historyRevsPerPath; i++ {
		h.add("f", historyEntry{rev: fmt.Sprintf("r%d", i), content: []byte(fmt.Sprintf("content %d", i))})
	}
	if _, found, _ := h.lookup("f", "first"); found {
		t.Fatal("the first revision was not evicted")
	}
	deadline := time.After(5 * time.Second)
	for {
		runtime.GC()
		select {
		case <-freed:
			runtime.KeepAlive(h)
			return
		case <-deadline:
			runtime.KeepAlive(h)
			t.Fatal("evicted content is still reachable from the history")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
