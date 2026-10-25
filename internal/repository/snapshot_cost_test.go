package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A staged rename (`git mv`) changes two paths: the source disappears and the destination appears.
// The workspace snapshot lists both ends, so a shell call that renames is seen at the new path too.
func TestWorkspaceSnapshotSeesBothEndsOfARename(t *testing.T) {
	s, root := blobWorkspace(t)
	write(t, filepath.Join(root, "a.txt"), "a\n")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "init")
	ctx := context.Background()

	pre := s.SnapshotWorkspace(ctx)
	gitIn(t, root, "mv", "a.txt", "c.txt")
	post := s.SnapshotWorkspace(ctx)
	changed, complete := ChangedPaths(pre, post)
	if fmt.Sprint(changed) != "[a.txt c.txt]" || !complete {
		t.Fatalf("changed = %v complete=%v, want both ends of the rename", changed, complete)
	}
	blobs, err := s.SnapshotFiles(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := blobs.Blob("a.txt"); b != BlobAbsent {
		t.Fatalf("a.txt blob %q, want absent", b)
	}
	if b, _ := blobs.Blob("c.txt"); len(b) != 40 {
		t.Fatalf("c.txt blob %q, want the moved content", b)
	}
}

// HEAD in a workspace snapshot is what `git rev-parse HEAD` names: empty on an unborn branch, the
// commit on a branch and on a detached HEAD.
func TestWorkspaceSnapshotHeadIsRevParseHead(t *testing.T) {
	s, root := blobWorkspace(t)
	ctx := context.Background()
	revParse := func() string {
		out, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "-q", "HEAD").Output()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	check := func(when string) {
		t.Helper()
		snap := s.SnapshotWorkspace(ctx)
		if !snap.OK || snap.Head != revParse() {
			t.Fatalf("%s: snapshot %+v, rev-parse HEAD %q", when, snap, revParse())
		}
	}
	write(t, filepath.Join(root, "a.txt"), "a\n")
	check("unborn")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "one")
	first := revParse()
	check("branch")
	write(t, filepath.Join(root, "a.txt"), "b\n")
	gitIn(t, root, "commit", "-q", "-am", "two")
	gitIn(t, root, "checkout", "-q", "--detach", first)
	check("detached")
}

// gitCallLog puts a logging git wrapper first on PATH and returns a reader of the argv it saw.
func gitCallLog(t *testing.T) func() []string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"$*\" >> %q\nexec %q \"$@\"\n", log, real)
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() []string {
		raw, err := os.ReadFile(log)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(raw)), "\n")
	}
}

// A workspace snapshot is one git process and a files snapshot is one, whatever the number of paths:
// these are the per-call costs approval section 7 budgets for shell and file tools.
func TestSnapshotsRunOneGitProcessEach(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell wrapper")
	}
	s, root := blobWorkspace(t)
	write(t, filepath.Join(root, "a.txt"), "a\n")
	write(t, filepath.Join(root, "b.txt"), "b\n")
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-q", "-m", "init")
	calls := gitCallLog(t)
	ctx := context.Background()

	if snap := s.SnapshotWorkspace(ctx); !snap.OK || snap.Head == "" {
		t.Fatalf("workspace snapshot %+v", snap)
	}
	if got := calls(); len(got) != 1 || !strings.HasPrefix(got[0], "status ") {
		t.Fatalf("workspace snapshot ran %q, want one git status", got)
	}
	if _, err := s.SnapshotFiles(ctx, []string{"a.txt", "b.txt", "missing.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := calls(); len(got) != 2 || !strings.HasPrefix(got[1], "hash-object ") {
		t.Fatalf("files snapshot ran %q, want one more git hash-object", got)
	}
}

// When PATH selects the macOS /usr/bin/git shim, snapshots run the git the shim would run, looked up
// once per DEVELOPER_DIR; any other git on PATH, another platform or a failed lookup runs "git".
func TestSnapshotGitSkipsTheMacShimLookup(t *testing.T) {
	target := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	finds := 0
	path, found := macGitShim, target
	var findErr error
	oldLook, oldFind := lookPath, findGit
	t.Cleanup(func() {
		lookPath, findGit = oldLook, oldFind
		shimMu.Lock()
		shimTarget = map[string]string{}
		shimMu.Unlock()
	})
	lookPath = func(string) (string, error) { return path, nil }
	findGit = func() (string, error) { finds++; return found, findErr }
	reset := func() {
		shimMu.Lock()
		shimTarget = map[string]string{}
		shimMu.Unlock()
		finds = 0
	}
	reset()

	if runtime.GOOS != "darwin" {
		if got := snapshotGit(); got != "git" || finds != 0 {
			t.Fatalf("%s: %q after %d lookups, want git and none", runtime.GOOS, got, finds)
		}
		return
	}
	t.Setenv("DEVELOPER_DIR", "")
	for i := 0; i < 3; i++ {
		if got := snapshotGit(); got != target {
			t.Fatalf("shim on PATH: %q, want %q", got, target)
		}
	}
	if finds != 1 {
		t.Fatalf("%d lookups for one DEVELOPER_DIR, want 1", finds)
	}
	t.Setenv("DEVELOPER_DIR", "/elsewhere")
	snapshotGit()
	if finds != 2 {
		t.Fatalf("%d lookups after DEVELOPER_DIR changed, want 2", finds)
	}

	path = "/opt/homebrew/bin/git"
	if got := snapshotGit(); got != "git" {
		t.Fatalf("other git on PATH: %q, want git", got)
	}

	path = macGitShim
	for _, bad := range []struct {
		found string
		err   error
	}{{"", errors.New("xcrun failed")}, {"relative/git", nil}, {macGitShim, nil}, {filepath.Join(t.TempDir(), "absent"), nil}} {
		reset()
		found, findErr = bad.found, bad.err
		if got := snapshotGit(); got != "git" {
			t.Fatalf("lookup %q/%v: %q, want git", bad.found, bad.err, got)
		}
	}
}
