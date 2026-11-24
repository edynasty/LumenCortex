//go:build windows

package editing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestR7WindowsExternalAppendHelper(t *testing.T) {
	target := os.Getenv("LC_R7_WIN_APPEND_TARGET")
	if target == "" {
		return
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("WIN-FOREIGN\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestR7WindowsNativeTimestampAndNoCompensation is native-ready for a Windows host (NTFS/ReFS/
// exFAT). It observes post-close identity/timestamps, asserts that an update of an existing file is
// refused (Windows has no atomic exchange primitive; OPUS-V-01), and asserts a published prefix of
// new files is never compensated after a later refusal. It has not been executed natively and
// must be run on Windows before any Windows PASS is claimed.
func TestR7WindowsNativeTimestampAndNoCompensation(t *testing.T) {
	t.Run("timestamp_observation_and_update_refused", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "a.txt")
		if err := os.WriteFile(target, []byte("old-a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(target, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("probe-write\n"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		preClose, err := captureFileIdentity(f)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		rf, err := os.Open(target)
		if err != nil {
			t.Fatal(err)
		}
		postClose, err := captureFileIdentity(rf)
		_ = rf.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Windows timestamp observation: preClose=%+v postClose=%+v equal=%v",
			preClose.lastWriteTime, postClose.lastWriteTime, preClose.lastWriteTime == postClose.lastWriteTime)

		if err := os.WriteFile(target, []byte("old-a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		e := newTestEngine(t, root)
		next := []byte("new-a\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
			{Path: "a.txt", Content: &next},
		}})
		assertExistingFileUpdateRefused(t, root, "a.txt", before, "old-a\n", err, res, rec)
	})

	t.Run("published_prefix_not_compensated_after_later_refusal", func(t *testing.T) {
		// The prefix consists of new files: Windows publishes them by no-replace MoveFileExW, while
		// updates of existing files are refused.
		root := t.TempDir()
		writeFile(t, root, "c.txt", "old-c\n")
		e := newTestEngine(t, root)
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b.txt" {
				cmd := exec.Command(os.Args[0], "-test.run=^TestR7WindowsExternalAppendHelper$")
				cmd.Env = append(os.Environ(), "LC_R7_WIN_APPEND_TARGET="+filepath.Join(root, "a.txt"))
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("append helper: %v\n%s", err, out)
				}
				if err := os.WriteFile(path, []byte("child-b\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
			{Path: "a.txt", Content: &na, Create: true},
			{Path: "b.txt", Content: &nb, Create: true},
			{Path: "c.txt", Content: &nc},
		}})
		if err == nil || CodeOf(err) != CodeConflict {
			t.Fatalf("want B conflict, got %v", err)
		}
		if res.CommitState != CommitStatePartiallyPublished || rec.State != CommitStatePartiallyPublished || !rec.DirtyFailure || rec.Committed {
			t.Fatalf("state: result=%+v receipt=%+v", res, rec)
		}
		if got := readFileContent(t, root, "a.txt"); got != "new-a\nWIN-FOREIGN\n" {
			t.Fatalf("external append lost or prefix compensated on Windows: %q", got)
		}
		fa, ra := r7Files(t, res, rec, "a.txt")
		if fa.PublishState != PublishExternalModification || !fa.Published || fa.Verified || fa.RolledBack || ra.RolledBack || ra.NewSHA256 != "" {
			t.Fatalf("Windows prefix after external append: %+v %+v", fa, ra)
		}
		if got := readFileContent(t, root, "b.txt"); got != "child-b\n" {
			t.Fatalf("foreign B lost: %q", got)
		}
		fc, rc := r7Files(t, res, rec, "c.txt")
		r7AssertNotAttempted(t, fc, rc)
		if got := readFileContent(t, root, "c.txt"); got != "old-c\n" {
			t.Fatalf("C attempted: %q", got)
		}
	})
}
