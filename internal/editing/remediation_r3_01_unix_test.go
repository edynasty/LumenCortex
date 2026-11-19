//go:build !windows

package editing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestR301CreateWithoutNoReplacePublishIsRefusedClearly pins R3-01: on a filesystem that supports
// neither hard links nor no-replace rename (exFAT on macOS answers both with ENOTSUP), a create is
// refused with unsupported_filesystem, nothing is published or left behind, the receipt is clean, and
// edits to existing files keep working. It swaps package-level primitives, so it must not run in
// parallel.
func TestR301CreateWithoutNoReplacePublishIsRefusedClearly(t *testing.T) {
	for _, renameErr := range []syscall.Errno{syscall.ENOTSUP, syscall.EINVAL, syscall.ENOSYS} {
		t.Run(renameErr.Error(), func(t *testing.T) {
			linkForPublish = func(oldname, newname string) error {
				return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.ENOTSUP}
			}
			renameNoReplaceForPublish = func(string, string) error { return renameErr }
			t.Cleanup(func() { linkForPublish, renameNoReplaceForPublish = os.Link, renameNoReplacePlatform })

			root := t.TempDir()
			writeFile(t, root, "keep.txt", "old\n")
			engine := newTestEngine(t, root)
			var receipts []CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipts = append(receipts, r) })

			fresh := []byte("hello\n")
			_, err := engine.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
				{Path: "new.txt", Content: &fresh, AllowCreate: true},
			}})
			editErr, ok := AsError(err)
			if !ok || editErr.Code != CodeUnsupportedFilesystem || editErr.Path != "new.txt" {
				t.Fatalf("create err = %v, want unsupported_filesystem for new.txt", err)
			}
			if !strings.Contains(err.Error(), "nothing was written") || editErr.Hint() == "" {
				t.Fatalf("refusal is not actionable: %v", err)
			}
			if len(receipts) == 0 || receipts[len(receipts)-1].Committed || receipts[len(receipts)-1].DirtyFailure {
				t.Fatalf("receipts = %+v, want a clean non-committed receipt", receipts)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "keep.txt" {
				var names []string
				for _, e := range entries {
					names = append(names, e.Name())
				}
				t.Fatalf("workspace after refused create = %v, want only keep.txt", names)
			}

			updated := []byte("new content\n")
			if _, err := engine.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
				{Path: "keep.txt", Content: &updated, AllowCreate: true},
			}}); err != nil {
				t.Fatalf("edit of an existing file: %v", err)
			}
			if got := readFileContent(t, root, "keep.txt"); got != "new content\n" {
				t.Fatalf("keep.txt = %q", got)
			}
			if _, err := os.Stat(filepath.Join(root, "new.txt")); !os.IsNotExist(err) {
				t.Fatalf("new.txt exists after the refused create: %v", err)
			}
		})
	}
}
