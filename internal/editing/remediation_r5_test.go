package editing

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestR5ExternalProcessWriterHelper is a self-exec child-process helper for a real second PID.
func TestR5ExternalProcessWriterHelper(t *testing.T) {
	mode := os.Getenv("LC_R5_HELPER_MODE")
	target := os.Getenv("LC_R5_HELPER_TARGET")
	payload := os.Getenv("LC_R5_HELPER_PAYLOAD")
	if mode == "" || target == "" {
		return
	}
	switch mode {
	case "append":
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("child append open: %v", err)
		}
		if _, err := f.WriteString(payload); err != nil {
			_ = f.Close()
			t.Fatalf("child append write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("child append close: %v", err)
		}
	case "overwrite_inplace":
		f, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("child overwrite open: %v", err)
		}
		if _, err := f.WriteAt([]byte(payload), 0); err != nil {
			_ = f.Close()
			t.Fatalf("child overwrite write: %v", err)
		}
		if err := f.Truncate(int64(len(payload))); err != nil {
			_ = f.Close()
			t.Fatalf("child overwrite truncate: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("child overwrite close: %v", err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func runExternalWriterProcess(t *testing.T, mode, target, payload string) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestR5ExternalProcessWriterHelper$")
	cmd.Env = append(os.Environ(),
		"LC_R5_HELPER_MODE="+mode,
		"LC_R5_HELPER_TARGET="+target,
		"LC_R5_HELPER_PAYLOAD="+payload,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(string(out) + ": " + err.Error())
	}
	return nil
}

func findReceiptFile(files []FileCommitReceipt, rel string) (FileCommitReceipt, bool) {
	for _, f := range files {
		if f.Path == rel {
			return f, true
		}
	}
	return FileCommitReceipt{}, false
}

func findResultFile(files []FileResult, rel string) (FileResult, bool) {
	for _, f := range files {
		if f.Path == rel {
			return f, true
		}
	}
	return FileResult{}, false
}

// TestR7SameInodeExternalOverwriteIsExternal covers a second PID rewriting a published prefix in
// place (same inode, different bytes) before a later path fails. Bytes and identity both matter;
// the child's bytes survive and the prefix loses its trusted revision and hash.
func TestR7SameInodeExternalOverwriteIsExternal(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "b.txt", "old-b\n")
	writeFile(t, root, "c.txt", "old-c\n")
	e := newTestEngine(t, root)
	actor := testActor()
	e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
	e.beforeWrite = func(path string) {
		if filepath.Base(path) == "b.txt" {
			if err := runExternalWriterProcess(t, "overwrite_inplace", filepath.Join(root, "a.txt"), "CHILD-REWRITE\n"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("child-b\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &na},
		{Path: "b.txt", Content: &nb},
		{Path: "c.txt", Content: &nc},
	}})
	if err == nil || CodeOf(err) != CodeConflict {
		t.Fatalf("want B conflict, got %v", err)
	}
	r7AssertSettledCount(t, res, rec, 3)
	if res.CommitState != CommitStatePartiallyPublished || !rec.DirtyFailure {
		t.Fatalf("state: %+v %+v", res, rec)
	}
	fa, ra := r7Files(t, res, rec, "a.txt")
	if fa.PublishState != PublishExternalModification || !fa.Published || !ra.Wrote || fa.Verified || fa.NewRev != "" || ra.NewSHA256 != "" {
		t.Fatalf("same-inode external write trusted: %+v %+v", fa, ra)
	}
	if got := readFileContent(t, root, "a.txt"); got != "CHILD-REWRITE\n" {
		t.Fatalf("child bytes lost: %q", got)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
		t.Fatalf("snapshot not invalidated: %q", got)
	}
	fb, rb := r7Files(t, res, rec, "b.txt")
	if fb.PublishState != PublishExternalModification || fb.Published || rb.Wrote || rb.NewSHA256 != "" {
		t.Fatalf("B: %+v %+v", fb, rb)
	}
	fc, rc := r7Files(t, res, rec, "c.txt")
	r7AssertNotAttempted(t, fc, rc)
}
