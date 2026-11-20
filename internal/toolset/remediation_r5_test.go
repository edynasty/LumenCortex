package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

func TestR5PartialPublicationPayloadAndBounds(t *testing.T) {
	// 1. Complete pre-publish failure vs clean multi-file rollback vs partial publication.
	t.Run("distinguishes_pre_publish_rolled_back_and_partial_publication", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("old-a\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		eng, err := editing.New(editing.Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, Editing: eng, SessionID: "s-r5"})
		if err != nil {
			t.Fatal(err)
		}

		// 1a: Clean pre-publish failure (missing target before write).
		cleanRes, cleanErr := set.Execute(context.Background(), "apply_patch", map[string]any{
			"patches": []any{
				map[string]any{"path": "missing.txt", "operation": "update", "edits": []any{map[string]any{"old_text": "x", "new_text": "y"}}},
			},
		}, nil)
		if cleanErr == nil || cleanRes.MutatesWorkspace {
			t.Fatalf("expected clean pre-publish error without mutation, got res=%+v err=%v", cleanRes, cleanErr)
		}

		// 1b: Published prefix stays (a.txt create lands, b.txt fails before publish): R7 does not
		// roll a.txt back.
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			BeforePublish: func(dst string) error {
				if filepath.Base(dst) == "new_b.txt" {
					return syscall.EACCES
				}
				return nil
			},
		})
		rbRes, rbErr := set.Execute(context.Background(), "apply_patch", map[string]any{
			"patches": []any{
				map[string]any{"path": "new_a.txt", "operation": "create", "content": "secret-a-body\n"},
				map[string]any{"path": "new_b.txt", "operation": "create", "content": "secret-b-body\n"},
			},
		}, nil)
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{})
		if rbErr == nil || !rbRes.MutatesWorkspace {
			t.Fatalf("expected partial-prefix failure with MutatesWorkspace=true, got res=%+v err=%v", rbRes, rbErr)
		}
		var rbPayload map[string]any
		if err := json.Unmarshal([]byte(rbRes.Content), &rbPayload); err != nil {
			t.Fatalf("unmarshal partial payload: %v (%s)", err, rbRes.Content)
		}
		if rbPayload["commitState"] != string(editing.CommitStatePartiallyPublished) ||
			rbPayload["dirtyFailure"] != true ||
			rbPayload["partialPublication"] != true {
			t.Fatalf("unexpected partial-prefix classification: %+v", rbPayload)
		}
		if !strings.Contains(rbRes.Content, string(editing.PublishSucceededAndVerified)) ||
			!strings.Contains(rbRes.Content, string(editing.PublishFailedBeforeWrite)) {
			t.Fatalf("partial payload missing per-file states: %s", rbRes.Content)
		}
		if files, ok := rbPayload["publishedFiles"].([]any); !ok || len(files) != 1 {
			t.Fatalf("publishedFiles must list only new_a.txt, got %+v", rbPayload["publishedFiles"])
		}
		if strings.Contains(rbRes.Content, root) || strings.Contains(rbRes.Content, "secret-a-body") {
			t.Fatalf("payload leaked absolute path or file contents: %s", rbRes.Content)
		}
		if got, rerr := os.ReadFile(filepath.Join(root, "new_a.txt")); rerr != nil || string(got) != "secret-a-body\n" {
			t.Fatalf("the published prefix was lost: %q err=%v", got, rerr)
		}
		if _, serr := os.Stat(filepath.Join(root, "new_b.txt")); !os.IsNotExist(serr) {
			t.Fatalf("the failed create must not exist: %v", serr)
		}

		// 1c: Partial publication with external append before Close EIO on b.txt.
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{
			AfterInPlaceWriteClose: func(dst string) error {
				if filepath.Base(dst) == "b.txt" {
					f, oerr := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0)
					if oerr == nil {
						_, _ = f.WriteString("FOREIGN\n")
						_ = f.Close()
					}
					return syscall.EIO
				}
				return nil
			},
		})
		partRes, partErr := set.Execute(context.Background(), "apply_patch", map[string]any{
			"patches": []any{
				map[string]any{"path": "a.txt", "operation": "update", "edits": []any{map[string]any{"old_text": "old-a", "new_text": "new-a"}}},
				map[string]any{"path": "b.txt", "operation": "update", "edits": []any{map[string]any{"old_text": "old-b", "new_text": "new-b"}}},
			},
		}, nil)
		eng.SetPublishFaultHooks(editing.PublishFaultHooks{})
		if partErr == nil || !partRes.MutatesWorkspace {
			t.Fatalf("expected partial publication error, got res=%+v err=%v", partRes, partErr)
		}
		var partPayload map[string]any
		if err := json.Unmarshal([]byte(partRes.Content), &partPayload); err != nil {
			t.Fatalf("unmarshal partial payload: %v (%s)", err, partRes.Content)
		}
		if partPayload["commitState"] != string(editing.CommitStatePartiallyPublished) ||
			partPayload["dirtyFailure"] != true ||
			partPayload["partialPublication"] != true {
			t.Fatalf("unexpected partial classification: %+v", partPayload)
		}
		if strings.Contains(partRes.Content, root) || strings.Contains(partRes.Content, "FOREIGN") {
			t.Fatalf("partial payload leaked absolute path or disk contents: %s", partRes.Content)
		}
		if !strings.Contains(partRes.Content, string(editing.PublishSucceededAndVerified)) ||
			!strings.Contains(partRes.Content, string(editing.PublishUncertain)) {
			t.Fatalf("partial payload missing per-file states: %s", partRes.Content)
		}
		// R7 preserves every published name: a.txt keeps its verified bytes and b.txt keeps the
		// foreign append after the uncertain close.
		gotA, _ := os.ReadFile(filepath.Join(root, "a.txt"))
		gotB, _ := os.ReadFile(filepath.Join(root, "b.txt"))
		if string(gotA) != "new-a\n" || string(gotB) != "new-b\nFOREIGN\n" {
			t.Fatalf("disk mismatch: a=%q b=%q", gotA, gotB)
		}
	})

	// 2. Preserves joined rollback error even when primary error is an *editing.Error (CodeConflict).
	t.Run("editing_error_preserves_joined_rollback_error_without_host_paths", func(t *testing.T) {
		root := t.TempDir()
		for _, n := range []string{"a.txt", "b.txt", "c.txt"} {
			if err := os.WriteFile(filepath.Join(root, n), []byte("old-"+n+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		eng, err := editing.New(editing.Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, Editing: eng, SessionID: "s-r5-join"})
		if err != nil {
			t.Fatal(err)
		}

		// Simulate editing.Error joined with a rollback error via commitOutcome directly.
		primary := &editing.Error{Code: editing.CodeConflict, Path: "c.txt", Detail: "changed after validation"}
		rbErr := fmt.Errorf("rollback b.txt (rollback_failed): write %s: %w", filepath.Join(root, "b.txt"), syscall.EROFS)
		joined := fmt.Errorf("%w\n%w", primary, rbErr)
		receipt := editing.CommitReceipt{
			Committed:    false,
			DirtyFailure: true,
			State:        editing.CommitStatePartiallyPublished,
			Files: []editing.FileCommitReceipt{
				{Path: "a.txt", AbsPath: filepath.Join(root, "a.txt"), Published: true, RolledBack: true, PublishState: editing.PublishRolledBack},
				{Path: "b.txt", AbsPath: filepath.Join(root, "b.txt"), Published: true, Wrote: true, PublishState: editing.PublishRollbackFailed, Error: "write b.txt: read-only file system"},
				{Path: "c.txt", AbsPath: filepath.Join(root, "c.txt"), PublishState: editing.PublishNone, Error: primary.Error()},
			},
		}
		res, outErr := set.commitOutcome(editing.Result{
			CommitState: editing.CommitStatePartiallyPublished,
			Files: []editing.FileResult{{
				Path: "b.txt", Published: true, PublishState: editing.PublishRollbackFailed, Error: "write b.txt: read-only file system",
			}},
		}, receipt, joined)
		if outErr != nil || res.OK || !res.MutatesWorkspace {
			t.Fatalf("unexpected commitOutcome: res=%+v err=%v", res, outErr)
		}
		if !strings.Contains(res.Content, "changed after validation") || !strings.Contains(res.Content, "read-only file system") {
			t.Fatalf("joined rollback error was dropped: %s", res.Content)
		}
		if strings.Contains(res.Content, root) {
			t.Fatalf("absolute host root leaked in payload: %s", res.Content)
		}
	})

	// 3. Large multi-file partial publication payload stays strictly bounded (<= 4096 bytes) and valid JSON.
	t.Run("large_multifile_failure_payload_is_bounded", func(t *testing.T) {
		root := t.TempDir()
		set, err := New(Options{Workspace: root, Policy: PolicyWorkspace, SessionID: "s-r5-bound"})
		if err != nil {
			t.Fatal(err)
		}
		rec := editing.CommitReceipt{
			Committed:    false,
			DirtyFailure: true,
			State:        editing.CommitStatePartiallyPublished,
		}
		var res editing.Result
		for i := 0; i < 60; i++ {
			rel := fmt.Sprintf("pkg/sub/file_%03d.go", i)
			rec.Files = append(rec.Files, editing.FileCommitReceipt{
				Path:         rel,
				AbsPath:      filepath.Join(root, rel),
				Published:    true,
				Wrote:        true,
				PublishState: editing.PublishRollbackFailed,
				Error:        "write " + rel + ": read-only file system",
			})
			res.Files = append(res.Files, editing.FileResult{
				Path:         rel,
				Published:    true,
				PublishState: editing.PublishRollbackFailed,
				Error:        "write " + rel + ": read-only file system",
			})
		}
		tr, _ := set.commitOutcome(res, rec, fmt.Errorf("failed committing %s: %w", filepath.Join(root, "pkg/sub/file_059.go"), syscall.EIO))
		if len(tr.Content) > maxPartialPayloadBytes {
			t.Fatalf("payload size %d exceeds bound %d", len(tr.Content), maxPartialPayloadBytes)
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(tr.Content), &parsed); err != nil {
			t.Fatalf("bounded payload invalid JSON: %v (%s)", err, tr.Content)
		}
		if omitted, ok := parsed["filesOmitted"].(float64); !ok || omitted <= 0 {
			t.Fatalf("expected filesOmitted > 0, got %+v", parsed)
		}
		if strings.Contains(tr.Content, root) {
			t.Fatalf("absolute host root leaked: %s", tr.Content)
		}
	})
}
