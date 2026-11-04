package repository

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func gitIn(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// A files snapshot names each path once with its blob, records a missing path as absent, leaves out
// a path that is not inside the workspace and says so, and its ref moves only when a blob does.
func TestSnapshotFiles(t *testing.T) {
	s, root := blobWorkspace(t)
	write(t, filepath.Join(root, "a.txt"), "one\n")
	ctx := context.Background()

	snap, err := s.SnapshotFiles(ctx, []string{"a.txt", filepath.Join(root, "a.txt"), "missing.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if !snap.Complete || len(snap.Files) != 2 {
		t.Fatalf("snapshot = %+v, want a.txt and missing.txt, complete", snap)
	}
	if blob, ok := snap.Blob("missing.txt"); !ok || blob != BlobAbsent {
		t.Fatalf("missing.txt = %q %v, want absent", blob, ok)
	}
	first, _ := snap.Blob("a.txt")
	if len(first) != 40 {
		t.Fatalf("a.txt blob = %q", first)
	}

	again, _ := s.SnapshotFiles(ctx, []string{"missing.txt", "a.txt"})
	if again.Ref() != snap.Ref() {
		t.Fatalf("same state, different refs: %s %s", snap.Ref(), again.Ref())
	}
	write(t, filepath.Join(root, "a.txt"), "two\n")
	changed, _ := s.SnapshotFiles(ctx, []string{"a.txt", "missing.txt"})
	if changed.Ref() == snap.Ref() {
		t.Fatal("a changed blob kept the ref")
	}

	outside, _ := s.SnapshotFiles(ctx, []string{"../x", "a.txt"})
	if outside.Complete || len(outside.Files) != 1 {
		t.Fatalf("escaping path: %+v, want it left out and the snapshot incomplete", outside)
	}
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	dir, _ := s.SnapshotFiles(ctx, []string{"dir"})
	if dir.Complete || len(dir.Files) != 1 || dir.Files[0].Blob != "" {
		t.Fatalf("directory: %+v, want an unknown blob and incomplete", dir)
	}
}

// The workspace snapshot sees a modified tracked file, a new untracked file and a deletion; it does
// not see an ignored file; a commit in between moves HEAD and makes the change list partial.
func TestWorkspaceSnapshotChanges(t *testing.T) {
	s, root := blobWorkspace(t)
	write(t, filepath.Join(root, "tracked.txt"), "v1\n")
	write(t, filepath.Join(root, "gone.txt"), "x\n")
	write(t, filepath.Join(root, ".gitignore"), "ignored.log\n")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "init")
	ctx := context.Background()

	pre := s.SnapshotWorkspace(ctx)
	if !pre.OK || !pre.Complete || pre.Head == "" {
		t.Fatalf("pre = %+v", pre)
	}
	write(t, filepath.Join(root, "tracked.txt"), "v2 longer\n")
	write(t, filepath.Join(root, "new.txt"), "n\n")
	write(t, filepath.Join(root, "ignored.log"), "i\n")
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	post := s.SnapshotWorkspace(ctx)
	changed, complete := ChangedPaths(pre, post)
	if fmt.Sprint(changed) != "[gone.txt new.txt tracked.txt]" || !complete {
		t.Fatalf("changed = %v complete=%v", changed, complete)
	}
	if pre.Ref() == post.Ref() {
		t.Fatal("refs equal across a change")
	}

	// A second write to an already-dirty file is seen through its size and modification time.
	time.Sleep(10 * time.Millisecond)
	write(t, filepath.Join(root, "tracked.txt"), "v3 even longer\n")
	third := s.SnapshotWorkspace(ctx)
	if changed, _ := ChangedPaths(post, third); fmt.Sprint(changed) != "[tracked.txt]" {
		t.Fatalf("rewrite of a dirty file: %v", changed)
	}

	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "second")
	committed := s.SnapshotWorkspace(ctx)
	if _, complete := ChangedPaths(third, committed); complete {
		t.Fatal("a moved HEAD must make the change list partial")
	}
}

// Outside a git work tree there is no workspace snapshot, and no change list.
func TestWorkspaceSnapshotOutsideGit(t *testing.T) {
	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	snap := s.SnapshotWorkspace(context.Background())
	if snap.OK || snap.Ref() != "" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if changed, complete := ChangedPaths(snap, snap); changed != nil || complete {
		t.Fatalf("changed = %v complete=%v", changed, complete)
	}
}

// Snapshots never write the repository: they work while another git command holds index.lock, they
// do not create index.lock, and they leave the index file untouched even when its stat cache is stale.
func TestSnapshotsDoNotTakeTheIndexLock(t *testing.T) {
	s, root := blobWorkspace(t)
	write(t, filepath.Join(root, "a.txt"), "a\n")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "init")
	// Backdate the tracked file without changing it: its stat no longer matches the index, so a plain
	// `git status` would refresh and rewrite the index (checked by hand: it does without
	// GIT_OPTIONAL_LOCKS=0).
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), past, past); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	index := filepath.Join(root, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	lock := filepath.Join(root, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	snap := s.SnapshotWorkspace(ctx)
	files, err := s.SnapshotFiles(ctx, []string{"a.txt"})
	if !snap.OK || err != nil || !files.Complete {
		t.Fatalf("with index.lock held: workspace=%+v files=%+v err=%v", snap, files, err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}

	_ = s.SnapshotWorkspace(ctx)
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("snapshot left index.lock: %v", err)
	}
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("index rewritten: %v/%d -> %v/%d", before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}
}
