//go:build !windows

package editing

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

func r7RunRlimitChild(t *testing.T, mode, root string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestR7RlimitChildHelper$")
	cmd.Env = append(os.Environ(), "LC_R7_RLIMIT_MODE="+mode, "LC_R7_RLIMIT_ROOT="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("rlimit child (%s): %v\n%s", mode, err, out)
	}
}

// TestR7RlimitChildHelper isolates RLIMIT_FSIZE and SIGXFSZ in a child process, so a file-size
// refusal cannot change the parent test process's signal disposition or limit.
func TestR7RlimitChildHelper(t *testing.T) {
	mode := os.Getenv("LC_R7_RLIMIT_MODE")
	if mode == "" {
		return
	}
	root := os.Getenv("LC_R7_RLIMIT_ROOT")
	// The child alone changes the SIGXFSZ disposition; the parent never does.
	signal.Ignore(syscall.SIGXFSZ)
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) }()

	e := newTestEngine(t, root)
	actor := testActor()
	switch mode {
	case "create_efbig":
		e.SetPublishFaultHooks(PublishFaultHooks{WriteStaged: func(_ string, f *os.File, data []byte) (int, error) {
			// A write that starts at the limit must fail with EFBIG, not a silent short file.
			lim := syscall.Rlimit{Cur: 0, Max: orig.Max}
			if err := syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim); err != nil {
				return 0, err
			}
			n, werr := f.Write(data)
			_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
			return n, werr
		}})
		content := []byte("recreated-content-that-must-not-be-partially-visible\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
			{Path: "d.txt", Content: &content, Create: true},
		}})
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
		if !errors.Is(err, syscall.EFBIG) {
			t.Fatalf("staged write at the limit: err=%v, want EFBIG", err)
		}
		r7AssertCreateCleanFailure(t, res, rec, err, PublishFailedBeforeWrite, true, false)
		r7AssertNoPartialDestination(t, root, "d.txt")
		if n := r7CountStagingLeftovers(t, root); n != 0 {
			t.Fatalf("%d staging leftovers", n)
		}
	default:
		t.Fatalf("unknown LC_R7_RLIMIT_MODE=%q", mode)
	}
}

// TestR7NonStagingHostRefusesUpdate pins the fail-closed policy for a host without private sibling
// staging: an update cannot be published atomically there, so it is refused as unsupported before
// any byte of any file changes, instead of truncating and rewriting the live file.
func TestR7NonStagingHostRefusesUpdate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "b.txt", "old-b\n")
	lh, err := NewLocalHost(root)
	if err != nil {
		t.Fatal(err)
	}
	host := &r7PlainHost{LocalHost: lh}
	e, err := New(Options{Root: root, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	actor := testActor()
	for _, name := range []string{"a.txt", "b.txt"} {
		e.RegisterRead(actor, filepath.Join(root, name), RevOf([]byte(readFileContent(t, root, name))))
	}
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Host: host, Tool: "write_file", Changes: []FileChange{
		{Path: "a.txt", Content: bytesPtr("new-a\n")},
		{Path: "b.txt", Content: bytesPtr("new-b\n")},
	}})
	if CodeOf(err) != CodeUnsupportedFilesystem {
		t.Fatalf("err=%v, want %s", err, CodeUnsupportedFilesystem)
	}
	if rec.Committed || rec.DirtyFailure {
		t.Fatalf("refusal reported a destination effect: %+v", rec)
	}
	fa, ra := r7Files(t, res, rec, "a.txt")
	if fa.PublishState != PublishUnsupported || ra.PublishState != PublishUnsupported || fa.Published || ra.Wrote ||
		fa.Verified || fa.NewRev != "" || ra.NewSHA256 != "" {
		t.Fatalf("unsupported update receipt: %+v %+v", fa, ra)
	}
	fb, rb := r7Files(t, res, rec, "b.txt")
	r7AssertNotAttempted(t, fb, rb)
	if got := readFileContent(t, root, "a.txt"); got != "old-a\n" {
		t.Fatalf("a.txt changed: %q", got)
	}
	if got := readFileContent(t, root, "b.txt"); got != "old-b\n" {
		t.Fatalf("b.txt changed: %q", got)
	}
	assertNoCreateTempLeftovers(t, root)
}

// TestR7CreateEFBIGIsolated stages a recreation through a fresh Create plan under a real kernel
// file-size refusal. The destination is never visible partially.
func TestR7CreateEFBIGIsolated(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "d.txt", "old-d\n")
	e := newTestEngine(t, root)
	if _, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "d.txt", Delete: true},
	}}); err != nil {
		t.Fatal(err)
	}
	r7RunRlimitChild(t, "create_efbig", root)
	if _, err := os.Stat(filepath.Join(root, "d.txt")); !errors.Is(err, os.ErrNotExist) {
		raw, _ := os.ReadFile(filepath.Join(root, "d.txt"))
		t.Fatalf("destination partially visible: err=%v bytes=%q", err, raw)
	}
	if n := r7CountStagingLeftovers(t, root); n != 0 {
		t.Fatalf("%d staging leftovers", n)
	}
}

// TestR7CreateRecreationUnixFaultMatrix covers ENOSPC injection, permission refusal, and a
// filesystem that supports neither hard links nor no-replace rename. Every case leaves the
// destination absent and no private staging debris.
func TestR7CreateRecreationUnixFaultMatrix(t *testing.T) {
	t.Run("enospc_injection", func(t *testing.T) {
		root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{
			WriteStaged: func(string, *os.File, []byte) (int, error) { return 0, syscall.ENOSPC },
		}, nil)
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("err = %v, want ENOSPC", err)
		}
		r7AssertCreateCleanFailure(t, res, rec, err, PublishFailedBeforeWrite, true, false)
		r7AssertNoPartialDestination(t, root, "d.txt")
		if n := r7CountStagingLeftovers(t, root); n != 0 {
			t.Fatalf("%d staging leftovers", n)
		}
	})

	t.Run("permission_refuses_staging", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("requires enforced directory permissions")
		}
		root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{}, func(root string) {
			if err := os.Chmod(root, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
		})
		if err == nil {
			t.Fatal("expected permission refusal")
		}
		r7AssertCreateCleanFailure(t, res, rec, err, PublishFailedBeforeWrite, false, false)
		r7AssertNoPartialDestination(t, root, "d.txt")
	})

	t.Run("unsupported_filesystem_refuses_create", func(t *testing.T) {
		oldLink, oldRename := linkForPublish, renameNoReplaceForPublish
		linkForPublish = func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOTSUP}
		}
		renameNoReplaceForPublish = func(string, string) error { return syscall.ENOTSUP }
		t.Cleanup(func() { linkForPublish, renameNoReplaceForPublish = oldLink, oldRename })

		root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{}, nil)
		if err == nil || CodeOf(err) != CodeUnsupportedFilesystem {
			t.Fatalf("err = %v, want unsupported_filesystem", err)
		}
		r7AssertCreateCleanFailure(t, res, rec, err, PublishUnsupported, true, false)
		r7AssertNoPartialDestination(t, root, "d.txt")
		if n := r7CountStagingLeftovers(t, root); n != 0 {
			t.Fatalf("%d staging leftovers", n)
		}
	})
}

// After the namespace move, the old private name can immediately be reused.
func TestR7ConsumedStageIsNotUnlinked(t *testing.T) {
	orig := renameNoReplaceForPublish
	t.Cleanup(func() { renameNoReplaceForPublish = orig })
	var reused string
	renameNoReplaceForPublish = func(src, dst string) error {
		if err := orig(src, dst); err != nil {
			return err
		}
		reused = src
		return os.WriteFile(src, []byte("independent file"), 0600)
	}
	_, _, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{ForceLinkFallback: func(string) error { return syscall.EPERM }}, nil)
	if errors.Is(err, errNoReplacePublishUnsupported) || CodeOf(err) == CodeUnsupportedFilesystem {
		t.Skip("no native no-replace rename")
	}
	if err != nil || !rec.Committed {
		t.Fatalf("publication: %+v %v", rec, err)
	}
	got, err := os.ReadFile(reused)
	if err != nil || string(got) != "independent file" {
		t.Fatalf("reused name: %q, %v", got, err)
	}
}

// TestR7DeletePermissionRefusalLeavesFileIntact pins a pre-effect delete refusal: no publication,
// clean failure, and the old snapshot remains usable.
func TestR7DeletePermissionRefusalLeavesFileIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permission refusal")
	}
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	e := newTestEngine(t, root)
	actor := testActor()
	e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
	e.beforeWrite = func(string) {
		if err := os.Chmod(root, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Delete: true},
	}})
	if err == nil {
		t.Fatal("expected permission refusal")
	}
	r7AssertSettledCount(t, res, rec, 1)
	if rec.DirtyFailure || rec.State != CommitStatePrePublishFailure || rec.Committed {
		t.Fatalf("delete refusal: %+v", rec)
	}
	f, r := r7Files(t, res, rec, "a.txt")
	if f.PublishState != PublishFailedBeforeWrite || f.Published || r.Wrote || f.Verified || r.NewSHA256 != "" {
		t.Fatalf("delete refusal claims effect: %+v %+v", f, r)
	}
	if got := readFileContent(t, root, "a.txt"); got != "old-a\n" {
		t.Fatalf("refused delete changed file: %q", got)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != RevOf([]byte("old-a\n")) {
		t.Fatalf("pre-effect delete refusal invalidated snapshot: %q", got)
	}
}
