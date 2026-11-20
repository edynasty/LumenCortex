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

// TestR5ExternalProcessWriterHelper is a self-exec child-process helper invoked by
// TestR5SeparateOSProcessExternalModification so separate-PID filesystem writes are tested using a
// real child OS process without external shell tools.
func TestR5ExternalProcessWriterHelper(t *testing.T) {
	mode := os.Getenv("LC_R5_HELPER_MODE")
	target := os.Getenv("LC_R5_HELPER_TARGET")
	payload := os.Getenv("LC_R5_HELPER_PAYLOAD")
	if mode == "" || target == "" {
		return
	}
	switch mode {
	case "append":
		f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatalf("child append open: %v", err)
		}
		if _, err := f.WriteString(payload); err != nil {
			_ = f.Close()
			t.Fatalf("child append write: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("child append close: %v", err)
		}
	case "overwrite_inplace":
		f, err := os.OpenFile(target, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("child overwrite open: %v", err)
		}
		if _, err := f.WriteAt([]byte(payload), 0); err != nil {
			_ = f.Close()
			t.Fatalf("child overwrite write: %v", err)
		}
		if err := f.Truncate(int64(len(payload))); err != nil {
			_ = f.Close()
			t.Fatalf("child overwrite truncate: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("child overwrite close: %v", err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func runExternalWriterProcess(t *testing.T, mode, target, payload string) error {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestR5ExternalProcessWriterHelper$")
	cmd.Env = append(os.Environ(),
		"LC_R5_HELPER_MODE="+mode,
		"LC_R5_HELPER_TARGET="+target,
		"LC_R5_HELPER_PAYLOAD="+payload,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errors.New(string(out) + ": " + err.Error())
	}
	return nil
}

func findReceiptFile(files []FileCommitReceipt, rel string) (FileCommitReceipt, bool) {
	for _, f := range files {
		if f.Path == rel {
			return f, true
		}
	}
	return FileCommitReceipt{}, false
}

func findResultFile(files []FileResult, rel string) (FileResult, bool) {
	for _, f := range files {
		if f.Path == rel {
			return f, true
		}
	}
	return FileResult{}, false
}

// TestR5RollbackSafetyAndStateIntegrity covers ADR-0024 R5 findings R4-02-C, R4-02-A, and R4-02-B
// across single-file, multi-file, external modification, rollback failure, and separate OS process
// scenarios.
func TestR5RollbackSafetyAndStateIntegrity(t *testing.T) {
	// 1. Existing file write + Close succeeds.
	t.Run("1_existing_file_write_and_close_succeeds", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		e := newTestEngine(t, root)
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		next := []byte("new-a\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "a.txt", Content: &next}},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got := readFileContent(t, root, "a.txt"); got != "new-a\n" {
			t.Fatalf("disk = %q", got)
		}
		if !rec.Committed || rec.DirtyFailure || rec.State != CommitStateCommitted {
			t.Fatalf("receipt = %+v", rec)
		}
		if len(res.Files) != 1 || res.Files[0].PublishState != PublishSucceededAndVerified || res.Files[0].NewRev != RevOf(next) {
			t.Fatalf("res.Files = %+v", res.Files)
		}
	})

	// 2. Existing file write succeeds, Close fails (single-file): preserves written file, clears
	// NewRev/NewSHA256/SnapshotStore, and reports PublishSucceededWithPostError.
	t.Run("2_existing_file_write_succeeds_close_fails_single_file", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		e := newTestEngine(t, root)
		e.RegisterRead(testActor(), filepath.Join(root, "a.txt"), RevOf([]byte("old-a\n")))
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterInPlaceWriteClose: func(string) error { return syscall.EIO },
		})
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		next := []byte("new-a\n")
		res, err := e.Commit(ctx, Plan{
			Actor:   testActor(),
			Tool:    "write_file",
			Changes: []FileChange{{Path: "a.txt", Content: &next}},
		})
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v, want EIO", err)
		}
		if got := readFileContent(t, root, "a.txt"); got != "new-a\n" {
			t.Fatalf("disk = %q, want new-a\\n", got)
		}
		if rec.Committed || !rec.DirtyFailure || rec.State != CommitStatePublishedWithPostError {
			t.Fatalf("receipt = %+v", rec)
		}
		rf, ok := findReceiptFile(rec.Files, "a.txt")
		if !ok || !rf.Published || rf.Verified || rf.RolledBack || rf.PublishState != PublishSucceededWithPostError || rf.NewSHA256 != "" {
			t.Fatalf("receipt file = %+v", rf)
		}
		resF, ok := findResultFile(res.Files, "a.txt")
		if !ok || resF.PublishState != PublishSucceededWithPostError || resF.NewRev != "" {
			t.Fatalf("result file = %+v", resF)
		}
		if latest := e.LatestRev(testActor(), filepath.Join(root, "a.txt")); latest != "" {
			t.Fatalf("expected SnapshotStore cleared on post-close error, got rev %q", latest)
		}
	})

	// 3. External append before Close failure on existing file: MUST preserve external append in
	// both single-file and multi-file commits (R4-02-C).
	t.Run("3_external_append_before_close_failure_preserved_multifile", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b\n")
		e := newTestEngine(t, root)
		e.RegisterRead(testActor(), filepath.Join(root, "b.txt"), RevOf([]byte("old-b\n")))
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterInPlaceWriteClose: func(dst string) error {
				if filepath.Base(dst) == "b.txt" {
					f, oerr := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0)
					if oerr != nil {
						return oerr
					}
					_, _ = f.WriteString("FOREIGN\n")
					_ = f.Close()
					return syscall.EIO
				}
				return nil
			},
		})
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		aNext, bNext := []byte("new-a\n"), []byte("new-b\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aNext},
				{Path: "b.txt", Content: &bNext},
			},
		})
		if !errors.Is(err, syscall.EIO) {
			t.Fatalf("err = %v, want EIO", err)
		}
		if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\n" {
			t.Fatalf("a.txt = %q, want old-a\\n", gotA)
		}
		if gotB := readFileContent(t, root, "b.txt"); gotB != "new-b\nFOREIGN\n" {
			t.Fatalf("R4-02-C DATA LOSS: b.txt = %q, want new-b\\nFOREIGN\\n", gotB)
		}
		if !rec.DirtyFailure || rec.State != CommitStatePartiallyPublished {
			t.Fatalf("receipt = %+v", rec)
		}
		fa, _ := findReceiptFile(rec.Files, "a.txt")
		fb, _ := findReceiptFile(rec.Files, "b.txt")
		if fa.PublishState != PublishRolledBack || !fa.RolledBack || fa.NewSHA256 != sha256Ref([]byte("old-a\n"), true) {
			t.Fatalf("a.txt receipt = %+v", fa)
		}
		if fb.PublishState != PublishExternalModification || fb.Verified || fb.RolledBack || fb.NewSHA256 != "" {
			t.Fatalf("b.txt receipt = %+v", fb)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "b.txt" || res.Files[0].PublishState != PublishExternalModification || res.Files[0].NewRev != "" {
			t.Fatalf("res.Files = %+v", res.Files)
		}
		if latest := e.LatestRev(testActor(), filepath.Join(root, "b.txt")); latest != "" {
			t.Fatalf("expected SnapshotStore cleared for externally modified b.txt, got %q", latest)
		}
	})

	// 4. External atomic replacement of existing file before rollback -> preserved untouched.
	t.Run("4_external_replacement_before_rollback_preserved", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b\n")
		e := newTestEngine(t, root)
		e.beforeWrite = func(p string) {
			if filepath.Base(p) == "b.txt" {
				aPath := filepath.Join(root, "a.txt")
				tmp := filepath.Join(root, ".foreign-a")
				_ = os.WriteFile(tmp, []byte("new-a\n"), 0o644) // even identical bytes on a new inode!
				_ = os.Rename(tmp, aPath)
				_ = os.WriteFile(p, []byte("foreign-b\n"), 0o644)
			}
		}
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		aNext, bNext := []byte("new-a\n"), []byte("new-b\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aNext},
				{Path: "b.txt", Content: &bNext},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "external_modification_detected") {
			t.Fatalf("expected joined external_modification_detected error, got: %v", err)
		}
		if gotA := readFileContent(t, root, "a.txt"); gotA != "new-a\n" {
			t.Fatalf("externally replaced a.txt overwritten: %q", gotA)
		}
		fa, _ := findReceiptFile(rec.Files, "a.txt")
		if fa.PublishState != PublishExternalModification || fa.Verified || fa.RolledBack || fa.NewSHA256 != "" {
			t.Fatalf("a.txt receipt = %+v", fa)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "a.txt" || res.Files[0].PublishState != PublishExternalModification || res.Files[0].NewRev != "" {
			t.Fatalf("res.Files = %+v", res.Files)
		}
	})

	// 5. External same-inode, same-length modification before rollback -> preserved untouched.
	t.Run("5_external_same_inode_same_length_before_rollback_preserved", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "OLD-AA\n")
		writeFile(t, root, "b.txt", "OLD-BB\n")
		e := newTestEngine(t, root)
		e.beforeWrite = func(p string) {
			if filepath.Base(p) == "b.txt" {
				aPath := filepath.Join(root, "a.txt")
				f, oerr := os.OpenFile(aPath, os.O_WRONLY, 0)
				if oerr == nil {
					_, _ = f.WriteAt([]byte("EXT-AA\n"), 0) // exact same inode & 7-byte length as "NEW-AA\n"
					_ = f.Close()
				}
				_ = os.WriteFile(p, []byte("CONFLICT-B\n"), 0o644)
			}
		}
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		aNext, bNext := []byte("NEW-AA\n"), []byte("NEW-BB\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aNext},
				{Path: "b.txt", Content: &bNext},
			},
		})
		if err == nil || !strings.Contains(err.Error(), "external_modification_detected") {
			t.Fatalf("err = %v", err)
		}
		if gotA := readFileContent(t, root, "a.txt"); gotA != "EXT-AA\n" {
			t.Fatalf("same-inode same-length external edit overwritten: got %q, want EXT-AA\\n", gotA)
		}
		fa, _ := findReceiptFile(rec.Files, "a.txt")
		if fa.PublishState != PublishExternalModification || fa.Verified || fa.NewSHA256 != "" {
			t.Fatalf("a.txt receipt = %+v", fa)
		}
		if len(res.Files) != 1 || res.Files[0].NewRev != "" || res.Files[0].PublishState != PublishExternalModification {
			t.Fatalf("res.Files = %+v", res.Files)
		}
	})

	// 6. External modification / fault between validation and restore (explicitly tests both
	// BeforeRestoreWrite refusal, AfterRestoreWrite post-write race detection, and documents the
	// residual POSIX/Windows check-then-write TOCTOU window).
	t.Run("6_validation_to_restore_window_and_post_restore_readback_detection", func(t *testing.T) {
		// 6a: BeforeRestoreWrite fault aborts restore without modifying a.txt.
		rootA := t.TempDir()
		writeFile(t, rootA, "a.txt", "old-a\n")
		writeFile(t, rootA, "b.txt", "old-b\n")
		eA := newTestEngine(t, rootA)
		eA.beforeWrite = func(p string) {
			if filepath.Base(p) == "b.txt" {
				_ = os.WriteFile(p, []byte("conflict-b\n"), 0o644)
			}
		}
		eA.SetPublishFaultHooks(PublishFaultHooks{
			BeforeRestoreWrite: func(string) error { return syscall.EACCES },
		})
		var recA CommitReceipt
		ctxA := WithCommitRecorder(context.Background(), func(r CommitReceipt) { recA = r })
		aNext, bNext := []byte("new-a\n"), []byte("new-b\n")
		_, errA := eA.Commit(ctxA, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aNext},
				{Path: "b.txt", Content: &bNext},
			},
		})
		if !errors.Is(errA, syscall.EACCES) {
			t.Fatalf("6a err = %v, want joined EACCES", errA)
		}
		if got := readFileContent(t, rootA, "a.txt"); got != "new-a\n" {
			t.Fatalf("6a a.txt = %q", got)
		}
		if fa, _ := findReceiptFile(recA.Files, "a.txt"); fa.PublishState != PublishRollbackFailed || fa.Verified || fa.NewSHA256 != "" {
			t.Fatalf("6a a.txt receipt = %+v", fa)
		}

		// 6b: External writer modifies file right after restore write, before post-restore readback.
		rootB := t.TempDir()
		writeFile(t, rootB, "a.txt", "old-a\n")
		writeFile(t, rootB, "b.txt", "old-b\n")
		eB := newTestEngine(t, rootB)
		eB.beforeWrite = func(p string) {
			if filepath.Base(p) == "b.txt" {
				_ = os.WriteFile(p, []byte("conflict-b\n"), 0o644)
			}
		}
		eB.SetPublishFaultHooks(PublishFaultHooks{
			AfterRestoreWrite: func(dst string, _ *os.File) error {
				f, oerr := os.OpenFile(dst, os.O_WRONLY|os.O_APPEND, 0)
				if oerr == nil {
					_, _ = f.WriteString("LATE-RACE\n")
					_ = f.Close()
				}
				return nil
			},
		})
		var recB CommitReceipt
		ctxB := WithCommitRecorder(context.Background(), func(r CommitReceipt) { recB = r })
		resB, errB := eB.Commit(ctxB, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aNext},
				{Path: "b.txt", Content: &bNext},
			},
		})
		if errB == nil || !strings.Contains(errB.Error(), "external_modification_detected") {
			t.Fatalf("6b err = %v", errB)
		}
		if got := readFileContent(t, rootB, "a.txt"); got != "old-a\nLATE-RACE\n" {
			t.Fatalf("6b disk = %q", got)
		}
		if fa, _ := findReceiptFile(recB.Files, "a.txt"); fa.PublishState != PublishExternalModification || fa.Verified || fa.RolledBack || fa.NewSHA256 != "" {
			t.Fatalf("6b a.txt receipt = %+v", fa)
		}
		if len(resB.Files) != 1 || resB.Files[0].PublishState != PublishExternalModification || resB.Files[0].NewRev != "" {
			t.Fatalf("6b resB.Files = %+v", resB.Files)
		}
	})

	// 7. Rollback fails mid-write (simulated partial truncation + ENOSPC) and via onRollback hook:
	// MUST preserve both errors, never report PublishSucceededAndVerified, and clear SnapshotStore.
	t.Run("7_rollback_failure_truncated_disk_preserves_errors_and_clears_state", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b\n")
		writeFile(t, root, "c.txt", "old-c\n")
		e := newTestEngine(t, root)
		e.RegisterRead(testActor(), filepath.Join(root, "b.txt"), RevOf([]byte("old-b\n")))
		e.beforeWrite = func(p string) {
			if filepath.Base(p) == "c.txt" {
				_ = os.WriteFile(p, []byte("conflict-c\n"), 0o644)
			}
		}
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterRestoreWrite: func(dst string, f *os.File) error {
				if filepath.Base(dst) == "b.txt" {
					_ = f.Truncate(0)
					return syscall.ENOSPC
				}
				return nil
			},
		})
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		aNext, bNext, cNext := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
		res, err := e.Commit(ctx, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aNext},
				{Path: "b.txt", Content: &bNext},
				{Path: "c.txt", Content: &cNext},
			},
		})
		if err == nil {
			t.Fatal("expected error")
		}
		if CodeOf(err) != CodeConflict {
			t.Fatalf("expected original CodeConflict preserved via errors.As, got code=%q err=%v", CodeOf(err), err)
		}
		if !errors.Is(err, syscall.ENOSPC) {
			t.Fatalf("expected rollback ENOSPC preserved via errors.Is, got err=%v", err)
		}
		if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\n" {
			t.Fatalf("a.txt = %q, want old-a\\n", gotA)
		}
		if gotB := readFileContent(t, root, "b.txt"); gotB != "" {
			t.Fatalf("b.txt = %q, want truncated empty", gotB)
		}
		fa, _ := findReceiptFile(rec.Files, "a.txt")
		fb, _ := findReceiptFile(rec.Files, "b.txt")
		fc, _ := findReceiptFile(rec.Files, "c.txt")
		if fa.PublishState != PublishRolledBack || !fa.RolledBack {
			t.Fatalf("a.txt receipt = %+v", fa)
		}
		if fb.PublishState != PublishRollbackFailed || fb.Verified || fb.RolledBack || fb.NewSHA256 != "" || fb.Error == "" {
			t.Fatalf("b.txt receipt = %+v", fb)
		}
		if fc.PublishState != PublishNone || fc.Published {
			t.Fatalf("c.txt receipt = %+v", fc)
		}
		if len(res.Files) != 1 || res.Files[0].Path != "b.txt" || res.Files[0].PublishState != PublishRollbackFailed || res.Files[0].NewRev != "" {
			t.Fatalf("res.Files = %+v", res.Files)
		}
		if latest := e.LatestRev(testActor(), filepath.Join(root, "b.txt")); latest != "" {
			t.Fatalf("SnapshotStore retained stale rev %q for truncated b.txt", latest)
		}
	})

	// 8. Separate OS process writes externally before Close failure AND before rollback.
	t.Run("8_separate_os_process_external_modifications_preserved", func(t *testing.T) {
		// 8a: Child PID appends to b.txt before b.txt Close fails in multi-file commit.
		root1 := t.TempDir()
		writeFile(t, root1, "a.txt", "old-a\n")
		writeFile(t, root1, "b.txt", "old-b\n")
		e1 := newTestEngine(t, root1)
		e1.SetPublishFaultHooks(PublishFaultHooks{
			AfterInPlaceWriteClose: func(dst string) error {
				if filepath.Base(dst) == "b.txt" {
					if herr := runExternalWriterProcess(t, "append", dst, "CHILD-PID-APPEND\n"); herr != nil {
						t.Errorf("child helper failed: %v", herr)
					}
					return syscall.EIO
				}
				return nil
			},
		})
		var rec1 CommitReceipt
		ctx1 := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec1 = r })
		aNext, bNext := []byte("new-a\n"), []byte("new-b\n")
		_, err1 := e1.Commit(ctx1, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &aNext},
				{Path: "b.txt", Content: &bNext},
			},
		})
		if !errors.Is(err1, syscall.EIO) {
			t.Fatalf("8a err = %v", err1)
		}
		if gotA := readFileContent(t, root1, "a.txt"); gotA != "old-a\n" {
			t.Fatalf("8a a.txt = %q", gotA)
		}
		if gotB := readFileContent(t, root1, "b.txt"); gotB != "new-b\nCHILD-PID-APPEND\n" {
			t.Fatalf("8a child PID append lost: got %q", gotB)
		}
		if fb, _ := findReceiptFile(rec1.Files, "b.txt"); fb.PublishState != PublishExternalModification {
			t.Fatalf("8a b.txt receipt = %+v", fb)
		}

		// 8b: Child PID performs same-inode same-length overwrite on a.txt before c.txt conflict.
		root2 := t.TempDir()
		writeFile(t, root2, "a.txt", "OLD-AA\n")
		writeFile(t, root2, "b.txt", "OLD-BB\n")
		e2 := newTestEngine(t, root2)
		e2.beforeWrite = func(p string) {
			if filepath.Base(p) == "b.txt" {
				if herr := runExternalWriterProcess(t, "overwrite_inplace", filepath.Join(root2, "a.txt"), "PID-AA\n"); herr != nil {
					t.Errorf("child helper failed: %v", herr)
				}
				_ = os.WriteFile(p, []byte("CONFLICT-BB\n"), 0o644)
			}
		}
		var rec2 CommitReceipt
		ctx2 := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec2 = r })
		a2, b2 := []byte("NEW-AA\n"), []byte("NEW-BB\n")
		_, err2 := e2.Commit(ctx2, Plan{
			Actor: testActor(),
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &a2},
				{Path: "b.txt", Content: &b2},
			},
		})
		if err2 == nil || !strings.Contains(err2.Error(), "external_modification_detected") {
			t.Fatalf("8b err = %v", err2)
		}
		if gotA := readFileContent(t, root2, "a.txt"); gotA != "PID-AA\n" {
			t.Fatalf("8b child PID overwrite clobbered: got %q, want PID-AA\\n", gotA)
		}
		if fa, _ := findReceiptFile(rec2.Files, "a.txt"); fa.PublishState != PublishExternalModification {
			t.Fatalf("8b a.txt receipt = %+v", fa)
		}
	})

	// 9. Multi-file transaction state matrix (§6.4 scenarios a..f).
	t.Run("9_multifile_state_matrix_all_six_scenarios", func(t *testing.T) {
		// (a) A restored, B restore failed (via onRollback), C never published.
		t.Run("a_A_restored_B_rollback_failed_C_not_published", func(t *testing.T) {
			root := t.TempDir()
			for _, n := range []string{"a", "b", "c"} {
				writeFile(t, root, n+".txt", "old-"+n+"\n")
			}
			e := newTestEngine(t, root)
			e.beforeWrite = func(p string) {
				if filepath.Base(p) == "c.txt" {
					_ = os.WriteFile(p, []byte("conflict-c\n"), 0o644)
				}
			}
			e.SetTestFaultHooks(nil, func(p string) error {
				if filepath.Base(p) == "b.txt" {
					return errors.New("injected b rollback error")
				}
				return nil
			})
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
			res, err := e.Commit(ctx, Plan{
				Actor: testActor(),
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &na},
					{Path: "b.txt", Content: &nb},
					{Path: "c.txt", Content: &nc},
				},
			})
			if err == nil || !strings.Contains(err.Error(), "injected b rollback error") {
				t.Fatalf("err = %v", err)
			}
			if rec.State != CommitStatePartiallyPublished || !rec.DirtyFailure {
				t.Fatalf("rec = %+v", rec)
			}
			fa, _ := findReceiptFile(rec.Files, "a.txt")
			fb, _ := findReceiptFile(rec.Files, "b.txt")
			fc, _ := findReceiptFile(rec.Files, "c.txt")
			if fa.PublishState != PublishRolledBack || fb.PublishState != PublishRollbackFailed || fc.PublishState != PublishNone {
				t.Fatalf("states: a=%s b=%s c=%s", fa.PublishState, fb.PublishState, fc.PublishState)
			}
			if len(res.Files) != 1 || res.Files[0].Path != "b.txt" || res.Files[0].NewRev != "" {
				t.Fatalf("res.Files = %+v", res.Files)
			}
		})

		// (b) A restored, B externally modified, C never published.
		t.Run("b_A_restored_B_externally_modified_C_not_published", func(t *testing.T) {
			root := t.TempDir()
			for _, n := range []string{"a", "b", "c"} {
				writeFile(t, root, n+".txt", "old-"+n+"\n")
			}
			e := newTestEngine(t, root)
			e.beforeWrite = func(p string) {
				if filepath.Base(p) == "c.txt" {
					f, oerr := os.OpenFile(filepath.Join(root, "b.txt"), os.O_WRONLY|os.O_APPEND, 0)
					if oerr == nil {
						_, _ = f.WriteString("EXT-B\n")
						_ = f.Close()
					}
					_ = os.WriteFile(p, []byte("conflict-c\n"), 0o644)
				}
			}
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
			_, err := e.Commit(ctx, Plan{
				Actor: testActor(),
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &na},
					{Path: "b.txt", Content: &nb},
					{Path: "c.txt", Content: &nc},
				},
			})
			if err == nil || !strings.Contains(err.Error(), "external_modification_detected") {
				t.Fatalf("err = %v", err)
			}
			if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\n" {
				t.Fatalf("a.txt = %q", gotA)
			}
			if gotB := readFileContent(t, root, "b.txt"); gotB != "new-b\nEXT-B\n" {
				t.Fatalf("b.txt = %q", gotB)
			}
			fa, _ := findReceiptFile(rec.Files, "a.txt")
			fb, _ := findReceiptFile(rec.Files, "b.txt")
			fc, _ := findReceiptFile(rec.Files, "c.txt")
			if rec.State != CommitStatePartiallyPublished || fa.PublishState != PublishRolledBack || fb.PublishState != PublishExternalModification || fc.PublishState != PublishNone {
				t.Fatalf("rec = %+v", rec)
			}
		})

		// (c) A published, B published, C failed, A rollback succeeds, B rollback fails (real I/O error via BeforeRestoreWrite).
		t.Run("c_A_and_B_published_C_fails_A_rollback_succeeds_B_rollback_fails", func(t *testing.T) {
			root := t.TempDir()
			for _, n := range []string{"a", "b", "c"} {
				writeFile(t, root, n+".txt", "old-"+n+"\n")
			}
			e := newTestEngine(t, root)
			e.beforeWrite = func(p string) {
				if filepath.Base(p) == "c.txt" {
					_ = os.WriteFile(p, []byte("conflict-c\n"), 0o644)
				}
			}
			e.SetPublishFaultHooks(PublishFaultHooks{
				BeforeRestoreWrite: func(dst string) error {
					if filepath.Base(dst) == "b.txt" {
						return syscall.EROFS
					}
					return nil
				},
			})
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
			res, err := e.Commit(ctx, Plan{
				Actor: testActor(),
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &na},
					{Path: "b.txt", Content: &nb},
					{Path: "c.txt", Content: &nc},
				},
			})
			if err == nil || CodeOf(err) != CodeConflict || !errors.Is(err, syscall.EROFS) {
				t.Fatalf("expected joined CodeConflict + EROFS, got err=%v", err)
			}
			if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\n" {
				t.Fatalf("a.txt = %q", gotA)
			}
			if gotB := readFileContent(t, root, "b.txt"); gotB != "new-b\n" {
				t.Fatalf("b.txt = %q", gotB)
			}
			fa, _ := findReceiptFile(rec.Files, "a.txt")
			fb, _ := findReceiptFile(rec.Files, "b.txt")
			fc, _ := findReceiptFile(rec.Files, "c.txt")
			if rec.State != CommitStatePartiallyPublished || fa.PublishState != PublishRolledBack || fb.PublishState != PublishRollbackFailed || fb.Verified || fc.PublishState != PublishNone {
				t.Fatalf("rec = %+v", rec)
			}
			if len(res.Files) != 1 || res.Files[0].Path != "b.txt" || res.Files[0].PublishState != PublishRollbackFailed || res.Files[0].NewRev != "" {
				t.Fatalf("res.Files = %+v", res.Files)
			}
		})

		// (d) All files cleanly restored -> CommitStateRolledBack (DirtyFailure=true, Result.Files=[]).
		t.Run("d_all_files_cleanly_rolled_back", func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a.txt", "old-a\n")
			writeFile(t, root, "c.txt", "old-c\n")
			e := newTestEngine(t, root)
			e.beforeWrite = func(p string) {
				if filepath.Base(p) == "c.txt" {
					_ = os.WriteFile(p, []byte("conflict-c\n"), 0o644)
				}
			}
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
			res, err := e.Commit(ctx, Plan{
				Actor: testActor(),
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &na},
					{Path: "b.txt", Content: &nb, Create: true},
					{Path: "c.txt", Content: &nc},
				},
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if rec.State != CommitStateRolledBack || !rec.DirtyFailure || len(res.Files) != 0 {
				t.Fatalf("rec=%+v res=%+v", rec, res)
			}
			if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\n" {
				t.Fatalf("a.txt = %q", gotA)
			}
			if _, serr := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(serr) {
				t.Fatalf("b.txt should be unlinked, stat=%v", serr)
			}
		})

		// (e) All files successfully published -> CommitStateCommitted.
		t.Run("e_all_files_committed", func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a.txt", "old-a\n")
			writeFile(t, root, "d.txt", "del-d\n")
			e := newTestEngine(t, root)
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			na, nb := []byte("new-a\n"), []byte("new-b\n")
			res, err := e.Commit(ctx, Plan{
				Actor: testActor(),
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &na},
					{Path: "b.txt", Content: &nb, Create: true},
					{Path: "d.txt", Delete: true},
				},
			})
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if !rec.Committed || rec.DirtyFailure || rec.State != CommitStateCommitted || len(res.Files) != 3 {
				t.Fatalf("rec=%+v res=%+v", rec, res)
			}
		})

		// (f) Mixed created + updated + deleted where deleted file is recreated externally before
		// rollback -> external recreated file is preserved and classified PublishExternalModification.
		t.Run("f_mixed_create_update_delete_externally_recreated_deleted_file_preserved", func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a.txt", "old-a\n")
			writeFile(t, root, "c_del.txt", "old-del\n")
			writeFile(t, root, "d_fail.txt", "old-fail\n")
			e := newTestEngine(t, root)
			e.beforeWrite = func(p string) {
				if filepath.Base(p) == "d_fail.txt" {
					// Recreate the already-deleted c_del.txt externally before d_fail.txt triggers rollback.
					_ = os.WriteFile(filepath.Join(root, "c_del.txt"), []byte("EXTERNALLY-RECREATED\n"), 0o644)
					_ = os.WriteFile(p, []byte("conflict-d\n"), 0o644)
				}
			}
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			na, nb, nd := []byte("new-a\n"), []byte("new-b\n"), []byte("new-d\n")
			res, err := e.Commit(ctx, Plan{
				Actor: testActor(),
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &na},
					{Path: "b_create.txt", Content: &nb, Create: true},
					{Path: "c_del.txt", Delete: true},
					{Path: "d_fail.txt", Content: &nd},
				},
			})
			if err == nil || !strings.Contains(err.Error(), "external_modification_detected") {
				t.Fatalf("err = %v", err)
			}
			if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\n" {
				t.Fatalf("a.txt = %q", gotA)
			}
			if _, serr := os.Stat(filepath.Join(root, "b_create.txt")); !os.IsNotExist(serr) {
				t.Fatalf("b_create.txt should be rolled back, stat=%v", serr)
			}
			if gotC := readFileContent(t, root, "c_del.txt"); gotC != "EXTERNALLY-RECREATED\n" {
				t.Fatalf("externally recreated c_del.txt overwritten: got %q", gotC)
			}
			fa, _ := findReceiptFile(rec.Files, "a.txt")
			fb, _ := findReceiptFile(rec.Files, "b_create.txt")
			fc, _ := findReceiptFile(rec.Files, "c_del.txt")
			fd, _ := findReceiptFile(rec.Files, "d_fail.txt")
			if rec.State != CommitStatePartiallyPublished ||
				fa.PublishState != PublishRolledBack ||
				fb.PublishState != PublishRolledBack ||
				fc.PublishState != PublishExternalModification ||
				fd.PublishState != PublishNone {
				t.Fatalf("states: commit=%s a=%s b=%s c=%s d=%s", rec.State, fa.PublishState, fb.PublishState, fc.PublishState, fd.PublishState)
			}
			if len(res.Files) != 1 || res.Files[0].Path != "c_del.txt" || res.Files[0].PublishState != PublishExternalModification || res.Files[0].NewRev != "" {
				t.Fatalf("res.Files = %+v", res.Files)
			}
		})
	})
}
