//go:build darwin || linux

package editing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func opusConflictCopies(t *testing.T, dir, base string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	copies := map[string]string{}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), base+".lc-conflict-") {
			b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
			if err != nil {
				t.Fatal(err)
			}
			copies[ent.Name()] = string(b)
		}
	}
	return copies
}

// TestOpusPRE02FallbackRecoveryNeverOverwritesThirdParty forces the no-exchange fallback (plain
// rename publication). A held descriptor writes to the original inode and a third process atomically
// replaces the target before the post-publication check. Recovery must keep the third party's file
// at the target and preserve the external bytes elsewhere instead of renaming over the target.
func TestOpusPRE02FallbackRecoveryNeverOverwritesThirdParty(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	orig := []byte("original-content-25bytes\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := os.OpenFile(target, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	saved := renameSwapForPublish
	renameSwapForPublish = func(_, _ string) error { return syscall.ENOTSUP }
	t.Cleanup(func() { renameSwapForPublish = saved })

	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), target, RevOf(orig))
	e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose: func(dst string) error {
		if _, err := ext.WriteAt([]byte("FOREIGN\n"), 0); err != nil {
			return err
		}
		tmp := target + ".third"
		if err := os.WriteFile(tmp, []byte("THIRD-PARTY-REPLACEMENT\n"), 0o644); err != nil {
			return err
		}
		return os.Rename(tmp, target)
	}})
	next := []byte("agent os process content\n")
	var rec CommitReceipt
	_, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
		Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "f.txt", BaseRev: RevOf(orig), Content: &next}},
	})
	if CodeOf(commitErr) != CodeConcurrentWrite || rec.Committed {
		t.Fatalf("expected concurrent write refusal, got err=%v rec=%+v", commitErr, rec)
	}
	if got, _ := os.ReadFile(target); string(got) != "THIRD-PARTY-REPLACEMENT\n" {
		t.Fatalf("third-party replacement overwritten during recovery: target=%q", got)
	}
	copies := opusConflictCopies(t, root, "f.txt")
	if len(copies) != 1 {
		t.Fatalf("external bytes not preserved exactly once: %v", copies)
	}
	for name, content := range copies {
		if content != "FOREIGN\n-content-25bytes\n" {
			t.Fatalf("conflict copy %s = %q", name, content)
		}
		if !strings.Contains(commitErr.Error(), name) {
			t.Fatalf("error does not name the preserved copy %s: %v", name, commitErr)
		}
	}
	assertNoLCXTempLeftovers(t, root)
}

// TestOpusPRE02DeleteRecoveryPreservesBytesWhenPathRecreated writes through a held descriptor after
// the quarantined delete target was unlinked, while another process recreates the path. The external
// bytes must survive in a reported conflict copy, and the recreated file must stay untouched.
func TestOpusPRE02DeleteRecoveryPreservesBytesWhenPathRecreated(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "victim.txt")
	orig := []byte("original-delete-target\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), target, RevOf(orig))
	e.SetPublishFaultHooks(PublishFaultHooks{BeforeFinalExternalCheck: func(dst string) {
		if dst != target {
			return
		}
		if _, err := ext.Write([]byte("EXTERNAL-APPEND-MUST-SURVIVE\n")); err != nil {
			t.Errorf("external append: %v", err)
		}
		if err := os.WriteFile(target, []byte("RECREATED-BY-THIRD-PARTY\n"), 0o644); err != nil {
			t.Errorf("recreate: %v", err)
		}
	}})
	var rec CommitReceipt
	_, delErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
		Actor: testActor(), Tool: "apply_patch",
		Changes: []FileChange{{Path: "victim.txt", BaseRev: RevOf(orig), Delete: true}},
	})
	if CodeOf(delErr) != CodeConcurrentWrite || rec.Committed {
		t.Fatalf("expected concurrent write refusal, got err=%v rec=%+v", delErr, rec)
	}
	if got, _ := os.ReadFile(target); string(got) != "RECREATED-BY-THIRD-PARTY\n" {
		t.Fatalf("recreated file changed: %q", got)
	}
	copies := opusConflictCopies(t, root, "victim.txt")
	if len(copies) != 1 {
		t.Fatalf("external bytes not preserved exactly once: %v (err=%v)", copies, delErr)
	}
	for name, content := range copies {
		if content != "original-delete-target\nEXTERNAL-APPEND-MUST-SURVIVE\n" {
			t.Fatalf("conflict copy %s = %q", name, content)
		}
		if !strings.Contains(delErr.Error(), name) {
			t.Fatalf("error does not name the preserved copy %s: %v", name, delErr)
		}
	}
	assertNoLCXTempLeftovers(t, root)
}

// TestOpusPRE02UncertainSwapKeepsDisplacedOriginal makes the post-swap close step fail after a held
// descriptor wrote to the displaced original. The outcome is uncertain, so the displaced original
// (holding the external bytes) must be kept and named, never deleted as private debris.
func TestOpusPRE02UncertainSwapKeepsDisplacedOriginal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "f.txt")
	orig := []byte("original-content-25bytes\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	ext, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ext.Close()
	e := newTestEngine(t, root)
	e.RegisterRead(testActor(), target, RevOf(orig))
	e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose: func(dst string) error {
		if _, err := ext.Write([]byte("EXTERNAL\n")); err != nil {
			return err
		}
		return syscall.EIO
	}})
	next := []byte("agent os process content\n")
	var rec CommitReceipt
	_, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
		Actor: testActor(), Tool: "write_file",
		Changes: []FileChange{{Path: "f.txt", BaseRev: RevOf(orig), Content: &next}},
	})
	if commitErr == nil || rec.Committed || !rec.DirtyFailure {
		t.Fatalf("expected dirty uncertain failure, got err=%v rec=%+v", commitErr, rec)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	kept := ""
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".lcx-create-") {
			b, err := os.ReadFile(filepath.Join(root, ent.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if string(b) == "original-content-25bytes\nEXTERNAL\n" {
				kept = ent.Name()
			}
		}
	}
	if kept == "" {
		t.Fatalf("displaced original with the external write was deleted: entries=%v err=%v", entries, commitErr)
	}
	if !strings.Contains(commitErr.Error(), kept) {
		t.Fatalf("error does not name the kept original %s: %v", kept, commitErr)
	}
}
