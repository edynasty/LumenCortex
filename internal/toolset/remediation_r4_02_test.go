//go:build !windows

package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// TestR402ToolsetPostPublicationAndPartialCommitOutcomes verifies that toolset file tools preserve
// DirtyFailure / MutatesWorkspace=true and report published files when Engine.Commit fails after
// destination publication, while keeping MutatesWorkspace=false on clean pre-publication failures.
func TestR402ToolsetPostPublicationAndPartialCommitOutcomes(t *testing.T) {
	t.Run("pre_publish_failure_keeps_mutates_workspace_false", func(t *testing.T) {
		root := t.TempDir()
		editEngine, err := editing.New(editing.Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{
			BeforePublish: func(string) error { return errors.New("pre-publish guard stop") },
		})
		set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: "s-r402", Editing: editEngine})
		if err != nil {
			t.Fatal(err)
		}
		var receipt editing.CommitReceipt
		ctx := editing.WithCommitRecorder(context.Background(), func(r editing.CommitReceipt) { receipt = r })
		res, execErr := set.Execute(ctx, "write_file", map[string]any{
			"path":    "clean.txt",
			"content": "clean\n",
		}, nil)
		if execErr == nil {
			t.Fatal("expected pre-publish error")
		}
		if res.MutatesWorkspace || receipt.DirtyFailure {
			t.Fatalf("expected clean failure: res=%+v receipt=%+v", res, receipt)
		}
		if _, serr := os.Stat(filepath.Join(root, "clean.txt")); !os.IsNotExist(serr) {
			t.Fatalf("clean.txt should not exist: %v", serr)
		}
	})

	t.Run("post_publish_close_error_sets_mutates_workspace_true_and_reports_published_file", func(t *testing.T) {
		root := t.TempDir()
		editEngine, err := editing.New(editing.Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{
			CloseAfterPublish: func(string) error { return syscall.EIO },
		})
		set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: "s-r402", Editing: editEngine})
		if err != nil {
			t.Fatal(err)
		}
		var receipt editing.CommitReceipt
		ctx := editing.WithCommitRecorder(context.Background(), func(r editing.CommitReceipt) { receipt = r })
		res, execErr := set.Execute(ctx, "write_file", map[string]any{
			"path":    "published.txt",
			"content": "published\n",
		}, nil)
		if !errors.Is(execErr, syscall.EIO) {
			t.Fatalf("execErr = %v, want EIO", execErr)
		}
		if !res.MutatesWorkspace {
			t.Fatalf("R4-02 DEFECT: MutatesWorkspace=false after published.txt landed on disk: %+v", res)
		}
		if !receipt.DirtyFailure || receipt.State != editing.CommitStatePublishedWithPostError {
			t.Fatalf("receipt = %+v", receipt)
		}
		got, rerr := os.ReadFile(filepath.Join(root, "published.txt"))
		if rerr != nil || string(got) != "published\n" {
			t.Fatalf("published.txt = %q err=%v", got, rerr)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
			t.Fatalf("unmarshal result content %q: %v", res.Content, err)
		}
		files, ok := payload["publishedFiles"].([]any)
		if !ok || len(files) != 1 {
			t.Fatalf("expected publishedFiles in ToolResult.Content, got %v", payload)
		}
	})

	t.Run("apply_patch_multifile_external_append_plus_conflict_reports_partial_publication", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		editEngine, err := editing.New(editing.Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		aPath := filepath.Join(root, "a.txt")
		bPath := filepath.Join(root, "b.txt")
		editEngine.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterLinkSuccess: func(dst string) error {
				if filepath.Base(dst) == "a.txt" {
					// External writer appends to published a.txt AND races b.txt to cause CodeConflict!
					af, oerr := os.OpenFile(aPath, os.O_WRONLY|os.O_APPEND, 0)
					if oerr != nil {
						return oerr
					}
					_, _ = af.Write([]byte("foreign-append\n"))
					_ = af.Close()
					_ = os.WriteFile(bPath, []byte("concurrent-b\n"), 0o644)
				}
				return nil
			},
		})
		set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: "s-r402", Editing: editEngine})
		if err != nil {
			t.Fatal(err)
		}
		var receipt editing.CommitReceipt
		ctx := editing.WithCommitRecorder(context.Background(), func(r editing.CommitReceipt) { receipt = r })
		res, execErr := set.Execute(ctx, "apply_patch", map[string]any{
			"patches": []any{
				map[string]any{"path": "a.txt", "operation": "create", "content": "agent-a\n"},
				map[string]any{
					"path": "b.txt", "operation": "update",
					"edits": []any{map[string]any{"old_text": "old-b", "new_text": "new-b"}},
				},
			},
		}, nil)
		if execErr != nil {
			t.Fatalf("expected structured editing.Error ToolResult (execErr=nil), got %v", execErr)
		}
		if res.OK || !res.MutatesWorkspace {
			t.Fatalf("expected OK=false MutatesWorkspace=true, got %+v", res)
		}
		if !receipt.DirtyFailure || receipt.State != editing.CommitStatePartiallyPublished {
			t.Fatalf("receipt = %+v", receipt)
		}
		gotA, rerr := os.ReadFile(aPath)
		if rerr != nil || string(gotA) != "agent-a\nforeign-append\n" {
			t.Fatalf("a.txt = %q err=%v", gotA, rerr)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(res.Content), &payload); err != nil {
			t.Fatalf("unmarshal %q: %v", res.Content, err)
		}
		files, ok := payload["publishedFiles"].([]any)
		if !ok || len(files) != 1 {
			t.Fatalf("expected publishedFiles=[a.txt] in refusal JSON, got %v", payload)
		}
	})
}
