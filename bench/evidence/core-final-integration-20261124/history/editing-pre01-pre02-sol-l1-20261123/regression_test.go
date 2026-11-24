//go:build darwin || linux

package editing

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

// TestSolPREL1UnsupportedSwapRefused injects ENOTSUP at exchange with complete staging.
func TestSolPREL1UnsupportedSwapRefused(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, root, "f.txt", "original-content\n")
	writeFile(t, root, "third-party.txt", "untouched-third-party\n")
	target := filepath.Join(root, "f.txt")
	third := filepath.Join(root, "third-party.txt")
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	thirdBefore, err := os.Lstat(third)
	if err != nil {
		t.Fatal(err)
	}
	next := []byte("agent replacement\n")
	saved := renameSwapForPublish
	swapCalls := 0
	renameSwapForPublish = func(src, dst string) error {
		swapCalls++
		staged, err := os.ReadFile(src)
		if err != nil || string(staged) != string(next) || dst != target {
			t.Errorf("exchange did not reach complete staging: src=%s dst=%s bytes=%q err=%v", src, dst, staged, err)
		}
		return syscall.ENOTSUP
	}
	t.Cleanup(func() { renameSwapForPublish = saved })
	e := newTestEngine(t, root)
	e.SetPublishFaultHooks(PublishFaultHooks{AfterInPlaceWriteClose: func(string) error {
		t.Error("unsupported exchange entered the post-publication path")
		return nil
	}})
	var rec CommitReceipt
	res, commitErr := e.Commit(WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r }), Plan{
		Actor: testActor(), Tool: "write_file", Changes: []FileChange{{Path: "f.txt", Content: &next}},
	})
	if swapCalls != 1 {
		t.Errorf("exchange calls=%d, want exactly one after successful staging", swapCalls)
	}
	r7AssertSettledCount(t, res, rec, 1)
	assertL1UnsupportedUpdate(t, "f.txt", commitErr, res, rec)
	assertL1OriginalFile(t, target, before, "original-content\n")
	assertL1OriginalFile(t, third, thirdBefore, "untouched-third-party\n")
	if copies := opusConflictCopies(t, root, "f.txt"); len(copies) != 0 {
		t.Errorf("unsupported exchange generated recovery copies: %v", copies)
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

// TestSolPREL1LinkChild creates the alias from a separate process at the staging barrier.
func TestSolPREL1LinkChild(t *testing.T) {
	if target := os.Getenv("LC_SOL_PRE_L1_LINK_TARGET"); target != "" {
		if err := os.Link(target, os.Getenv("LC_SOL_PRE_L1_LINK_ALIAS")); err != nil {
			t.Fatal(err)
		}
	}
}

func assertL1UnsupportedUpdate(t *testing.T, rel string, err error, res Result, rec CommitReceipt) {
	t.Helper()
	if CodeOf(err) != CodeUnsupportedFilesystem || rec.Committed || rec.DirtyFailure ||
		res.CommitState != CommitStatePrePublishFailure || rec.State != CommitStatePrePublishFailure {
		t.Errorf("expected clean unsupported refusal for %s: err=%v res=%+v rec=%+v", rel, err, res, rec)
	}
	f, r := r7Files(t, res, rec, rel)
	if f.PublishState != PublishUnsupported || f.Changed || f.Published || r.Wrote || f.Verified ||
		f.NewRev != "" || r.NewSHA256 != "" || f.Error == "" || r.Error == "" {
		t.Errorf("unsupported update result and receipt: %+v %+v", f, r)
	}
}

func assertL1OriginalFile(t *testing.T, path string, before os.FileInfo, want string) {
	t.Helper()
	after, statErr := os.Lstat(path)
	raw, readErr := os.ReadFile(path)
	same := statErr == nil && os.SameFile(before, after)
	if !same || readErr != nil || string(raw) != want {
		t.Errorf("%s did not retain original inode and bytes: same inode=%v, bytes=%q, stat=%v, read=%v",
			path, same, raw, statErr, readErr)
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
		if err := os.WriteFile(filepath.Join(root, "0-plain.txt"), []byte("plain-v1\n"), 0o644); err != nil {
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

	t.Run("batch_preflight_refuses_everything", func(t *testing.T) {
		_, root, aliases := setup(t)
		plain := filepath.Join(root, "0-plain.txt")
		plainBefore, err := os.Lstat(plain)
		if err != nil {
			t.Fatal(err)
		}
		linkedBefore, err := os.Lstat(aliases[0])
		if err != nil {
			t.Fatal(err)
		}
		e := newTestEngine(t, root)
		writeAttempts := 0
		e.beforeWrite = func(string) { writeAttempts++ }
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
			{Path: "0-plain.txt", Content: bytesPtr("plain-v2\n")},
			{Path: "a/x.txt", Content: bytesPtr("shared-v2\n")},
		}})
		r7AssertSettledCount(t, res, rec, 2)
		assertL1UnsupportedUpdate(t, "a/x.txt", err, res, rec)
		f, r := r7Files(t, res, rec, "0-plain.txt")
		r7AssertNotAttempted(t, f, r)
		if writeAttempts != 0 || f.Changed || r.Staged || r.Wrote {
			t.Errorf("preflight reached writer: attempts=%d result=%+v receipt=%+v", writeAttempts, f, r)
		}
		assertL1OriginalFile(t, plain, plainBefore, "plain-v1\n")
		for _, alias := range aliases {
			assertL1OriginalFile(t, alias, linkedBefore, "shared-v1\n")
		}
		assertNoLCXTempLeftovers(t, root)
		assertNoLCXTempLeftovers(t, filepath.Dir(aliases[0]))
	})

	t.Run("link_created_after_staging", func(t *testing.T) {
		base, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(base, "ws")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, root, "single.txt", "single-v1\n")
		writeFile(t, root, "unrelated.txt", "untouched\n")
		target := filepath.Join(root, "single.txt")
		unrelated := filepath.Join(root, "unrelated.txt")
		before, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		unrelatedBefore, err := os.Lstat(unrelated)
		if err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(base, "late-alias.txt")
		var stagedPath string
		linkCreated := false
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			WriteStaged: func(_ string, f *os.File, data []byte) (int, error) {
				stagedPath = f.Name()
				return f.Write(data)
			},
			BeforePublish: func(string) error {
				if staged, err := os.ReadFile(stagedPath); err != nil || string(staged) != "single-v2\n" {
					t.Errorf("barrier did not observe complete stage: %q (%v)", staged, err)
					return errors.New("invalid staging barrier")
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestSolPREL1LinkChild$")
				cmd.Env = append(os.Environ(), "LC_SOL_PRE_L1_LINK_TARGET="+target, "LC_SOL_PRE_L1_LINK_ALIAS="+alias)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Errorf("hard-link child: %v: %s", err, out)
					return err
				}
				info, err := os.Lstat(alias)
				if err != nil || !os.SameFile(before, info) {
					t.Errorf("child did not link original inode at barrier: %v", err)
					return errors.New("invalid hard-link barrier")
				}
				linkCreated = true
				t.Logf("child pid=%d linked original inode after complete staging", cmd.ProcessState.Pid())
				return nil
			},
		})
		saved := renameSwapForPublish
		swapCalls := 0
		renameSwapForPublish = func(src, dst string) error {
			swapCalls++
			return saved(src, dst)
		}
		t.Cleanup(func() { renameSwapForPublish = saved })
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file",
			Changes: []FileChange{{Path: "single.txt", Content: bytesPtr("single-v2\n")}}})
		if !linkCreated || swapCalls != 0 {
			t.Errorf("late-link barrier completed=%v, exchange calls=%d; want true and 0", linkCreated, swapCalls)
		}
		r7AssertSettledCount(t, res, rec, 1)
		assertL1UnsupportedUpdate(t, "single.txt", err, res, rec)
		assertL1OriginalFile(t, target, before, "single-v1\n")
		assertL1OriginalFile(t, alias, before, "single-v1\n")
		assertL1OriginalFile(t, unrelated, unrelatedBefore, "untouched\n")
		assertNoLCXTempLeftovers(t, root)
		assertNoLCXTempLeftovers(t, base)
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
