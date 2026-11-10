package editing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Four agents edit different lines of one file from the same snapshot: no update is lost, and every
// edit that was refused was refused explicitly.
func TestConcurrentLineEditsLoseNoUpdate(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for line := 1; line <= 40; line++ {
		fmt.Fprintf(&b, "line %02d\n", line)
	}
	writeFile(t, root, "f.txt", b.String())
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "f.txt")

	targets := []int{5, 15, 25, 35}
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, line := range targets {
		wg.Add(1)
		go func(i, line int) {
			defer wg.Done()
			ops, err := ParseOps(snap, []AnchorOp{{Op: OpReplace, Start: anchorAt(snap.Doc, line), Text: fmt.Sprintf("EDIT-%02d", line)}})
			if err != nil {
				errs[i] = err
				return
			}
			_, errs[i] = commitLineOps(t, engine, "f.txt", snap.Rev, ops)
		}(i, line)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("edit %d was refused: %v", targets[i], err)
		}
	}
	got := readFileContent(t, root, "f.txt")
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 40 {
		t.Fatalf("line count changed: %d", len(lines))
	}
	for _, target := range targets {
		want := fmt.Sprintf("EDIT-%02d", target)
		if lines[target-1] != want {
			t.Fatalf("line %d = %q, want %q (lost update)", target, lines[target-1], want)
		}
	}
	for _, line := range []int{1, 4, 6, 39, 40} {
		if strings.HasPrefix(lines[line-1], "EDIT-") {
			t.Fatalf("unrelated line %d was rewritten: %q", line, lines[line-1])
		}
	}
}

// A large file's anchor and edit are bounded: the edit is a line splice, not a re-render of the file.
func TestLargeFileAnchorEditIsBounded(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	const total = 50_000
	for line := 1; line <= total; line++ {
		fmt.Fprintf(&b, "func generated%06d() {}\n", line)
	}
	writeFile(t, root, "big.go", b.String())
	engine := newTestEngine(t, root)
	started := time.Now()
	snap := readSnapshot(t, engine, "big.go")
	if got := len(snap.Doc.Lines); got != total {
		t.Fatalf("lines = %d", got)
	}
	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 25_000), Text: "func generated025000() { panic(1) }"})
	if _, err := commitLineOps(t, engine, "big.go", snap.Rev, ops); err != nil {
		t.Fatalf("commit: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed > 5*time.Second {
		t.Fatalf("read+edit of %d lines took %s", total, elapsed)
	}
	got := readFileContent(t, root, "big.go")
	lines := strings.Split(got, "\n")
	if lines[24_999] != "func generated025000() { panic(1) }" {
		t.Fatalf("line 25000 = %q", lines[24_999])
	}
	if lines[24_998] != "func generated024999() {}" || lines[25_000] != "func generated025001() {}" {
		t.Fatal("neighbouring lines changed")
	}
}

// A write lease held by another holder refuses the write with a lease_conflict that names the holder;
// releasing it restores the normal path.
func TestWriteLeaseRefusesOtherHolder(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	engine := newTestEngine(t, root)
	target := canonicalPath(filepath.Join(root, "a.txt"))
	lease, err := engine.Leases().Acquire(root, target, "other-agent", false, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Covers(target) || lease.Holder != "other-agent" {
		t.Fatalf("lease = %+v", lease)
	}
	content := []byte("beta\n")
	_, err = engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "a.txt", Content: &content}}})
	editErr, ok := AsError(err)
	if !ok || editErr.Code != CodeLeaseConflict || editErr.Holder != "other-agent" {
		t.Fatalf("err = %v, want lease_conflict held by other-agent", err)
	}
	if got := readFileContent(t, root, "a.txt"); got != "alpha\n" {
		t.Fatalf("a leased file was written: %q", got)
	}
	engine.Leases().Release(target, "other-agent")
	if _, err := engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "a.txt", Content: &content}}}); err != nil {
		t.Fatalf("commit after release: %v", err)
	}
	if got := readFileContent(t, root, "a.txt"); got != "beta\n" {
		t.Fatalf("content = %q", got)
	}
}

// A lease covers a directory prefix and expires, so a holder that dies cannot lock a path forever.
func TestWriteLeasePrefixAndExpiry(t *testing.T) {
	store := NewLeaseStore()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	root := t.TempDir()
	if _, err := store.Acquire(root, filepath.Join(root, "pkg"), "a", true, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := store.Check("b", filepath.Join(root, "pkg", "x.go")); err == nil {
		t.Fatal("a prefix lease did not cover a file below it")
	}
	if err := store.Check("a", filepath.Join(root, "pkg", "x.go")); err != nil {
		t.Fatalf("the holder was refused its own lease: %v", err)
	}
	if _, err := store.Acquire(root, filepath.Join(root, "pkg"), "b", true, time.Minute); err == nil {
		t.Fatal("a second holder acquired an overlapping lease")
	}
	now = now.Add(2 * time.Minute)
	if err := store.Check("b", filepath.Join(root, "pkg", "x.go")); err != nil {
		t.Fatalf("an expired lease still refused a write: %v", err)
	}
}

// A failure while writing the second file restores the first: a plan is all or nothing.
func TestMultiFileCommitRollsBackOnWriteFailure(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "sub/b.txt", "beta\n")
	engine := newTestEngine(t, root)
	second := filepath.Join(root, "sub", "b.txt")
	engine.beforeWrite = func(path string) {
		if path == second {
			_ = os.RemoveAll(filepath.Join(root, "sub"))
		}
	}
	first := []byte("alpha 2\n")
	next := []byte("beta 2\n")
	_, err := engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &first},
		{Path: "sub/b.txt", Content: &next},
	}})
	if err == nil {
		t.Fatal("expected the second write to fail")
	}
	if got := readFileContent(t, root, "a.txt"); got != "alpha\n" {
		t.Fatalf("the first file was not rolled back: %q", got)
	}
}

// Two files commit together and both report their new revisions.
func TestMultiFileCommitIsAtomic(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha\n")
	writeFile(t, root, "b.txt", "beta\n")
	engine := newTestEngine(t, root)
	first := []byte("alpha 2\n")
	second := []byte("beta 2\n")
	result, err := engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &first},
		{Path: "b.txt", Content: &second},
	}})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(result.Files) != 2 {
		t.Fatalf("result = %+v", result)
	}
	for _, file := range result.Files {
		if file.NewRev == file.OldRev || file.NewRev != RevOf([]byte(map[string]string{"a.txt": "alpha 2\n", "b.txt": "beta 2\n"}[file.Path])) {
			t.Fatalf("file result = %+v", file)
		}
	}
	if readFileContent(t, root, "a.txt") != "alpha 2\n" || readFileContent(t, root, "b.txt") != "beta 2\n" {
		t.Fatal("both files must be written")
	}
}

// Creating and deleting are the two other shapes of a change; a create over an existing file and a
// delete of a missing one are refused.
func TestCreateAndDeleteChanges(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "gone.txt", "bye\n")
	engine := newTestEngine(t, root)
	fresh := []byte("hello\n")
	result, err := engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
		{Path: "new.txt", Content: &fresh, AllowCreate: true},
		{Path: "gone.txt", Delete: true},
	}})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if len(result.Files) != 2 {
		t.Fatalf("result = %+v", result)
	}
	byPath := map[string]FileResult{}
	for _, file := range result.Files {
		byPath[file.Path] = file
	}
	if !byPath["new.txt"].Created || !byPath["gone.txt"].Deleted {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Fatal("the file was not deleted")
	}
	if readFileContent(t, root, "new.txt") != "hello\n" {
		t.Fatal("the file was not created")
	}
	_, err = engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "new.txt", Content: &fresh, Create: true},
	}})
	if code := CodeOf(err); code != CodeExists {
		t.Fatalf("err = %v, want exists", err)
	}
	_, err = engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "missing.txt", Delete: true},
	}})
	if code := CodeOf(err); code != CodeNotFound {
		t.Fatalf("err = %v, want not_found", err)
	}
}

// The snapshot store is bounded per session and keeps several revisions of one path.
func TestSnapshotStoreIsBounded(t *testing.T) {
	store := NewSnapshotStore()
	doc := DecodeDoc([]byte("a\nb\n"))
	for i := 0; i < 100; i++ {
		store.Register("s", filepath.Join("/w", fmt.Sprintf("f%03d.go", i)), doc, 1, 2)
	}
	store.mu.Lock()
	entries := len(store.sessions["s"].entries)
	store.mu.Unlock()
	if entries > snapshotFilesPerSession*snapshotRevsPerPath {
		t.Fatalf("store kept %d entries", entries)
	}
	// Two revisions of one path both stay resolvable for a while.
	store.Reset()
	first := DecodeDoc([]byte("a\n"))
	second := DecodeDoc([]byte("a\nb\n"))
	store.Register("s", "/w/x.go", first, 1, 1)
	store.Register("s", "/w/x.go", second, 1, 2)
	if _, ok := store.Lookup("s", "/w/x.go", first.Rev); !ok {
		t.Fatal("the older revision was dropped immediately")
	}
	if snap, ok := store.Latest("s", "/w/x.go"); !ok || snap.Rev != second.Rev {
		t.Fatal("latest is not the most recent registration")
	}
}

// A file changed beyond the relocation budget is refused instead of guessed at.
func TestRelocationRefusesBeyondBudget(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for line := 1; line <= 600; line++ {
		fmt.Fprintf(&b, "original %d\n", line)
	}
	writeFile(t, root, "f.txt", b.String())
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "f.txt")
	var next strings.Builder
	for line := 1; line <= 600; line++ {
		fmt.Fprintf(&next, "rewritten %d\n", line)
	}
	writeFile(t, root, "f.txt", next.String())
	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 300), Text: "x"})
	_, err := commitLineOps(t, engine, "f.txt", snap.Rev, ops)
	if code := CodeOf(err); code != CodeStale {
		t.Fatalf("err = %v, want stale", err)
	}
}
