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

func assertNoCreateTempLeftovers(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".lcx-create-") {
			t.Fatalf("leftover private staging file: %s", entry.Name())
		}
	}
}

// TestR7ForwardPublicationFaultMatrix pins the forward publication states R7 retains. Rollback
// outcomes are no longer produced; a published or partially written destination is preserved.
func TestR7ForwardPublicationFaultMatrix(t *testing.T) {
	t.Run("01_failure_before_publication_is_clean", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			BeforePublish: func(string) error { return errors.New("guard rejected before publish") },
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("never-published\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "clean.txt", Content: &body, AllowCreate: true}},
		})
		if err == nil {
			t.Fatal("expected pre-publish error")
		}
		r7AssertSettledCount(t, res, receipt, 1)
		if receipt.Committed || receipt.DirtyFailure || receipt.State != CommitStatePrePublishFailure {
			t.Fatalf("expected clean pre-publish receipt, got %+v", receipt)
		}
		f, r := r7Files(t, res, receipt, "clean.txt")
		if f.PublishState != PublishFailedBeforeWrite || f.Published || r.Wrote || f.Verified || r.NewSHA256 != "" || !r.Staged {
			t.Fatalf("pre-publish file: %+v %+v", f, r)
		}
		if _, serr := os.Stat(filepath.Join(root, "clean.txt")); !os.IsNotExist(serr) {
			t.Fatalf("clean.txt should not exist on disk: %v", serr)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	t.Run("05b_inplace_close_failure_is_uncertain", func(t *testing.T) {
		requireExistingFileUpdate(t)
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		e := newTestEngine(t, root)
		actor := testActor()
		e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterInPlaceWriteClose: func(string) error { return syscall.EIO },
		})
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		next := []byte("new-a\n")
		res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
			{Path: "a.txt", Content: &next},
		}})
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v, want EIO", err)
		}
		if got := readFileContent(t, root, "a.txt"); got != "new-a\n" {
			t.Fatalf("visible bytes changed: %q", got)
		}
		if !rec.DirtyFailure || rec.Committed {
			t.Fatalf("receipt = %+v", rec)
		}
		f, r := r7Files(t, res, rec, "a.txt")
		if f.PublishState != PublishUncertain || !f.Published || f.Verified || !r.Wrote || f.NewRev != "" || r.NewSHA256 != "" || f.RolledBack || r.RolledBack {
			t.Fatalf("in-place close uncertainty: %+v %+v", f, r)
		}
		if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
			t.Fatalf("uncertain close kept snapshot %q", got)
		}
	})

	for _, tc := range []struct {
		name  string
		hooks PublishFaultHooks
	}{
		{name: "02_link_succeeds_then_error", hooks: PublishFaultHooks{
			AfterLinkSuccess: func(string) error { return errors.New("injected post-link failure") },
		}},
		{name: "03_rename_succeeds_then_error", hooks: PublishFaultHooks{
			ForceLinkFallback: func(dst string) error {
				return &os.LinkError{Op: "link", Old: dst + ".tmp", New: dst, Err: syscall.EPERM}
			},
			AfterRenameSuccess: func(string) error { return errors.New("injected post-rename failure") },
		}},
		{name: "04_stat_fails_after_publication", hooks: PublishFaultHooks{
			StatAfterPublish: func(string, *os.File) (os.FileInfo, error) { return nil, syscall.EIO },
		}},
		{name: "05_close_fails_after_publication", hooks: PublishFaultHooks{
			CloseAfterPublish: func(string) error { return syscall.EIO },
		}},
		{name: "06_identity_capture_fails", hooks: PublishFaultHooks{
			CaptureIdentity: func(string, *os.File) error { return errors.New("cannot read inode identity") },
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			e := newTestEngine(t, root)
			e.SetPublishFaultHooks(tc.hooks)
			var receipt CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
			body := []byte("published-before-error\n")
			res, err := e.Commit(ctx, Plan{
				Actor:   testActor(),
				Tool:    "write_file",
				Changes: []FileChange{{Path: "out.txt", Content: &body, AllowCreate: true}},
			})
			if err == nil {
				t.Fatal("expected post-publication error")
			}
			if got := readFileContent(t, root, "out.txt"); got != "published-before-error\n" {
				t.Fatalf("published destination changed: %q", got)
			}
			r7AssertSettledCount(t, res, receipt, 1)
			if !receipt.DirtyFailure || receipt.Committed || receipt.State != CommitStatePublishedWithPostError {
				t.Fatalf("receipt = %+v", receipt)
			}
			f, r := r7Files(t, res, receipt, "out.txt")
			if f.PublishState != PublishSucceededWithPostError || !f.Published || !r.Wrote || f.Verified ||
				f.NewRev != "" || r.NewSHA256 != "" || f.RolledBack || r.RolledBack {
				t.Fatalf("post-publication failure: %+v %+v", f, r)
			}
			assertNoCreateTempLeftovers(t, root)
		})
	}

	t.Run("07_identity_failure_keeps_published_prefix_and_skips_suffix", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			CaptureIdentity: func(dst string, _ *os.File) error {
				if filepath.Base(dst) == "a.txt" {
					return errors.New("identity unavailable on a.txt")
				}
				return nil
			},
		})
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		aBytes, bBytes, cBytes := []byte("a-published\n"), []byte("b-unreached\n"), []byte("c-unreached\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aBytes, Create: true},
				{Path: "b.txt", Content: &bBytes, Create: true},
				{Path: "c.txt", Content: &cBytes, Create: true},
			},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		r7AssertSettledCount(t, res, rec, 3)
		if !rec.DirtyFailure || rec.State != CommitStatePartiallyPublished {
			t.Fatalf("receipt = %+v", rec)
		}
		if got := readFileContent(t, root, "a.txt"); got != "a-published\n" {
			t.Fatalf("published prefix changed: %q", got)
		}
		fa, ra := r7Files(t, res, rec, "a.txt")
		if fa.PublishState != PublishSucceededWithPostError || !fa.Published || !ra.Wrote || fa.Verified || fa.NewRev != "" || ra.NewSHA256 != "" {
			t.Fatalf("prefix: %+v %+v", fa, ra)
		}
		for _, name := range []string{"b.txt", "c.txt"} {
			f, r := r7Files(t, res, rec, name)
			r7AssertNotAttempted(t, f, r)
			if _, serr := os.Stat(filepath.Join(root, name)); !os.IsNotExist(serr) {
				t.Fatalf("%s exists: %v", name, serr)
			}
		}
		assertNoCreateTempLeftovers(t, root)
	})

	t.Run("08_second_path_refusal_keeps_verified_first", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			BeforePublish: func(dst string) error {
				if filepath.Base(dst) == "b.txt" {
					return errors.New("b.txt refused before publish")
				}
				return nil
			},
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		aBytes, bBytes, cBytes := []byte("first\n"), []byte("second\n"), []byte("third\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aBytes, Create: true},
				{Path: "b.txt", Content: &bBytes, Create: true},
				{Path: "c.txt", Content: &cBytes, Create: true},
			},
		})
		if err == nil {
			t.Fatal("expected failure on b.txt")
		}
		r7AssertSettledCount(t, res, receipt, 3)
		if !receipt.DirtyFailure || receipt.State != CommitStatePartiallyPublished {
			t.Fatalf("receipt = %+v", receipt)
		}
		fa, ra := r7Files(t, res, receipt, "a.txt")
		r7AssertVerified(t, fa, ra)
		if got := readFileContent(t, root, "a.txt"); got != "first\n" {
			t.Fatalf("a.txt = %q, want first", got)
		}
		fb, rb := r7Files(t, res, receipt, "b.txt")
		if fb.PublishState != PublishFailedBeforeWrite || fb.Published || rb.Wrote || fb.Verified || rb.NewSHA256 != "" {
			t.Fatalf("b.txt: %+v %+v", fb, rb)
		}
		fc, rc := r7Files(t, res, receipt, "c.txt")
		r7AssertNotAttempted(t, fc, rc)
		for _, name := range []string{"b.txt", "c.txt"} {
			if _, serr := os.Stat(filepath.Join(root, name)); !os.IsNotExist(serr) {
				t.Fatalf("%s should not exist: %v", name, serr)
			}
		}
		assertNoCreateTempLeftovers(t, root)
	})

	t.Run("09_external_append_on_prefix_is_external", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		aPath := filepath.Join(root, "a.txt")
		e.SetPublishFaultHooks(PublishFaultHooks{
			BeforePublish: func(dst string) error {
				if filepath.Base(dst) == "b.txt" {
					f, oerr := os.OpenFile(aPath, os.O_WRONLY|os.O_APPEND, 0)
					if oerr != nil {
						return oerr
					}
					_, _ = f.Write([]byte("external-append\n"))
					_ = f.Close()
					return errors.New("b.txt failed after external append to a.txt")
				}
				return nil
			},
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		aBytes, bBytes := []byte("agent-a\n"), []byte("agent-b\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aBytes, Create: true},
				{Path: "b.txt", Content: &bBytes, Create: true},
			},
		})
		if err == nil {
			t.Fatal("expected failure on b.txt")
		}
		if got := readFileContent(t, root, "a.txt"); got != "agent-a\nexternal-append\n" {
			t.Fatalf("external append lost/corrupted: got %q", got)
		}
		if !receipt.DirtyFailure || receipt.State != CommitStatePartiallyPublished {
			t.Fatalf("receipt = %+v", receipt)
		}
		fa, ra := r7Files(t, res, receipt, "a.txt")
		if fa.PublishState != PublishExternalModification || !fa.Published || !ra.Wrote || fa.Verified || fa.NewRev != "" || ra.NewSHA256 != "" {
			t.Fatalf("prefix external: %+v %+v", fa, ra)
		}
		fb, rb := r7Files(t, res, receipt, "b.txt")
		if fb.PublishState != PublishFailedBeforeWrite || fb.Published {
			t.Fatalf("b.txt: %+v %+v", fb, rb)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	t.Run("10_external_same_length_replacement_is_external", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		aPath := filepath.Join(root, "a.txt")
		foreignSameLen := []byte("FOREIGN!\n")
		e.SetPublishFaultHooks(PublishFaultHooks{
			BeforePublish: func(dst string) error {
				if filepath.Base(dst) == "b.txt" {
					foreignTmp := filepath.Join(root, ".foreign-replace")
					if werr := os.WriteFile(foreignTmp, foreignSameLen, 0o644); werr != nil {
						return werr
					}
					if rerr := os.Rename(foreignTmp, aPath); rerr != nil {
						return rerr
					}
					return errors.New("b.txt failed after external atomic replacement of a.txt")
				}
				return nil
			},
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		aBytes, bBytes := []byte("AGENT-A!\n"), []byte("AGENT-B!\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aBytes, Create: true},
				{Path: "b.txt", Content: &bBytes, Create: true},
			},
		})
		if err == nil {
			t.Fatal("expected failure on b.txt")
		}
		if got := readFileContent(t, root, "a.txt"); got != string(foreignSameLen) {
			t.Fatalf("external replacement lost: got %q", got)
		}
		if !receipt.DirtyFailure || receipt.State != CommitStatePartiallyPublished {
			t.Fatalf("receipt = %+v", receipt)
		}
		fa, ra := r7Files(t, res, receipt, "a.txt")
		if fa.PublishState != PublishExternalModification || fa.NewRev != "" || ra.NewSHA256 != "" {
			t.Fatalf("replacement prefix: %+v %+v", fa, ra)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	t.Run("11_uncertain_publication_reports_dirty_failure", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			PublishUncertainErr: func(string) error { return syscall.EIO },
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("maybe-published\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "uncertain.txt", Content: &body, AllowCreate: true}},
		})
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v, want EIO", err)
		}
		r7AssertSettledCount(t, res, receipt, 1)
		if !receipt.DirtyFailure || receipt.Committed || receipt.State != CommitStatePublicationUncertain {
			t.Fatalf("receipt = %+v", receipt)
		}
		f, r := r7Files(t, res, receipt, "uncertain.txt")
		if f.PublishState != PublishUncertain || f.Published || f.Verified || r.NewSHA256 != "" {
			t.Fatalf("uncertain destination: %+v %+v", f, r)
		}
		assertNoCreateTempLeftovers(t, root)
	})
}
