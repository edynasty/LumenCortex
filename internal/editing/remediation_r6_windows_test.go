//go:build windows

package editing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestR6WindowsExternalAppendHelper(t *testing.T) {
	target := os.Getenv("LC_R6_WIN_APPEND_TARGET")
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

// TestR6WindowsRollbackTimestampAndIdentity exercises pre-Close vs post-Close Windows file
// identity/timestamp semantics (R5-06) and verifies R5-01 external-append preservation on native
// Windows hosts (NTFS/ReFS/exFAT).
func TestR6WindowsRollbackTimestampAndIdentity(t *testing.T) {
	t.Run("capture_identity_before_and_after_close_and_clean_rollback", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "a.txt")
		if err := os.WriteFile(target, []byte("old-a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		// Observe raw handle LastWriteTime before and after Close across a tick boundary.
		f, err := os.OpenFile(target, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("probe-write\n"); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		preCloseID, err := captureFileIdentity(f)
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
		postCloseID, err := captureFileIdentity(rf)
		_ = rf.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("R5-06 Windows timestamp observation: preClose=%+v postClose=%+v equal=%v",
			preCloseID.lastWriteTime, postCloseID.lastWriteTime, preCloseID.lastWriteTime == postCloseID.lastWriteTime)

		// Reset a.txt and run multi-file rollback across tick boundary.
		if err := os.WriteFile(target, []byte("old-a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e := newTestEngine(t, root)
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b.txt" {
				time.Sleep(20 * time.Millisecond)
				_ = os.WriteFile(path, []byte("conflict-b\n"), 0o644)
			}
		}
		na := []byte("longer-new-a\n")
		nb := []byte("new-b\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &na},
				{Path: "b.txt", Content: &nb},
			},
		})
		if err == nil || CodeOf(err) != CodeConflict {
			t.Fatalf("expected CodeConflict, got %v", err)
		}
		disk, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("R5-06 rollback result: state=%s receipt=%+v disk=%q", res.CommitState, rec, string(disk))
		if rec.State != CommitStateRolledBack || string(disk) != "old-a\n" {
			t.Fatalf("Windows rollback did not cleanly restore unmodified file (R5-06 triggered?): state=%s disk=%q receipt=%+v",
				rec.State, string(disk), rec)
		}
	})

	t.Run("external_append_after_restore_write_preserved_on_windows", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b\n")
		e := newTestEngine(t, root)
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b.txt" {
				_ = os.WriteFile(path, []byte("conflict-b\n"), 0o644)
			}
		}
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterRestorePayloadWrite: func(path string) error {
				if filepath.Base(path) != "a.txt" {
					return nil
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestR6WindowsExternalAppendHelper$")
				cmd.Env = append(os.Environ(), "LC_R6_WIN_APPEND_TARGET="+path)
				out, cmdErr := cmd.CombinedOutput()
				if cmdErr != nil {
					t.Logf("helper output: %s", out)
				}
				return cmdErr
			},
		})
		na := []byte("new-a\n")
		nb := []byte("new-b\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		_, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &na},
				{Path: "b.txt", Content: &nb},
			},
		})
		if err == nil {
			t.Fatal("expected conflict")
		}
		got := readFileContent(t, root, "a.txt")
		if !strings.Contains(got, "WIN-FOREIGN\n") {
			t.Fatalf("external append lost on Windows: %q", got)
		}
		ra, ok := findReceiptFile(rec.Files, "a.txt")
		if !ok || ra.PublishState != PublishExternalModification || ra.RolledBack || ra.Verified {
			t.Fatalf("unexpected receipt on Windows: %+v", ra)
		}
	})
}
