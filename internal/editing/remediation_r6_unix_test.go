//go:build !windows

package editing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestR6PartialRLimitFSizeRollbackHelper(t *testing.T) {
	mode := os.Getenv("LC_R6_RLIMIT_MODE")
	if mode == "" {
		return
	}
	root := os.Getenv("LC_R6_RLIMIT_ROOT")
	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}

	var cur syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_FSIZE, &cur); err != nil {
		t.Fatal(err)
	}

	switch mode {
	case "existing_partial":
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "c.txt" {
				_ = os.WriteFile(path, []byte("conflict-c\n"), 0o600)
			}
		}
		e.onRollback = func(path string) error {
			if filepath.Base(path) == "a.txt" {
				lim := syscall.Rlimit{Cur: 8192, Max: cur.Max}
				return syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim)
			}
			return nil
		}
		newA := []byte("short-new-a\n")
		newC := []byte("new-c\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		_, commitErr := e.Commit(ctx, Plan{
			Actor: Actor{Session: "s-rlimit"},
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &newA},
				{Path: "c.txt", Content: &newC},
			},
		})
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &cur)
		if commitErr == nil || !errors.Is(commitErr, syscall.EFBIG) {
			t.Fatalf("expected EFBIG joined on partial restore write, got %v", commitErr)
		}
		ra, ok := findReceiptFile(rec.Files, "a.txt")
		if !ok || ra.PublishState != PublishRollbackFailed || ra.Verified || ra.RolledBack || ra.NewSHA256 != "" {
			t.Fatalf("unexpected receipt on partial EFBIG restore: %+v", ra)
		}

	case "deleted_partial":
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "z.txt" {
				_ = os.WriteFile(path, []byte("conflict-z\n"), 0o600)
			}
		}
		e.onRollback = func(path string) error {
			if filepath.Base(path) == "d.txt" {
				lim := syscall.Rlimit{Cur: 8192, Max: cur.Max}
				return syscall.Setrlimit(syscall.RLIMIT_FSIZE, &lim)
			}
			return nil
		}
		newZ := []byte("new-z\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		_, commitErr := e.Commit(ctx, Plan{
			Actor: Actor{Session: "s-rlimit-del"},
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "d.txt", Delete: true},
				{Path: "z.txt", Content: &newZ},
			},
		})
		_ = syscall.Setrlimit(syscall.RLIMIT_FSIZE, &cur)
		if commitErr == nil || !errors.Is(commitErr, syscall.EFBIG) {
			t.Fatalf("expected EFBIG joined on deleted-file restore staging, got %v", commitErr)
		}
		rd, ok := findReceiptFile(rec.Files, "d.txt")
		if !ok || rd.PublishState != PublishRollbackFailed || rd.Verified || rd.RolledBack || rd.NewSHA256 != "" {
			t.Fatalf("unexpected receipt for d.txt: %+v", rd)
		}
		// Staging in a private temp file means d.txt must NOT exist as a half-written 8192-byte file!
		if _, statErr := os.Lstat(filepath.Join(root, "d.txt")); !errors.Is(statErr, os.ErrNotExist) {
			raw, _ := os.ReadFile(filepath.Join(root, "d.txt"))
			t.Fatalf("expected no half-written d.txt at destination, got statErr=%v len=%d", statErr, len(raw))
		}
	}
}

func TestR6PartialRLimitFSizeRollback(t *testing.T) {
	t.Run("existing_20kb_restore_under_8kb_rlimit", func(t *testing.T) {
		root := t.TempDir()
		orig20k := bytes.Repeat([]byte("A"), 20000)
		if err := os.WriteFile(filepath.Join(root, "a.txt"), orig20k, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "c.txt"), []byte("old-c\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.v", "-test.run=^TestR6PartialRLimitFSizeRollbackHelper$")
		cmd.Env = append(os.Environ(), "LC_R6_RLIMIT_MODE=existing_partial", "LC_R6_RLIMIT_ROOT="+root)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("subtest failed: %v\n%s", err, out)
		}
	})

	t.Run("deleted_20kb_restore_under_8kb_rlimit_leaves_no_corrupt_destination", func(t *testing.T) {
		root := t.TempDir()
		orig20k := bytes.Repeat([]byte("D"), 20000)
		if err := os.WriteFile(filepath.Join(root, "d.txt"), orig20k, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "z.txt"), []byte("old-z\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(os.Args[0], "-test.v", "-test.run=^TestR6PartialRLimitFSizeRollbackHelper$")
		cmd.Env = append(os.Environ(), "LC_R6_RLIMIT_MODE=deleted_partial", "LC_R6_RLIMIT_ROOT="+root)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("subtest failed: %v\n%s", err, out)
		}
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, ent := range entries {
			if strings.HasPrefix(ent.Name(), ".lcx-create-") {
				t.Fatalf("leftover temp file after failed deleted-file restore: %s", ent.Name())
			}
		}
	})
}
