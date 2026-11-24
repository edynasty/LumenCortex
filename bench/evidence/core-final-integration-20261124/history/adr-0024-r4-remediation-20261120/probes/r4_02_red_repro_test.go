//go:build !windows

package editing

import (
	"context"
	"os"
	"path/filepath"
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

// TestR402PostPublishDefectsReproducer reproduces all three confirmed R4-02 defects on unmodified
// internal/editing using only existing package-level primitives:
//  1. Single-file atomic link succeeds, then post-publish Stat/Close fails -> new.txt is published on
//     disk ("published\n") while CommitReceipt reports Committed=false, DirtyFailure=false and omits
//     new.txt from both Result.Files and CommitReceipt.Files.
//  2. Single-file atomic no-replace rename fallback succeeds, then post-publish Stat/Close fails ->
//     same false-clean receipt and omitted published file.
//  3. Multi-file plan [a.txt, b.txt] where a.txt publishes cleanly and b.txt publishes via link and
//     then fails post-publish -> rollback removes a.txt but skips published b.txt!
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
		if !receipt.DirtyFailure {
			t.Errorf("R4-02 DEFECT: new.txt=%q published on disk, yet CommitReceipt reported clean failure (%+v)", got, receipt)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "new.txt" {
			t.Errorf("R4-02 DEFECT: published file new.txt omitted from Result.Files: %+v", res.Files)
		}
		if len(receipt.Files) != 1 || !receipt.Files[0].Wrote {
			t.Errorf("R4-02 DEFECT: published file new.txt omitted/unmarked in CommitReceipt.Files: %+v", receipt.Files)
		}
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
		if !receipt.DirtyFailure {
			t.Errorf("R4-02 DEFECT: new.txt=%q published via rename, yet CommitReceipt reported clean failure (%+v)", got, receipt)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "new.txt" {
			t.Errorf("R4-02 DEFECT: published file new.txt omitted from Result.Files: %+v", res.Files)
		}
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
		_, cerr := e.Commit(ctx, Plan{
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
		if !receipt.DirtyFailure {
			t.Errorf("expected DirtyFailure=true on multi-file partial publish, got %+v", receipt)
		}
		if _, err := os.Stat(filepath.Join(root, "a.txt")); !os.IsNotExist(err) {
			t.Errorf("expected a.txt rolled back, stat err=%v", err)
		}
		if bData, err := os.ReadFile(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
			t.Errorf("R4-02 DEFECT: multi-file rollback skipped published b.txt (%q still on disk, err=%v)", bData, err)
		}
	})
}
