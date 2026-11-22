package editing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

type r6FallbackProbeStager struct {
	LocalHost
	limit syscall.Rlimit
}

func (s *r6FallbackProbeStager) OpenCreateTemp(target string) (*os.File, string, error) {
	return NewCreateTempFile(filepath.Dir(target))
}

func (s *r6FallbackProbeStager) BeforeAtomicPublish(string) error {
	return syscall.Setrlimit(syscall.RLIMIT_FSIZE, &syscall.Rlimit{Cur: 8192, Max: s.limit.Max})
}

func TestR6FallbackDeletedRestorePartialWriteProbe(t *testing.T) {
	root := t.TempDir()
	original := make([]byte, 20000)
	for i := range original {
		original[i] = 'D'
	}
	if err := os.WriteFile(filepath.Join(root, "d.txt"), original, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "z.txt"), []byte("old-z\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var oldLimit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &oldLimit); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &oldLimit) }()

	oldLink, oldRename := linkForPublish, renameNoReplaceForPublish
	defer func() { linkForPublish, renameNoReplaceForPublish = oldLink, oldRename }()
	linkForPublish = func(old, new string) error {
		return &os.LinkError{Op: "link", Old: old, New: new, Err: syscall.ENOTSUP}
	}
	renameNoReplaceForPublish = func(string, string) error { return syscall.EINVAL }

	lh, err := NewLocalHost(root)
	if err != nil {
		t.Fatal(err)
	}
	host := &r6FallbackProbeStager{LocalHost: lh, limit: oldLimit}
	e, err := New(Options{Root: root, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	e.beforeWrite = func(path string) {
		if filepath.Base(path) == "z.txt" {
			if err := os.WriteFile(path, []byte("conflict-z\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	var receipt CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
	replacement := []byte("new-z\n")
	_, commitErr := e.Commit(ctx, Plan{
		Actor: Actor{Session: "r6-fallback-probe"},
		Tool:  "apply_patch",
		Changes: []FileChange{
			{Path: "d.txt", Delete: true},
			{Path: "z.txt", Content: &replacement},
		},
	})
	if commitErr == nil || !errors.Is(commitErr, syscall.EFBIG) {
		t.Fatalf("expected conflict joined with fallback EFBIG, got %v", commitErr)
	}
	got, err := os.ReadFile(filepath.Join(root, "d.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 8192 {
		t.Fatalf("fallback restore left %d bytes, want 8192-byte partial destination", len(got))
	}
	if receipt.State != CommitStatePartiallyPublished || !receipt.DirtyFailure {
		t.Fatalf("receipt = %+v", receipt)
	}
	rf, ok := findReceiptFile(receipt.Files, "d.txt")
	if !ok || rf.PublishState != PublishRollbackFailed || rf.Verified || rf.RolledBack || rf.NewSHA256 != "" {
		t.Fatalf("deleted restore receipt = %+v", rf)
	}
}
