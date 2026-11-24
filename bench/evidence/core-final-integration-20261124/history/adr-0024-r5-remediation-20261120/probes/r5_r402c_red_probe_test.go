//go:build !windows

package editing

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestR5RedR402CCloseFailOverwritesForeignData reproduces R4-02-C on 0b3b8e72 (via Go -overlay
// injecting a deterministic f.Close() EIO hook right at engine.go:1194):
// A and B exist; both writes verify; B.Close() returns EIO after an external writer appends
// "FOREIGN\n" to B; R4's rollback blindly calls writeRestore on B, destroying "FOREIGN\n".
func TestR5RedR402CCloseFailOverwritesForeignData(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "b.txt", "old-b\n")
	e := newTestEngine(t, root)
	r5RedCloseHook = func(p string) error {
		if filepath.Base(p) == "b.txt" {
			f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			_, _ = f.Write([]byte("FOREIGN\n"))
			_ = f.Close()
			return syscall.EIO
		}
		return nil
	}
	defer func() { r5RedCloseHook = nil }()

	a, b := []byte("new-a\n"), []byte("new-b\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	_, err := e.Commit(ctx, Plan{
		Actor: testActor(),
		Tool:  "apply_patch",
		Changes: []FileChange{
			{Path: "a.txt", Content: &a},
			{Path: "b.txt", Content: &b},
		},
	})
	if err == nil {
		t.Fatal("expected EIO close error")
	}
	gotA := readFileContent(t, root, "a.txt")
	gotB := readFileContent(t, root, "b.txt")
	t.Logf("err=%v diskA=%q diskB=%q receipt=%+v", err, gotA, gotB, rec)
	if gotB != "new-b\nFOREIGN\n" {
		t.Errorf("R4-02-C DATA LOSS DEFECT: rollback overwrote external append on b.txt! got=%q want=%q", gotB, "new-b\nFOREIGN\n")
	}
}
