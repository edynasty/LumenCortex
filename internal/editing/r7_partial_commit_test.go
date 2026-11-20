package editing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// r7Files indexes one path across Result.Files and CommitReceipt.Files. The ADR-0024 R7 contract
// requires every resolved path, including a clean failed prefix and the untouched suffix, in both.
func r7Files(t *testing.T, res Result, rec CommitReceipt, rel string) (FileResult, FileCommitReceipt) {
	t.Helper()
	fr, ok := findResultFile(res.Files, rel)
	if !ok {
		t.Fatalf("Result.Files missing %s: %+v", rel, res.Files)
	}
	rc, ok := findReceiptFile(rec.Files, rel)
	if !ok {
		t.Fatalf("CommitReceipt.Files missing %s: %+v", rel, rec.Files)
	}
	if fr.Path != rc.Path || fr.PublishState != rc.PublishState || fr.Published != rc.Published ||
		fr.Verified != rc.Verified {
		t.Fatalf("result/receipt disagree for %s: %+v vs %+v", rel, fr, rc)
	}
	return fr, rc
}

func r7AssertSettledCount(t *testing.T, res Result, rec CommitReceipt, want int) {
	t.Helper()
	if len(res.Files) != want || len(rec.Files) != want {
		t.Fatalf("settled paths: result=%d receipt=%d, want %d each", len(res.Files), len(rec.Files), want)
	}
}

func r7AssertNotAttempted(t *testing.T, fr FileResult, rc FileCommitReceipt) {
	t.Helper()
	if fr.PublishState != PublishNotAttempted || rc.PublishState != PublishNotAttempted {
		t.Fatalf("not-attempted suffix reported as %s/%s", fr.PublishState, rc.PublishState)
	}
	if fr.Published || rc.Published || fr.Verified || rc.Verified || fr.NewRev != "" || rc.NewSHA256 != "" || fr.Error != "" || rc.Error != "" {
		t.Fatalf("not-attempted path claims an effect: %+v %+v", fr, rc)
	}
	if fr.RolledBack || rc.RolledBack {
		t.Fatalf("R7 must not report a rollback: %+v %+v", fr, rc)
	}
}

func r7AssertVerified(t *testing.T, fr FileResult, rc FileCommitReceipt) {
	t.Helper()
	if fr.PublishState != PublishSucceededAndVerified || !fr.Published || !fr.Verified {
		t.Fatalf("verified path: %+v", fr)
	}
	if rc.PublishState != PublishSucceededAndVerified || !rc.Published || !rc.Verified {
		t.Fatalf("verified receipt: %+v", rc)
	}
	if fr.Deleted {
		if fr.NewRev != "" || rc.NewSHA256 != "-" {
			t.Fatalf("verified delete claims a live revision: %+v %+v", fr, rc)
		}
	} else if fr.NewRev == "" || rc.NewSHA256 == "" {
		t.Fatalf("verified live path lacks revision/hash: %+v %+v", fr, rc)
	}
	if fr.RolledBack || rc.RolledBack {
		t.Fatalf("verified path claims rollback: %+v %+v", fr, rc)
	}
}

// TestR7PrefixExternalAppendBeforeLaterConflict is the reviewed 3+ path ordering: A is published
// and verified, an independent OS child appends to A, then B fails. A keeps the child's bytes but
// loses its trusted revision and hash; the snapshot is invalidated; C is never attempted.
func TestR7PrefixExternalAppendBeforeLaterConflict(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "b.txt", "old-b\n")
	writeFile(t, root, "c.txt", "old-c\n")
	e := newTestEngine(t, root)
	actor := testActor()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		raw, _ := os.ReadFile(filepath.Join(root, name))
		e.RegisterRead(actor, filepath.Join(root, name), RevOf(raw))
	}
	e.beforeWrite = func(path string) {
		if filepath.Base(path) == "b.txt" {
			runR6ExternalWriter(t, "append", filepath.Join(root, "a.txt"), "CHILD-APPEND\n")
			if err := os.WriteFile(path, []byte("child-b\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &na},
		{Path: "b.txt", Content: &nb},
		{Path: "c.txt", Content: &nc},
	}})
	if err == nil || CodeOf(err) != CodeConflict {
		t.Fatalf("want CodeConflict from B, got %v", err)
	}
	r7AssertSettledCount(t, res, rec, 3)
	if res.CommitState != CommitStatePartiallyPublished || rec.State != CommitStatePartiallyPublished || !rec.DirtyFailure || rec.Committed {
		t.Fatalf("state: result=%+v receipt=%+v", res, rec)
	}

	if got := readFileContent(t, root, "a.txt"); got != "new-a\nCHILD-APPEND\n" {
		t.Fatalf("foreign append lost: %q", got)
	}
	fa, ra := r7Files(t, res, rec, "a.txt")
	if fa.PublishState != PublishExternalModification || ra.PublishState != PublishExternalModification {
		t.Fatalf("A must be external after a child append: %+v %+v", fa, ra)
	}
	if !fa.Published || !ra.Published || !ra.Wrote || fa.Verified || ra.Verified || fa.NewRev != "" || ra.NewSHA256 != "" || fa.Error == "" {
		t.Fatalf("A external claims trusted output: %+v %+v", fa, ra)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
		t.Fatalf("A snapshot not invalidated: %q", got)
	}

	if got := readFileContent(t, root, "b.txt"); got != "child-b\n" {
		t.Fatalf("foreign B lost: %q", got)
	}
	fb, rb := r7Files(t, res, rec, "b.txt")
	if fb.PublishState != PublishExternalModification || fb.Published || rb.Wrote || fb.Verified || fb.NewRev != "" || rb.NewSHA256 != "" {
		t.Fatalf("B conflict with no LC write: %+v %+v", fb, rb)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "b.txt")); got != "" {
		t.Fatalf("B snapshot not invalidated: %q", got)
	}

	if got := readFileContent(t, root, "c.txt"); got != "old-c\n" {
		t.Fatalf("C was attempted: %q", got)
	}
	fc, rc := r7Files(t, res, rec, "c.txt")
	r7AssertNotAttempted(t, fc, rc)
	if got := e.LatestRev(actor, filepath.Join(root, "c.txt")); got != RevOf([]byte("old-c\n")) {
		t.Fatalf("C snapshot lost though untouched: %q", got)
	}
}

// TestR7PrefixExternalSameByteReplacement replaces A with the exact intended bytes but on a new
// inode. Bytes alone must not authorize a trusted revision; identity divergence is external.
func TestR7PrefixExternalSameByteReplacement(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "b.txt", "old-b\n")
	writeFile(t, root, "c.txt", "old-c\n")
	e := newTestEngine(t, root)
	actor := testActor()
	e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
	e.beforeWrite = func(path string) {
		if filepath.Base(path) == "b.txt" {
			// Same bytes as A's intended content, new inode via rename.
			runR6ExternalWriter(t, "replace", filepath.Join(root, "a.txt"), "new-a\n")
			if err := os.WriteFile(path, []byte("child-b\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &na},
		{Path: "b.txt", Content: &nb},
		{Path: "c.txt", Content: &nc},
	}})
	if err == nil || CodeOf(err) != CodeConflict {
		t.Fatalf("want B conflict, got %v", err)
	}
	r7AssertSettledCount(t, res, rec, 3)
	if res.CommitState != CommitStatePartiallyPublished || !rec.DirtyFailure {
		t.Fatalf("state: %+v %+v", res, rec)
	}
	fa, ra := r7Files(t, res, rec, "a.txt")
	if fa.PublishState != PublishExternalModification || !fa.Published || !ra.Wrote || fa.Verified || fa.NewRev != "" || ra.NewSHA256 != "" {
		t.Fatalf("same-byte new-inode replacement trusted: %+v %+v", fa, ra)
	}
	if got := readFileContent(t, root, "a.txt"); got != "new-a\n" {
		t.Fatalf("foreign same-byte replacement lost: %q", got)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
		t.Fatalf("A snapshot not invalidated: %q", got)
	}
	fc, rc := r7Files(t, res, rec, "c.txt")
	r7AssertNotAttempted(t, fc, rc)
}

// TestR7VerifiedPrefixUpdateCreateDelete keeps a confirmed prefix under both a clean and dirty
// failure. The published file is re-observed read-only at settlement and keeps its receipt.
func TestR7VerifiedPrefixUpdateCreateDelete(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, root string)
		change  FileChange
		verify  func(t *testing.T, root string, fr FileResult, rc FileCommitReceipt)
	}{
		{
			name:    "update",
			prepare: func(t *testing.T, root string) { writeFile(t, root, "a.txt", "old-a\n") },
			change:  FileChange{Path: "a.txt", Content: bytesPtr("new-a\n")},
			verify: func(t *testing.T, root string, fr FileResult, rc FileCommitReceipt) {
				if fr.Created || fr.Deleted || fr.NewRev != RevOf([]byte("new-a\n")) || rc.NewSHA256 != sha256Ref([]byte("new-a\n"), true) {
					t.Fatalf("update receipt: %+v %+v", fr, rc)
				}
				if got := readFileContent(t, root, "a.txt"); got != "new-a\n" {
					t.Fatalf("update disk: %q", got)
				}
			},
		},
		{
			name:    "create",
			prepare: func(t *testing.T, root string) {},
			change:  FileChange{Path: "a.txt", Content: bytesPtr("new-a\n"), Create: true},
			verify: func(t *testing.T, root string, fr FileResult, rc FileCommitReceipt) {
				if !fr.Created || fr.Deleted || fr.NewRev != RevOf([]byte("new-a\n")) || rc.NewSHA256 != sha256Ref([]byte("new-a\n"), true) {
					t.Fatalf("create receipt: %+v %+v", fr, rc)
				}
				if got := readFileContent(t, root, "a.txt"); got != "new-a\n" {
					t.Fatalf("create disk: %q", got)
				}
			},
		},
		{
			name:    "delete",
			prepare: func(t *testing.T, root string) { writeFile(t, root, "a.txt", "old-a\n") },
			change:  FileChange{Path: "a.txt", Delete: true},
			verify: func(t *testing.T, root string, fr FileResult, rc FileCommitReceipt) {
				if !fr.Deleted || fr.Created || fr.NewRev != "" || rc.NewSHA256 != "-" {
					t.Fatalf("delete receipt: %+v %+v", fr, rc)
				}
				if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("delete disk: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.prepare(t, root)
			writeFile(t, root, "b.txt", "old-b\n")
			writeFile(t, root, "c.txt", "old-c\n")
			e := newTestEngine(t, root)
			actor := testActor()
			if tc.name == "delete" {
				e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
			}
			e.RegisterRead(actor, filepath.Join(root, "b.txt"), RevOf([]byte("old-b\n")))
			e.beforeWrite = func(path string) {
				if filepath.Base(path) == "b.txt" {
					if err := os.WriteFile(path, []byte("child-b\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			nb := []byte("new-b\n")
			res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
				tc.change,
				{Path: "b.txt", Content: &nb},
				{Path: "c.txt", Content: bytesPtr("new-c\n")},
			}})
			if err == nil || CodeOf(err) != CodeConflict {
				t.Fatalf("want B conflict, got %v", err)
			}
			r7AssertSettledCount(t, res, rec, 3)
			if res.CommitState != CommitStatePartiallyPublished || !rec.DirtyFailure {
				t.Fatalf("state: %+v %+v", res, rec)
			}
			fa, ra := r7Files(t, res, rec, "a.txt")
			r7AssertVerified(t, fa, ra)
			tc.verify(t, root, fa, ra)
			fb, rb := r7Files(t, res, rec, "b.txt")
			if fb.PublishState != PublishExternalModification || fb.Published || fb.Verified {
				t.Fatalf("B: %+v %+v", fb, rb)
			}
			fc, rc := r7Files(t, res, rec, "c.txt")
			r7AssertNotAttempted(t, fc, rc)
			if tc.name == "delete" {
				if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
					t.Fatalf("verified delete kept snapshot %q", got)
				}
			}
		})
	}
}

// TestR7CleanFailureSettlesEveryPathAndKeepsSnapshots is a failure that never entered the write
// window: no path is claimed written, DirtyFailure is false, and untouched snapshots stay valid.
func TestR7CleanFailureSettlesEveryPathAndKeepsSnapshots(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "old-a\n")
	writeFile(t, root, "b.txt", "old-b\n")
	writeFile(t, root, "c.txt", "old-c\n")
	e := newTestEngine(t, root)
	actor := testActor()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		raw, _ := os.ReadFile(filepath.Join(root, name))
		e.RegisterRead(actor, filepath.Join(root, name), RevOf(raw))
	}
	e.beforeWrite = func(path string) {
		if filepath.Base(path) == "a.txt" {
			if err := os.WriteFile(path, []byte("child-a\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: bytesPtr("new-a\n")},
		{Path: "b.txt", Content: bytesPtr("new-b\n")},
		{Path: "c.txt", Delete: true},
	}})
	if err == nil || CodeOf(err) != CodeConflict {
		t.Fatalf("want conflict, got %v", err)
	}
	r7AssertSettledCount(t, res, rec, 3)
	if res.CommitState != CommitStatePrePublishFailure || rec.State != CommitStatePrePublishFailure ||
		rec.DirtyFailure || rec.Committed {
		t.Fatalf("clean failure: %+v %+v", res, rec)
	}
	fa, ra := r7Files(t, res, rec, "a.txt")
	if fa.PublishState != PublishExternalModification || fa.Published || ra.Wrote || fa.Verified || ra.NewSHA256 != "" {
		t.Fatalf("failed path claims a write: %+v %+v", fa, ra)
	}
	if got := readFileContent(t, root, "a.txt"); got != "child-a\n" {
		t.Fatalf("foreign A lost: %q", got)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
		t.Fatalf("failed A snapshot stayed trusted: %q", got)
	}
	for _, name := range []string{"b.txt", "c.txt"} {
		fr, rc := r7Files(t, res, rec, name)
		r7AssertNotAttempted(t, fr, rc)
		raw, _ := os.ReadFile(filepath.Join(root, name))
		if got := e.LatestRev(actor, filepath.Join(root, name)); got != RevOf(raw) {
			t.Fatalf("%s snapshot changed although not attempted: %q", name, got)
		}
	}
	if got := readFileContent(t, root, "b.txt"); got != "old-b\n" {
		t.Fatalf("B attempted: %q", got)
	}
	if got := readFileContent(t, root, "c.txt"); got != "old-c\n" {
		t.Fatalf("C attempted: %q", got)
	}
}

// TestR7NoOpVerification covers a committed no-op and a no-op prefix in a failed plan. A no-op is
// verified current bytes, not a publication, and never reports the new revision as written.
func TestR7NoOpVerification(t *testing.T) {
	t.Run("committed_noop", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "same\n")
		e := newTestEngine(t, root)
		actor := testActor()
		e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("same\n")))
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
			{Path: "a.txt", Content: bytesPtr("same\n")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !rec.Committed || res.CommitState != CommitStateCommitted || len(res.Files) != 1 || len(rec.Files) != 1 {
			t.Fatalf("commit: %+v %+v", res, rec)
		}
		fa, ra := r7Files(t, res, rec, "a.txt")
		if fa.PublishState != PublishNone || fa.Published || ra.Wrote || fa.Changed || !fa.Verified ||
			fa.NewRev != RevOf([]byte("same\n")) || ra.NewSHA256 != sha256Ref([]byte("same\n"), true) || !ra.Verified {
			t.Fatalf("no-op: %+v %+v", fa, ra)
		}
		if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != RevOf([]byte("same\n")) {
			t.Fatalf("no-op snapshot: %q", got)
		}
	})

	t.Run("noop_prefix_in_failed_plan", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "same\n")
		writeFile(t, root, "b.txt", "old-b\n")
		writeFile(t, root, "c.txt", "old-c\n")
		e := newTestEngine(t, root)
		actor := testActor()
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b.txt" {
				if err := os.WriteFile(path, []byte("child-b\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
			{Path: "a.txt", Content: bytesPtr("same\n")},
			{Path: "b.txt", Content: bytesPtr("new-b\n")},
			{Path: "c.txt", Content: bytesPtr("new-c\n")},
		}})
		if err == nil || CodeOf(err) != CodeConflict {
			t.Fatalf("want conflict, got %v", err)
		}
		r7AssertSettledCount(t, res, rec, 3)
		if rec.DirtyFailure || res.CommitState != CommitStatePrePublishFailure {
			t.Fatalf("no-op prefix must stay clean: %+v", rec)
		}
		fa, ra := r7Files(t, res, rec, "a.txt")
		if fa.PublishState != PublishNone || !fa.Verified || fa.Published || ra.Wrote || fa.Changed || ra.NewSHA256 != sha256Ref([]byte("same\n"), true) {
			t.Fatalf("no-op prefix: %+v %+v", fa, ra)
		}
		fc, rc := r7Files(t, res, rec, "c.txt")
		r7AssertNotAttempted(t, fc, rc)
	})
}

// TestR7DeleteVerification checks absence, the SHA absence sentinel, snapshot removal, and a
// refused delete that preserves the changed file.
func TestR7DeleteVerification(t *testing.T) {
	t.Run("committed_delete_observes_absence", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		e := newTestEngine(t, root)
		actor := testActor()
		e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
			{Path: "a.txt", Delete: true},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !rec.Committed || res.CommitState != CommitStateCommitted {
			t.Fatalf("delete: %+v %+v", res, rec)
		}
		fa, ra := r7Files(t, res, rec, "a.txt")
		if fa.PublishState != PublishSucceededAndVerified || !fa.Deleted || !fa.Published || !ra.Wrote || !fa.Verified ||
			fa.NewRev != "" || ra.NewSHA256 != "-" || fa.RolledBack || ra.RolledBack {
			t.Fatalf("delete receipt: %+v %+v", fa, ra)
		}
		if _, err := os.Stat(filepath.Join(root, "a.txt")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("deleted file still present: %v", err)
		}
		if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
			t.Fatalf("delete kept snapshot: %q", got)
		}
	})

	t.Run("external_change_refuses_delete_and_preserves_foreign_bytes", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b\n")
		e := newTestEngine(t, root)
		actor := testActor()
		e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "a.txt" {
				if err := os.WriteFile(path, []byte("child-a\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
			{Path: "a.txt", Delete: true},
			{Path: "b.txt", Content: bytesPtr("new-b\n")},
		}})
		if err == nil || CodeOf(err) != CodeConflict {
			t.Fatalf("want conflict, got %v", err)
		}
		r7AssertSettledCount(t, res, rec, 2)
		if rec.DirtyFailure || res.CommitState != CommitStatePrePublishFailure {
			t.Fatalf("clean delete refusal: %+v", rec)
		}
		fa, ra := r7Files(t, res, rec, "a.txt")
		if fa.PublishState != PublishExternalModification || fa.Deleted || fa.Published || ra.Wrote || fa.Verified || ra.NewSHA256 != "" {
			t.Fatalf("refused delete: %+v %+v", fa, ra)
		}
		if got := readFileContent(t, root, "a.txt"); got != "child-a\n" {
			t.Fatalf("foreign bytes lost: %q", got)
		}
		fb, rb := r7Files(t, res, rec, "b.txt")
		r7AssertNotAttempted(t, fb, rb)
		if got := readFileContent(t, root, "b.txt"); got != "old-b\n" {
			t.Fatalf("B attempted: %q", got)
		}
	})
}

// TestR7CreateContention covers a foreign name present before staging and one that appears after
// staging. Both must refuse, preserve the foreign bytes exactly, and never expose a partial body.
func TestR7CreateContention(t *testing.T) {
	foreign := []byte("FOREIGN CONTENT\n")
	for _, when := range []string{"before_staging", "at_publication"} {
		t.Run(when, func(t *testing.T) {
			root := t.TempDir()
			e := newTestEngine(t, root)
			actor := testActor()
			if when == "before_staging" {
				e.beforeWrite = func(path string) {
					if err := os.WriteFile(path, foreign, 0o600); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				e.SetPublishFaultHooks(PublishFaultHooks{BeforePublish: func(dst string) error {
					return os.WriteFile(dst, foreign, 0o600)
				}})
			}
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
				{Path: "a.txt", Content: bytesPtr("agent content\n"), Create: true},
			}})
			if err == nil || CodeOf(err) != CodeConflict {
				t.Fatalf("want conflict, got %v", err)
			}
			fa, ra := r7Files(t, res, rec, "a.txt")
			if fa.PublishState != PublishExternalModification || fa.Published || ra.Wrote || fa.Verified || ra.NewSHA256 != "" {
				t.Fatalf("contended create: %+v %+v", fa, ra)
			}
			if got, rerr := os.ReadFile(filepath.Join(root, "a.txt")); rerr != nil || !bytes.Equal(got, foreign) {
				t.Fatalf("foreign target: err=%v got=%q", rerr, got)
			}
			if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
				t.Fatalf("create conflict kept snapshot: %q", got)
			}
			assertNoCreateTempLeftovers(t, root)
		})
	}
}

// r7PlainHost is a Host with no AtomicFileStager extension. New-file publication must fail closed
// rather than fall back to a direct create whose destination could be visible partially.
type r7PlainHost struct{ LocalHost }

func TestR7CustomHostWithoutStagerIsUnsupported(t *testing.T) {
	root := t.TempDir()
	lh, err := NewLocalHost(root)
	if err != nil {
		t.Fatal(err)
	}
	host := &r7PlainHost{LocalHost: lh}
	e, err := New(Options{Root: root, Host: host})
	if err != nil {
		t.Fatal(err)
	}
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: testActor(), Host: host, Tool: "write_file", Changes: []FileChange{
		{Path: "new.txt", Content: bytesPtr("agent\n"), Create: true},
	}})
	if err == nil || CodeOf(err) != CodeUnsupportedFilesystem {
		t.Fatalf("custom host without stager: %v", err)
	}
	f, r := r7Files(t, res, rec, "new.txt")
	if f.PublishState != PublishUnsupported || r.PublishState != PublishUnsupported || f.Published || r.Wrote || r.NewSHA256 != "" {
		t.Fatalf("unsupported custom host: %+v %+v", f, r)
	}
	if _, err := os.Stat(filepath.Join(root, "new.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination partially visible: %v", err)
	}
	assertNoCreateTempLeftovers(t, root)
}

// TestR7ExplicitRecreateAfterDeleteUsesFreshCreate deletes, then recreates from a fresh Create plan
// with no stale revision. The new bytes replace absence atomically and the old snapshot stays gone.
func TestR7ExplicitRecreateAfterDeleteUsesFreshCreate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "first\n")
	e := newTestEngine(t, root)
	actor := testActor()
	var deleted CommitReceipt
	dctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { deleted = r })
	if _, err := e.Commit(dctx, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Delete: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if !deleted.Committed || deleted.State != CommitStateCommitted || len(deleted.Files) != 1 ||
		deleted.Files[0].PublishState != PublishSucceededAndVerified || !deleted.Files[0].Deleted || deleted.Files[0].NewSHA256 != "-" {
		t.Fatalf("seed delete receipt: %+v", deleted)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
		t.Fatalf("delete kept snapshot: %q", got)
	}
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
		{Path: "a.txt", Content: bytesPtr("second\n"), Create: true},
	}})
	if err != nil {
		t.Fatalf("fresh create after delete: %v", err)
	}
	if !rec.Committed || res.CommitState != CommitStateCommitted {
		t.Fatalf("recreate: %+v %+v", res, rec)
	}
	f, r := r7Files(t, res, rec, "a.txt")
	if f.PublishState != PublishSucceededAndVerified || !f.Created || f.Deleted || !f.Published || !f.Verified || r.NewSHA256 != sha256Ref([]byte("second\n"), true) {
		t.Fatalf("recreate receipt: %+v %+v", f, r)
	}
	if got := readFileContent(t, root, "a.txt"); got != "second\n" {
		t.Fatalf("recreated bytes: %q", got)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != RevOf([]byte("second\n")) {
		t.Fatalf("recreate snapshot: %q", got)
	}
}

// TestR7TimeBoundaryNoOpIdentifiesSameBytes ensures a same-byte plan on an existing file is a
// verified no-op, never a publication with a synthetic new revision.
func TestR7TimeBoundaryNoOpIdentifiesSameBytes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "stable\n")
	e := newTestEngine(t, root)
	oldTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
		{Path: "a.txt", Content: bytesPtr("stable\n")},
	}}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(filepath.Join(root, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatalf("no-op changed inode")
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("no-op rewrote mtime: %v -> %v", before.ModTime(), after.ModTime())
	}
}

func bytesPtr(s string) *[]byte {
	b := []byte(s)
	return &b
}
