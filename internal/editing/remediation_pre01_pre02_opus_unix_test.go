//go:build darwin || linux

package editing

import (
	"context"
	"errors"
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

// TestOpusPRE01HardLinkedUpdateRefusedBeforeAnyWrite pins the fail-closed hard-link policy. An update
// of a file with several links (across directories, one outside the workspace) is refused in the
// plan preflight, so a batch that also changes an ordinary file writes nothing; a link that appears
// after the preflight is refused at publication. Every alias keeps the shared inode and bytes.
// Deleting one name of a hard-linked file stays supported and leaves the other names intact.
func TestOpusPRE01HardLinkedUpdateRefusedBeforeAnyWrite(t *testing.T) {
	setup := func(t *testing.T) (base, root string, aliases []string) {
		t.Helper()
		var err error
		base, err = filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(base, "ws")
		for _, d := range []string{"a", "b", "c"} {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("plain-v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		aliases = []string{filepath.Join(root, "a", "x.txt"), filepath.Join(root, "b", "y.txt"),
			filepath.Join(root, "c", "z.txt"), filepath.Join(base, "outside.txt")}
		if err := os.WriteFile(aliases[0], []byte("shared-v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, l := range aliases[1:] {
			if err := os.Link(aliases[0], l); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
		}
		return base, root, aliases
	}
	assertAliasesIntact := func(t *testing.T, aliases []string, want string) {
		t.Helper()
		first, err := os.Stat(aliases[0])
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range aliases {
			info, err := os.Stat(l)
			if err != nil {
				t.Fatal(err)
			}
			if b, _ := os.ReadFile(l); string(b) != want || !os.SameFile(first, info) {
				t.Fatalf("alias %s = %q (same inode %v), want %q", l, b, os.SameFile(first, info), want)
			}
		}
	}
	assertRefused := func(t *testing.T, err error, rec CommitReceipt, res Result) {
		t.Helper()
		if CodeOf(err) != CodeUnsupportedFilesystem || rec.Committed || rec.DirtyFailure {
			t.Fatalf("expected clean unsupported refusal, got err=%v rec=%+v", err, rec)
		}
		f, r := r7Files(t, res, rec, "a/x.txt")
		if f.PublishState != PublishUnsupported || f.Published || r.Wrote || f.NewRev != "" || r.NewSHA256 != "" {
			t.Fatalf("hard-link receipt: %+v %+v", f, r)
		}
	}

	t.Run("batch_preflight_refuses_everything", func(t *testing.T) {
		_, root, aliases := setup(t)
		e := newTestEngine(t, root)
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
			{Path: "plain.txt", Content: bytesPtr("plain-v2\n")},
			{Path: "a/x.txt", Content: bytesPtr("shared-v2\n")},
		}})
		assertRefused(t, err, rec, res)
		if got := readFileContent(t, root, "plain.txt"); got != "plain-v1\n" {
			t.Fatalf("preflight refusal still wrote plain.txt: %q", got)
		}
		assertAliasesIntact(t, aliases, "shared-v1\n")
		assertNoLCXTempLeftovers(t, root)
	})

	t.Run("link_created_after_preflight", func(t *testing.T) {
		base, root, _ := setup(t)
		single := filepath.Join(root, "a", "single.txt")
		if err := os.WriteFile(single, []byte("single-v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		late := filepath.Join(base, "late-alias.txt")
		e := newTestEngine(t, root)
		e.beforeWrite = func(dst string) {
			if dst == single {
				if err := os.Link(single, late); err != nil {
					t.Errorf("late link: %v", err)
				}
			}
		}
		_, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file",
			Changes: []FileChange{{Path: "a/single.txt", Content: bytesPtr("single-v2\n")}}})
		if CodeOf(err) != CodeUnsupportedFilesystem {
			t.Fatalf("err=%v, want %s", err, CodeUnsupportedFilesystem)
		}
		assertAliasesIntact(t, []string{single, late}, "single-v1\n")
		assertNoLCXTempLeftovers(t, filepath.Join(root, "a"))
	})

	t.Run("delete_one_name_keeps_other_links", func(t *testing.T) {
		_, root, aliases := setup(t)
		e := newTestEngine(t, root)
		if _, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "apply_patch",
			Changes: []FileChange{{Path: "a/x.txt", BaseRev: RevOf([]byte("shared-v1\n")), Delete: true}}}); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := os.Lstat(aliases[0]); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted name still present: %v", err)
		}
		assertAliasesIntact(t, aliases[1:], "shared-v1\n")
		assertNoLCXTempLeftovers(t, filepath.Join(root, "a"))
	})
}

// TestOpusPRE02DeleteRecreatedPathKeepsQuarantinedOriginal recreates the delete target's path right
// after LC moved the original aside. The delete must not report success or touch the new file, and
// the original stays recoverable at the quarantine path the error names.
func TestOpusPRE02DeleteRecreatedPathKeepsQuarantinedOriginal(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "victim.txt")
	orig := []byte("original-delete-target\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	saved := renameNoReplaceForQuarantine
	renameNoReplaceForQuarantine = func(src, dst string) error {
		if err := saved(src, dst); err != nil {
			return err
		}
		if src == target {
			return os.WriteFile(target, []byte("RECREATED-BY-THIRD-PARTY\n"), 0o644)
		}
		return nil
	}
	t.Cleanup(func() { renameNoReplaceForQuarantine = saved })
	e := newTestEngine(t, root)
	var rec CommitReceipt
	_, delErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
		Actor: testActor(), Tool: "apply_patch",
		Changes: []FileChange{{Path: "victim.txt", BaseRev: RevOf(orig), Delete: true}},
	})
	if CodeOf(delErr) != CodeConflict || rec.Committed {
		t.Fatalf("expected conflict, got err=%v rec=%+v", delErr, rec)
	}
	if got, _ := os.ReadFile(target); string(got) != "RECREATED-BY-THIRD-PARTY\n" {
		t.Fatalf("recreated file changed: %q", got)
	}
	assertOnlyReportedLCXLeftovers(t, root, delErr, orig)
	entries, _ := os.ReadDir(root)
	kept := 0
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".lcx-del-") {
			kept++
		}
	}
	if kept != 1 {
		t.Fatalf("original not kept exactly once: entries=%v err=%v", entries, delErr)
	}
}

// TestOpusPRE02DeleteWithoutNoReplaceMoveRefused pins the fail-closed delete policy: without a
// no-replace move the target could only be removed by name after a separate check, so the delete is
// refused as unsupported and the file is left exactly as it was.
func TestOpusPRE02DeleteWithoutNoReplaceMoveRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "victim.txt")
	orig := []byte("original-delete-target\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	saved := renameNoReplaceForQuarantine
	renameNoReplaceForQuarantine = func(_, _ string) error { return syscall.ENOTSUP }
	t.Cleanup(func() { renameNoReplaceForQuarantine = saved })
	e := newTestEngine(t, root)
	var rec CommitReceipt
	res, delErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
		Actor: testActor(), Tool: "apply_patch",
		Changes: []FileChange{{Path: "victim.txt", BaseRev: RevOf(orig), Delete: true}},
	})
	if CodeOf(delErr) != CodeUnsupportedFilesystem || rec.Committed || rec.DirtyFailure {
		t.Fatalf("expected clean unsupported refusal, got err=%v rec=%+v", delErr, rec)
	}
	f, r := r7Files(t, res, rec, "victim.txt")
	if f.PublishState != PublishUnsupported || f.Published || r.Wrote {
		t.Fatalf("unsupported delete receipt: %+v %+v", f, r)
	}
	after, err := os.Lstat(target)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("refused delete changed the file: err=%v", err)
	}
	if got, _ := os.ReadFile(target); string(got) != string(orig) {
		t.Fatalf("content = %q", got)
	}
	assertNoLCXTempLeftovers(t, root)
}
