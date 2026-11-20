//go:build !windows

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

type r402ClosingStager struct {
	LocalHost
	files map[string]*os.File
}

func (h *r402ClosingStager) OpenCreateTemp(target string) (*os.File, string, error) {
	f, p, err := NewCreateTempFile(filepath.Dir(target))
	if err == nil {
		if h.files == nil {
			h.files = make(map[string]*os.File)
		}
		h.files[target] = f
	}
	return f, p, err
}

func (h *r402ClosingStager) BeforeAtomicPublish(string) error { return nil }

func assertNoCreateTempLeftovers(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(p string, _ os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if strings.HasPrefix(filepath.Base(p), ".lcx-create-") {
			t.Errorf("unexpected temp file leftover: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk workspace %s: %v", root, err)
	}
}

// TestR402PostPublishDefectsReproducer pins the three un-hooked R4-02 reproducers that failed on
// unmodified aeb2e50 / 4ac97248 internal/editing code (logs/r4-02-red.log).
func TestR402PostPublishDefectsReproducer(t *testing.T) {
	t.Run("single_file_link_succeeds_then_post_publish_error", func(t *testing.T) {
		root := t.TempDir()
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		h := &r402ClosingStager{LocalHost: lh}
		linkForPublish = func(oldname, newname string) error {
			err := os.Link(oldname, newname)
			if f := h.files[newname]; f != nil {
				_ = f.Close()
			}
			return err
		}
		t.Cleanup(func() { linkForPublish = os.Link })

		e := newTestEngine(t, root)
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("published\n")
		res, cerr := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Host:    h,
			Changes: []FileChange{{Path: "new.txt", Content: &body, AllowCreate: true}},
		})
		if cerr == nil {
			t.Fatalf("expected post-publish error")
		}
		got, rerr := os.ReadFile(filepath.Join(root, "new.txt"))
		if rerr != nil || string(got) != "published\n" {
			t.Fatalf("expected new.txt published on disk, got %q err=%v", got, rerr)
		}
		if receipt.Committed || !receipt.DirtyFailure || receipt.State != CommitStatePublishedWithPostError {
			t.Errorf("R4-02 DEFECT: new.txt=%q published on disk, yet CommitReceipt=%+v", got, receipt)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "new.txt" || !res.Files[0].Published || res.Files[0].PublishState != PublishSucceededWithPostError {
			t.Errorf("R4-02 DEFECT: published file new.txt omitted/misclassified in Result.Files: %+v", res.Files)
		}
		if len(receipt.Files) != 1 || !receipt.Files[0].Wrote || !receipt.Files[0].Published || receipt.Files[0].Verified || receipt.Files[0].PublishState != PublishSucceededWithPostError {
			t.Errorf("R4-02 DEFECT: published file new.txt omitted/misclassified in CommitReceipt.Files: %+v", receipt.Files)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	t.Run("single_file_rename_fallback_succeeds_then_post_publish_error", func(t *testing.T) {
		root := t.TempDir()
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		h := &r402ClosingStager{LocalHost: lh}
		linkForPublish = func(oldname, newname string) error {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EPERM}
		}
		renameNoReplaceForPublish = func(oldname, newname string) error {
			err := renameNoReplacePlatform(oldname, newname)
			if f := h.files[newname]; f != nil {
				_ = f.Close()
			}
			return err
		}
		t.Cleanup(func() {
			linkForPublish = os.Link
			renameNoReplaceForPublish = renameNoReplacePlatform
		})

		e := newTestEngine(t, root)
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("published-via-rename\n")
		res, cerr := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Host:    h,
			Changes: []FileChange{{Path: "new.txt", Content: &body, AllowCreate: true}},
		})
		if cerr == nil {
			t.Fatalf("expected post-publish error")
		}
		got, rerr := os.ReadFile(filepath.Join(root, "new.txt"))
		if rerr != nil || string(got) != "published-via-rename\n" {
			t.Fatalf("expected new.txt published on disk via rename, got %q err=%v", got, rerr)
		}
		if receipt.Committed || !receipt.DirtyFailure || receipt.State != CommitStatePublishedWithPostError {
			t.Errorf("R4-02 DEFECT: new.txt=%q published via rename, yet CommitReceipt=%+v", got, receipt)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "new.txt" || !res.Files[0].Published {
			t.Errorf("R4-02 DEFECT: published file new.txt omitted from Result.Files: %+v", res.Files)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	t.Run("multi_file_second_file_post_publish_error_must_not_skip_rollback", func(t *testing.T) {
		root := t.TempDir()
		lh, err := NewLocalHost(root)
		if err != nil {
			t.Fatal(err)
		}
		h := &r402ClosingStager{LocalHost: lh}
		linkForPublish = func(oldname, newname string) error {
			err := os.Link(oldname, newname)
			if filepath.Base(newname) == "b.txt" {
				if f := h.files[newname]; f != nil {
					_ = f.Close()
				}
			}
			return err
		}
		t.Cleanup(func() { linkForPublish = os.Link })

		e := newTestEngine(t, root)
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		aBody := []byte("published-a\n")
		bBody := []byte("published-b\n")
		res, cerr := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Host:  h,
			Changes: []FileChange{
				{Path: "a.txt", Content: &aBody, Create: true},
				{Path: "b.txt", Content: &bBody, Create: true},
			},
		})
		if cerr == nil {
			t.Fatalf("expected post-publish error on b.txt")
		}
		if !receipt.DirtyFailure || receipt.State != CommitStateRolledBack {
			t.Errorf("expected DirtyFailure=true State=rolled_back, got %+v", receipt)
		}
		if len(res.Files) != 0 {
			t.Errorf("expected 0 remaining files in Result.Files after clean rollback, got %+v", res.Files)
		}
		if len(receipt.Files) != 2 ||
			!receipt.Files[0].Published || !receipt.Files[0].RolledBack || receipt.Files[0].Wrote || receipt.Files[0].PublishState != PublishRolledBack ||
			!receipt.Files[1].Published || !receipt.Files[1].RolledBack || receipt.Files[1].Wrote || receipt.Files[1].PublishState != PublishRolledBack {
			t.Errorf("unexpected multi-file receipt files: %+v", receipt.Files)
		}
		if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
			t.Errorf("expected a.txt rolled back, stat err=%v", err)
		}
		if bData, err := os.ReadFile(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
			t.Errorf("R4-02 DEFECT: multi-file rollback skipped published b.txt (%q still on disk, err=%v)", bData, err)
		}
		assertNoCreateTempLeftovers(t, root)
	})
}

// TestR402PublicationStateModelAndFaultMatrix exercises deterministic fault injection across all
// R4-02 publication states and multi-file rollback paths on real filesystem directories.
func TestR402PublicationStateModelAndFaultMatrix(t *testing.T) {
	// 1. Failure before publication keeps DirtyFailure=false and leaves destination absent.
	t.Run("1_failure_before_publication_is_clean", func(t *testing.T) {
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
		if receipt.Committed || receipt.DirtyFailure || receipt.State != CommitStatePrePublishFailure {
			t.Fatalf("expected clean pre-publish receipt, got %+v", receipt)
		}
		if len(res.Files) != 0 {
			t.Fatalf("expected empty Result.Files, got %+v", res.Files)
		}
		if _, serr := os.Stat(filepath.Join(root, "clean.txt")); !os.IsNotExist(serr) {
			t.Fatalf("clean.txt should not exist on disk: %v", serr)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 2. Atomic link succeeds, followed by post-link fault.
	t.Run("2_atomic_link_succeeds_then_error", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterLinkSuccess: func(string) error { return errors.New("injected post-link failure") },
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("linked-ok\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "linked.txt", Content: &body, AllowCreate: true}},
		})
		if err == nil {
			t.Fatal("expected post-link error")
		}
		if got := readFileContent(t, root, "linked.txt"); got != "linked-ok\n" {
			t.Fatalf("linked.txt = %q, want linked-ok\\n", got)
		}
		if !receipt.DirtyFailure || receipt.Committed || receipt.State != CommitStatePublishedWithPostError {
			t.Fatalf("receipt = %+v, want DirtyFailure=true PublishedWithPostError", receipt)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "linked.txt" || !res.Files[0].Published || res.Files[0].PublishState != PublishSucceededWithPostError {
			t.Fatalf("res.Files = %+v", res.Files)
		}
		if len(receipt.Files) != 1 || !receipt.Files[0].Staged || !receipt.Files[0].Published || receipt.Files[0].Verified || !receipt.Files[0].Wrote {
			t.Fatalf("receipt.Files = %+v", receipt.Files)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 3. Atomic no-replace rename fallback succeeds, followed by post-rename fault.
	t.Run("3_atomic_rename_succeeds_then_error", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			ForceLinkFallback: func(dst string) error {
				return &os.LinkError{Op: "link", Old: dst + ".tmp", New: dst, Err: syscall.EPERM}
			},
			AfterRenameSuccess: func(string) error { return errors.New("injected post-rename failure") },
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("renamed-ok\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "renamed.txt", Content: &body, AllowCreate: true}},
		})
		if err == nil {
			t.Fatal("expected post-rename error")
		}
		if got := readFileContent(t, root, "renamed.txt"); got != "renamed-ok\n" {
			t.Fatalf("renamed.txt = %q, want renamed-ok\\n", got)
		}
		if !receipt.DirtyFailure || receipt.State != CommitStatePublishedWithPostError {
			t.Fatalf("receipt = %+v", receipt)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "renamed.txt" || !res.Files[0].Published {
			t.Fatalf("res.Files = %+v", res.Files)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 4. Stat fails after publication (single-file preserves published file; multi-file rolls back via pre-publish identity).
	t.Run("4_stat_fails_after_publication", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			StatAfterPublish: func(string, *os.File) (os.FileInfo, error) {
				return nil, syscall.EIO
			},
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("stat-failed-after-pub\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "stat_err.txt", Content: &body, AllowCreate: true}},
		})
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v, want EIO", err)
		}
		if got := readFileContent(t, root, "stat_err.txt"); got != "stat-failed-after-pub\n" {
			t.Fatalf("stat_err.txt = %q", got)
		}
		if !receipt.DirtyFailure || receipt.State != CommitStatePublishedWithPostError || len(res.Files) != 1 {
			t.Fatalf("receipt=%+v res=%+v", receipt, res)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 5. Close fails after publication.
	t.Run("5_close_fails_after_publication", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		e.SetPublishFaultHooks(PublishFaultHooks{
			CloseAfterPublish: func(string) error { return syscall.EIO },
		})
		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		body := []byte("close-failed-after-pub\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "close_err.txt", Content: &body, AllowCreate: true}},
		})
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v, want EIO", err)
		}
		if got := readFileContent(t, root, "close_err.txt"); got != "close-failed-after-pub\n" {
			t.Fatalf("close_err.txt = %q", got)
		}
		if !receipt.DirtyFailure || receipt.State != CommitStatePublishedWithPostError || len(res.Files) != 1 || !receipt.Files[0].Wrote {
			t.Fatalf("receipt=%+v res=%+v", receipt, res)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 6. File identity capture fails (single-file AND multi-file rollback refusing blind unlink without verified identity).
	t.Run("6_identity_capture_fails_single_and_multifile", func(t *testing.T) {
		// 6a: Single-file
		root1 := t.TempDir()
		e1 := newTestEngine(t, root1)
		e1.SetPublishFaultHooks(PublishFaultHooks{
			CaptureIdentity: func(string, *os.File) error { return errors.New("cannot read inode identity") },
		})
		var r1 CommitReceipt
		ctx1 := WithCommitRecorder(context.Background(), func(r CommitReceipt) { r1 = r })
		b1 := []byte("no-id-single\n")
		res1, err1 := e1.Commit(ctx1, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "noid.txt", Content: &b1, AllowCreate: true}},
		})
		if err1 == nil || !r1.DirtyFailure || r1.State != CommitStatePublishedWithPostError || len(res1.Files) != 1 {
			t.Fatalf("6a err=%v r1=%+v res1=%+v", err1, r1, res1)
		}
		if got := readFileContent(t, root1, "noid.txt"); got != "no-id-single\n" {
			t.Fatalf("6a noid.txt = %q", got)
		}

		// 6b: Multi-file where a.txt fails identity capture after publishing: rollback must NOT unlink
		// an unverified file and must report it as remaining published in Result.Files & CommitReceipt!
		root2 := t.TempDir()
		e2 := newTestEngine(t, root2)
		e2.SetPublishFaultHooks(PublishFaultHooks{
			CaptureIdentity: func(dst string, _ *os.File) error {
				if filepath.Base(dst) == "a.txt" {
					return errors.New("identity unavailable on a.txt")
				}
				return nil
			},
		})
		var r2 CommitReceipt
		ctx2 := WithCommitRecorder(context.Background(), func(r CommitReceipt) { r2 = r })
		aBytes, bBytes := []byte("a-noid\n"), []byte("b-unreached\n")
		res2, err2 := e2.Commit(ctx2, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aBytes, Create: true},
				{Path: "b.txt", Content: &bBytes, Create: true},
			},
		})
		if err2 == nil {
			t.Fatal("6b expected error")
		}
		if !r2.DirtyFailure || r2.State != CommitStatePartiallyPublished {
			t.Fatalf("6b receipt = %+v", r2)
		}
		if got := readFileContent(t, root2, "a.txt"); got != "a-noid\n" {
			t.Fatalf("6b expected unverified published a.txt preserved on disk, got %q", got)
		}
		if _, serr := os.Stat(filepath.Join(root2, "b.txt")); !os.IsNotExist(serr) {
			t.Fatalf("6b expected b.txt never created, stat=%v", serr)
		}
		if len(res2.Files) != 1 || res2.Files[0].Path != "a.txt" || !res2.Files[0].Published || res2.Files[0].RolledBack {
			t.Fatalf("6b res2.Files = %+v", res2.Files)
		}
		if len(r2.Files) != 2 || !r2.Files[0].Published || r2.Files[0].RolledBack || !r2.Files[0].Wrote || r2.Files[1].Published {
			t.Fatalf("6b r2.Files = %+v", r2.Files)
		}
		assertNoCreateTempLeftovers(t, root2)
	})

	// 7. Multi-file first publication succeeds, second fails before publication.
	t.Run("7_multifile_first_publishes_second_fails_rolls_back_cleanly", func(t *testing.T) {
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
		aBytes, bBytes := []byte("first\n"), []byte("second\n")
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
		if !receipt.DirtyFailure || receipt.State != CommitStateRolledBack {
			t.Fatalf("receipt = %+v, want DirtyFailure=true State=rolled_back", receipt)
		}
		if len(res.Files) != 0 {
			t.Fatalf("res.Files = %+v, want empty after rollback", res.Files)
		}
		if len(receipt.Files) != 2 ||
			receipt.Files[0].PublishState != PublishRolledBack || !receipt.Files[0].Published || !receipt.Files[0].RolledBack || receipt.Files[0].Wrote ||
			receipt.Files[1].PublishState != PublishNone || receipt.Files[1].Published {
			t.Fatalf("receipt.Files = %+v", receipt.Files)
		}
		if _, serr := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(serr) {
			t.Fatalf("a.txt not rolled back: %v", serr)
		}
		if _, serr := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(serr) {
			t.Fatalf("b.txt should not exist: %v", serr)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 8. Multi-file rollback encounters external append on published first file -> preserves external append!
	t.Run("8_rollback_preserves_external_append_and_reports_published_file", func(t *testing.T) {
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
		if len(res.Files) != 1 || res.Files[0].Path != "a.txt" || !res.Files[0].Published || res.Files[0].RolledBack || res.Files[0].PublishState != PublishExternalModification || res.Files[0].NewRev != "" {
			t.Fatalf("expected published externally-modified a.txt in Result.Files, got %+v", res.Files)
		}
		if len(receipt.Files) != 2 || !receipt.Files[0].Published || receipt.Files[0].RolledBack || !receipt.Files[0].Wrote || receipt.Files[0].Verified || receipt.Files[0].PublishState != PublishExternalModification || receipt.Files[0].NewSHA256 != "" {
			t.Fatalf("receipt.Files = %+v", receipt.Files)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 9. Multi-file rollback encounters external atomic replacement (even exact same byte length!).
	t.Run("9_rollback_preserves_external_replacement_same_length", func(t *testing.T) {
		root := t.TempDir()
		e := newTestEngine(t, root)
		aPath := filepath.Join(root, "a.txt")
		foreignSameLen := []byte("FOREIGN!\n") // exact same 9-byte length as "AGENT-A!\n"
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
			t.Fatalf("external replacement lost/corrupted: got %q, want %q", got, foreignSameLen)
		}
		if !receipt.DirtyFailure || receipt.State != CommitStatePartiallyPublished {
			t.Fatalf("receipt = %+v", receipt)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "a.txt" || !res.Files[0].Published || res.Files[0].RolledBack || res.Files[0].PublishState != PublishExternalModification || res.Files[0].NewRev != "" {
			t.Fatalf("res.Files = %+v", res.Files)
		}
		assertNoCreateTempLeftovers(t, root)
	})

	// 10. Uncertain publication (ambiguous EIO during link/rename) reports PublishUncertain + DirtyFailure=true.
	t.Run("10_uncertain_publication_reports_dirty_failure", func(t *testing.T) {
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
		if !receipt.DirtyFailure || receipt.Committed || receipt.State != CommitStatePublicationUncertain {
			t.Fatalf("receipt = %+v, want DirtyFailure=true PublicationUncertain", receipt)
		}
		if len(receipt.Files) != 1 || receipt.Files[0].PublishState != PublishUncertain || receipt.Files[0].Published {
			t.Fatalf("receipt.Files = %+v", receipt.Files)
		}
		if len(res.Files) != 0 {
			t.Fatalf("res.Files = %+v, want empty when destination not confirmed published", res.Files)
		}
		assertNoCreateTempLeftovers(t, root)
	})
}
