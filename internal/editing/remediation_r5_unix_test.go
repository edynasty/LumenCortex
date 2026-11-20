//go:build !windows

package editing

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
)

// TestR5RollbackRLimitFSizeFailure verifies R4-02-A on POSIX using a real kernel EFBIG refusal
// during rollback: b.txt is neither silently zero-truncated nor falsely reported as
// published_verified, its SnapshotStore entry is cleared, and both the original CodeConflict and
// rollback syscall.EFBIG errors are preserved.
func TestR5RollbackRLimitFSizeFailure(t *testing.T) {
	signal.Ignore(syscall.SIGXFSZ)
	var orig syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &orig); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig) }()

	root := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		writeFile(t, root, n+".txt", "old-"+n+"\n")
	}
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), filepath.Join(root, "b.txt"), RevOf([]byte("old-b\n")))
	e.beforeWrite = func(p string) {
		if filepath.Base(p) == "c.txt" {
			_ = os.WriteFile(p, []byte("foreign-c\n"), 0o644)
		}
	}
	e.SetTestFaultHooks(nil, func(p string) error {
		switch filepath.Base(p) {
		case "b.txt":
			lim := syscall.Rlimit{Cur: 0, Max: orig.Max}
			return syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim)
		case "a.txt":
			return syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)
		}
		return nil
	})

	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
	res, err := e.Commit(ctx, Plan{
		Actor: testActor(),
		Tool:  "apply_patch",
		Changes: []FileChange{
			{Path: "a.txt", Content: &na},
			{Path: "b.txt", Content: &nb},
			{Path: "c.txt", Content: &nc},
		},
	})
	_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &orig)

	if err == nil {
		t.Fatal("expected error")
	}
	if CodeOf(err) != CodeConflict {
		t.Fatalf("expected original CodeConflict preserved, got code=%q err=%v", CodeOf(err), err)
	}
	if !errors.Is(err, syscall.EFBIG) {
		t.Fatalf("expected rollback EFBIG preserved, got err=%v", err)
	}
	if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\n" {
		t.Fatalf("a.txt = %q, want old-a\\n", gotA)
	}
	// Because writeRestoreConditional writes before shrinking, RLIMIT_FSIZE=0 refuses byte 0 without
	// zero-truncating b.txt.
	if gotB := readFileContent(t, root, "b.txt"); gotB != "new-b\n" {
		t.Fatalf("b.txt = %q, want non-truncated new-b\\n", gotB)
	}
	fb, ok := findReceiptFile(rec.Files, "b.txt")
	if !ok || fb.PublishState != PublishRollbackFailed || fb.Verified || fb.RolledBack || fb.NewSHA256 != "" || fb.Error == "" {
		t.Fatalf("b.txt receipt = %+v", fb)
	}
	if len(res.Files) != 1 || res.Files[0].Path != "b.txt" || res.Files[0].PublishState != PublishRollbackFailed || res.Files[0].NewRev != "" {
		t.Fatalf("res.Files = %+v", res.Files)
	}
	if latest := e.LatestRev(testActor(), filepath.Join(root, "b.txt")); latest != "" {
		t.Fatalf("SnapshotStore retained stale rev %q for failed-rollback b.txt", latest)
	}
}
