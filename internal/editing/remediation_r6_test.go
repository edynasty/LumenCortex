package editing

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestIndependentR5AppendAfterRestoreWriteHelper(t *testing.T) {
	p := os.Getenv("LC_R5_ORDERING_TARGET")
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("FOREIGN\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Logf("external_pid=%d parent=%d", os.Getpid(), os.Getppid())
}

// TestIndependentR5AppendAfterRestoreWrite is the deterministic two-process reproduction for R5-01
// (external O_APPEND between restoration Write(original) and Truncate(len(original))).
func TestIndependentR5AppendAfterRestoreWrite(t *testing.T) {
	cases := []struct {
		name     string
		oldA     string
		newA     string
		wantDisk string
	}{
		{
			name:     "equal_length_r5_01_original",
			oldA:     "old-a\n",
			newA:     "new-a\n",
			wantDisk: "old-a\nFOREIGN\n",
		},
		{
			name:     "growing_restore_12_from_6",
			oldA:     "longer-old!\n",
			newA:     "new-a\n",
			wantDisk: "longer-old!\nFOREIGN\n",
		},
		{
			name:     "shrinking_restore_4_from_11",
			oldA:     "old\n",
			newA:     "longer-new\n",
			wantDisk: "old\ner-new\nFOREIGN\n",
		},
		{
			name:     "empty_original_0_from_6",
			oldA:     "",
			newA:     "new-a\n",
			wantDisk: "new-a\nFOREIGN\n",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a.txt", tc.oldA)
			writeFile(t, root, "b.txt", "old-b\n")
			aOrigInfo, err := os.Lstat(filepath.Join(root, "a.txt"))
			if err != nil {
				t.Fatal(err)
			}

			e := newTestEngine(t, root)
			actor := testActor()
			e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte(tc.oldA)))

			e.beforeWrite = func(path string) {
				if filepath.Base(path) == "b.txt" {
					if werr := os.WriteFile(path, []byte("conflict-b\n"), 0o600); werr != nil {
						t.Fatal(werr)
					}
				}
			}

			hit := false
			e.SetPublishFaultHooks(PublishFaultHooks{
				AfterRestorePayloadWrite: func(path string) error {
					if filepath.Base(path) != "a.txt" {
						return nil
					}
					hit = true
					cmd := exec.Command(os.Args[0], "-test.v", "-test.run=^TestIndependentR5AppendAfterRestoreWriteHelper$")
					cmd.Env = append(os.Environ(), "LC_R5_ORDERING_TARGET="+path)
					out, cmdErr := cmd.CombinedOutput()
					t.Logf("writer: %s", out)
					return cmdErr
				},
			})

			a := []byte(tc.newA)
			b := []byte("new-b\n")
			var receipt CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
			result, commitErr := e.Commit(ctx, Plan{
				Actor: actor,
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &a},
					{Path: "b.txt", Content: &b},
				},
			})
			if commitErr == nil || !hit {
				t.Fatalf("not reached: commitErr=%v hit=%t", commitErr, hit)
			}
			if CodeOf(commitErr) != CodeConflict {
				t.Fatalf("want primary CodeConflict, got %v", commitErr)
			}

			disk := readFileContent(t, root, "a.txt")
			lost := !strings.Contains(disk, "FOREIGN\n")
			if lost || disk != tc.wantDisk {
				t.Fatalf("R5-01 safety failure: disk=%q want=%q foreign_lost=%t", disk, tc.wantDisk, lost)
			}

			aAfterInfo, err := os.Lstat(filepath.Join(root, "a.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(aOrigInfo, aAfterInfo) {
				t.Fatalf("expected in-place inode preserved on a.txt")
			}

			if result.CommitState != CommitStatePartiallyPublished || receipt.State != CommitStatePartiallyPublished || !receipt.DirtyFailure || receipt.Committed {
				t.Fatalf("unexpected commit state: result=%+v receipt=%+v", result, receipt)
			}
			ra, ok := findReceiptFile(receipt.Files, "a.txt")
			if !ok || ra.PublishState != PublishExternalModification || ra.Verified || ra.RolledBack || ra.NewSHA256 != "" || ra.Error == "" {
				t.Fatalf("unexpected receipt for a.txt: %+v", ra)
			}
			rb, ok := findReceiptFile(receipt.Files, "b.txt")
			if !ok || rb.PublishState != PublishNone || rb.Verified || rb.Published || rb.NewSHA256 != "" {
				t.Fatalf("unexpected receipt for b.txt: %+v", rb)
			}
			if len(result.Files) != 1 || result.Files[0].Path != "a.txt" ||
				result.Files[0].PublishState != PublishExternalModification ||
				result.Files[0].RolledBack || result.Files[0].NewRev != "" {
				t.Fatalf("unexpected result.Files: %+v", result.Files)
			}
			if gotRev := e.LatestRev(actor, filepath.Join(root, "a.txt")); gotRev != "" {
				t.Fatalf("expected SnapshotStore cleared for a.txt, got rev=%q", gotRev)
			}
		})
	}
}

func TestR6ExternalWriterProcessHelper(t *testing.T) {
	mode := os.Getenv("LC_R6_WRITER_MODE")
	if mode == "" {
		return
	}
	path := os.Getenv("LC_R6_WRITER_PATH")
	data := []byte(os.Getenv("LC_R6_WRITER_DATA"))
	switch mode {
	case "append":
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(data); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	case "rewrite":
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(data, 0); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Truncate(int64(len(data))); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	case "truncate":
		if err := os.Truncate(path, 0); err != nil {
			t.Fatal(err)
		}
	case "replace":
		tmp := path + ".r6foreign"
		if err := os.WriteFile(tmp, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown LC_R6_WRITER_MODE=%q", mode)
	}
}

func runR6ExternalWriter(t *testing.T, mode, path, data string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestR6ExternalWriterProcessHelper$")
	cmd.Env = append(os.Environ(),
		"LC_R6_WRITER_MODE="+mode,
		"LC_R6_WRITER_PATH="+path,
		"LC_R6_WRITER_DATA="+data,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("external writer (%s) failed: %v\n%s", mode, err, out)
	}
}

func TestR6RollbackRaceAndFaultMatrix(t *testing.T) {
	type matrixCase struct {
		name        string
		oldA        string
		newA        string
		when        string
		mode        string
		foreignData string
		wantDisk    string
		wantState   PublishState
	}

	cases := []matrixCase{
		{
			name:        "01_append_before_validation",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "before_validation",
			mode:        "append",
			foreignData: "FOREIGN\n",
			wantDisk:    "new-a\nFOREIGN\n",
			wantState:   PublishExternalModification,
		},
		{
			name:        "02_append_after_validation_before_write",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "after_validation",
			mode:        "append",
			foreignData: "FOREIGN\n",
			wantDisk:    "old-a\nFOREIGN\n",
			wantState:   PublishExternalModification,
		},
		{
			name:        "03_append_after_write_before_truncate",
			oldA:        "old\n",
			newA:        "longer-new\n",
			when:        "after_payload_write",
			mode:        "append",
			foreignData: "FOREIGN\n",
			wantDisk:    "old\ner-new\nFOREIGN\n",
			wantState:   PublishExternalModification,
		},
		{
			name:        "04_append_after_readback_before_close",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "after_readback",
			mode:        "append",
			foreignData: "FOREIGN\n",
			wantDisk:    "old-a\nFOREIGN\n",
			wantState:   PublishExternalModification,
		},
		{
			name:        "05a_replacement_before_validation",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "before_validation",
			mode:        "replace",
			foreignData: "FOREIGN-REPLACED\n",
			wantDisk:    "FOREIGN-REPLACED\n",
			wantState:   PublishExternalModification,
		},
		{
			name:        "05b_replacement_between_validation_and_restore",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "after_validation",
			mode:        "replace",
			foreignData: "FOREIGN-REPLACED\n",
			wantDisk:    "FOREIGN-REPLACED\n",
			wantState:   PublishExternalModification,
		},
		{
			name:        "06_same_inode_modification_before_validation",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "before_validation",
			mode:        "rewrite",
			foreignData: "FORGN\n",
			wantDisk:    "FORGN\n",
			wantState:   PublishExternalModification,
		},
		{
			name:        "07a_external_truncate_before_validation",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "before_validation",
			mode:        "truncate",
			foreignData: "",
			wantDisk:    "",
			wantState:   PublishExternalModification,
		},
		{
			name:        "07b_external_truncate_after_readback",
			oldA:        "old-a\n",
			newA:        "new-a\n",
			when:        "after_readback",
			mode:        "truncate",
			foreignData: "",
			wantDisk:    "",
			wantState:   PublishExternalModification,
		},
		{
			name:      "08_rollback_write_fault_hook",
			oldA:      "old-a\n",
			newA:      "new-a\n",
			when:      "write_fault",
			wantDisk:  "new-a\n",
			wantState: PublishRollbackFailed,
		},
		{
			name:      "09a_rollback_close_failure",
			oldA:      "old-a\n",
			newA:      "new-a\n",
			when:      "close_failure",
			wantDisk:  "old-a\n",
			wantState: PublishRollbackFailed,
		},
		{
			name:      "09b_rollback_readback_descriptor_failure",
			oldA:      "old-a\n",
			newA:      "new-a\n",
			when:      "readback_failure",
			wantDisk:  "old-a\n",
			wantState: PublishRollbackFailed,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeFile(t, root, "a.txt", tc.oldA)
			writeFile(t, root, "b.txt", "old-b\n")
			e := newTestEngine(t, root)
			actor := testActor()
			target := filepath.Join(root, "a.txt")
			e.RegisterRead(actor, target, RevOf([]byte(tc.oldA)))

			e.beforeWrite = func(path string) {
				if filepath.Base(path) == "b.txt" {
					if err := os.WriteFile(path, []byte("conflict-b\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			e.onRollback = func(path string) error {
				if path == target && tc.when == "before_validation" {
					runR6ExternalWriter(t, tc.mode, target, tc.foreignData)
				}
				return nil
			}
			e.SetPublishFaultHooks(PublishFaultHooks{
				BeforeRestoreWrite: func(path string) error {
					if path != target {
						return nil
					}
					if tc.when == "after_validation" {
						runR6ExternalWriter(t, tc.mode, target, tc.foreignData)
					}
					if tc.when == "write_fault" {
						return syscall.EIO
					}
					return nil
				},
				AfterRestorePayloadWrite: func(path string) error {
					if path == target && tc.when == "after_payload_write" {
						runR6ExternalWriter(t, tc.mode, target, tc.foreignData)
					}
					return nil
				},
				AfterRestoreWrite: func(path string, f *os.File) error {
					if path == target && tc.when == "readback_failure" {
						return f.Close()
					}
					return nil
				},
				AfterRestoreReadback: func(path string, f *os.File) error {
					if path != target {
						return nil
					}
					if tc.when == "after_readback" {
						runR6ExternalWriter(t, tc.mode, target, tc.foreignData)
					}
					if tc.when == "close_failure" {
						return f.Close()
					}
					return nil
				},
			})

			a := []byte(tc.newA)
			b := []byte("new-b\n")
			var receipt CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
			result, err := e.Commit(ctx, Plan{
				Actor: actor,
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", Content: &a},
					{Path: "b.txt", Content: &b},
				},
			})
			if err == nil || CodeOf(err) != CodeConflict {
				t.Fatalf("want primary CodeConflict, got %v", err)
			}
			if !strings.Contains(err.Error(), "rollback a.txt") {
				t.Fatalf("expected joined rollback diagnostic in error, got %v", err)
			}

			gotDisk := readFileContent(t, root, "a.txt")
			if gotDisk != tc.wantDisk {
				t.Fatalf("disk mismatch: got %q, want %q", gotDisk, tc.wantDisk)
			}
			ra, ok := findReceiptFile(receipt.Files, "a.txt")
			if !ok {
				t.Fatal("missing receipt for a.txt")
			}
			if ra.PublishState != tc.wantState || ra.Verified || ra.RolledBack || ra.NewSHA256 != "" || ra.Error == "" {
				t.Fatalf("unexpected receipt for a.txt: %+v", ra)
			}
			rb, ok := findReceiptFile(receipt.Files, "b.txt")
			if !ok || rb.PublishState != PublishNone || rb.Verified || rb.NewSHA256 != "" {
				t.Fatalf("unexpected receipt for b.txt: %+v", rb)
			}
			if result.CommitState != CommitStatePartiallyPublished || receipt.State != CommitStatePartiallyPublished || !receipt.DirtyFailure {
				t.Fatalf("unexpected commit state: result=%+v receipt=%+v", result, receipt)
			}
			if len(result.Files) != 1 || result.Files[0].Path != "a.txt" || result.Files[0].NewRev != "" || result.Files[0].PublishState != tc.wantState {
				t.Fatalf("unexpected result.Files: %+v", result.Files)
			}
			if gotRev := e.LatestRev(actor, target); gotRev != "" {
				t.Fatalf("SnapshotStore retained stale rev %q for %s", gotRev, target)
			}
		})
	}

	t.Run("10_multifile_mixed_outcomes_and_unverified_conflict_sha", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b-longer\n")
		writeFile(t, root, "c.txt", "old-c\n")
		writeFile(t, root, "e.txt", "old-e\n")

		e := newTestEngine(t, root)
		actor := testActor()
		for _, n := range []string{"a.txt", "b.txt", "c.txt", "e.txt"} {
			raw, _ := os.ReadFile(filepath.Join(root, n))
			e.RegisterRead(actor, filepath.Join(root, n), RevOf(raw))
		}

		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "e.txt" {
				if err := os.WriteFile(path, []byte("foreign-e\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}
		e.SetPublishFaultHooks(PublishFaultHooks{
			AfterRestorePayloadWrite: func(path string) error {
				if filepath.Base(path) == "a.txt" {
					runR6ExternalWriter(t, "append", path, "FOREIGN-A\n")
				}
				return nil
			},
			BeforeRestoreWrite: func(path string) error {
				if filepath.Base(path) == "c.txt" {
					return syscall.EIO
				}
				return nil
			},
		})

		na := []byte("new-a\n")
		nb := []byte("new-b\n")
		nc := []byte("new-c\n")
		nd := []byte("created-d\n")
		ne := []byte("new-e\n")

		var receipt CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
		result, err := e.Commit(ctx, Plan{
			Actor: actor,
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a.txt", Content: &na},
				{Path: "b.txt", Content: &nb},
				{Path: "c.txt", Content: &nc},
				{Path: "d.txt", Content: &nd, Create: true},
				{Path: "e.txt", Content: &ne},
			},
		})
		if err == nil || CodeOf(err) != CodeConflict || !errors.Is(err, syscall.EIO) {
			t.Fatalf("expected primary CodeConflict joined with EIO, got %v", err)
		}

		if gotA := readFileContent(t, root, "a.txt"); gotA != "old-a\nFOREIGN-A\n" {
			t.Fatalf("a.txt mismatch: %q", gotA)
		}
		if gotB := readFileContent(t, root, "b.txt"); gotB != "old-b-longer\n" {
			t.Fatalf("b.txt mismatch: %q", gotB)
		}
		if gotC := readFileContent(t, root, "c.txt"); gotC != "new-c\n" {
			t.Fatalf("c.txt mismatch: %q", gotC)
		}
		if _, statErr := os.Lstat(filepath.Join(root, "d.txt")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expected created d.txt cleanly rolled back, statErr=%v", statErr)
		}
		if gotE := readFileContent(t, root, "e.txt"); gotE != "foreign-e\n" {
			t.Fatalf("e.txt mismatch: %q", gotE)
		}

		ra, _ := findReceiptFile(receipt.Files, "a.txt")
		rb, _ := findReceiptFile(receipt.Files, "b.txt")
		rc, _ := findReceiptFile(receipt.Files, "c.txt")
		rd, _ := findReceiptFile(receipt.Files, "d.txt")
		re, _ := findReceiptFile(receipt.Files, "e.txt")

		if ra.PublishState != PublishExternalModification || ra.Verified || ra.NewSHA256 != "" {
			t.Fatalf("bad ra: %+v", ra)
		}
		if rb.PublishState != PublishRolledBack || !rb.RolledBack || !rb.Verified || rb.NewSHA256 != sha256Ref([]byte("old-b-longer\n"), true) {
			t.Fatalf("bad rb: %+v", rb)
		}
		if rc.PublishState != PublishRollbackFailed || rc.Verified || rc.NewSHA256 != "" {
			t.Fatalf("bad rc: %+v", rc)
		}
		if rd.PublishState != PublishRolledBack || !rd.RolledBack || rd.NewSHA256 != "-" {
			t.Fatalf("bad rd: %+v", rd)
		}
		if re.PublishState != PublishNone || re.Verified || re.NewSHA256 != "" {
			t.Fatalf("expected CAS-conflicted e.txt to have empty unverified NewSHA256, got %+v", re)
		}
		if result.CommitState != CommitStatePartiallyPublished || len(result.Files) != 2 {
			t.Fatalf("unexpected result: %+v", result)
		}
	})

	t.Run("11_cross_process_concurrent_append_stress", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "base-line\n")
		writeFile(t, root, "b.txt", "old-b\n")
		e := newTestEngine(t, root)
		actor := testActor()
		targetA := filepath.Join(root, "a.txt")

		for iter := 0; iter < 12; iter++ {
			curA, err := os.ReadFile(targetA)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("old-b\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			foreignLine := fmt.Sprintf("FOREIGN-ITER-%02d\n", iter)
			e.beforeWrite = func(path string) {
				if filepath.Base(path) == "b.txt" {
					_ = os.WriteFile(path, []byte("conflict-b\n"), 0o600)
				}
			}
			// Alternate injection across validation, payload-write, and post-readback boundaries.
			phase := iter % 3
			e.onRollback = func(path string) error {
				if path == targetA && phase == 0 {
					runR6ExternalWriter(t, "append", targetA, foreignLine)
				}
				return nil
			}
			e.SetPublishFaultHooks(PublishFaultHooks{
				AfterRestorePayloadWrite: func(path string) error {
					if path == targetA && phase == 1 {
						runR6ExternalWriter(t, "append", targetA, foreignLine)
					}
					return nil
				},
				AfterRestoreReadback: func(path string, _ *os.File) error {
					if path == targetA && phase == 2 {
						runR6ExternalWriter(t, "append", targetA, foreignLine)
					}
					return nil
				},
			})

			nextA := append(append([]byte(nil), curA...), []byte(fmt.Sprintf("lc-edit-%02d-longer\n", iter))...)
			nextB := []byte("new-b\n")
			var receipt CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
			_, commitErr := e.Commit(ctx, Plan{
				Actor: actor,
				Tool:  "apply_patch",
				Changes: []FileChange{
					{Path: "a.txt", BaseRev: RevOf(curA), Content: &nextA},
					{Path: "b.txt", BaseRev: RevOf([]byte("old-b\n")), Content: &nextB},
				},
			})
			if commitErr == nil {
				t.Fatalf("iter %d: expected conflict", iter)
			}
			diskAfter := readFileContent(t, root, "a.txt")
			if !strings.Contains(diskAfter, foreignLine) {
				t.Fatalf("iter %d (phase=%d): lost foreign line %q in disk=%q", iter, phase, foreignLine, diskAfter)
			}
			ra, ok := findReceiptFile(receipt.Files, "a.txt")
			if !ok || ra.PublishState != PublishExternalModification || ra.RolledBack || ra.Verified {
				t.Fatalf("iter %d (phase=%d): false rollback receipt %+v", iter, phase, ra)
			}
		}

		finalDisk := readFileContent(t, root, "a.txt")
		for iter := 0; iter < 12; iter++ {
			want := fmt.Sprintf("FOREIGN-ITER-%02d\n", iter)
			if !strings.Contains(finalDisk, want) {
				t.Fatalf("missing %q in final disk %q", want, finalDisk)
			}
		}
	})
}

func TestR6ValidateRelativizesDiagnosticsAndSiblingPaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Sibling file in the same Go package on disk (not in Plan.Changes) with a broken reference once edited.
	writeFile(t, root, "pkg/helper.go", "package pkg\n\nfunc useVal() int { return sharedVal }\n")
	writeFile(t, root, "pkg/main.go", "package pkg\n\nvar sharedVal = 42\n")

	e := newTestEngine(t, root)
	broken := []byte("package pkg\n\nvar sharedVal = \"now-a-string\"\n")
	_, err := e.Commit(context.Background(), Plan{
		Actor:    testActor(),
		Tool:     "edit_lines",
		Changes:  []FileChange{{Path: "pkg/main.go", Content: &broken}},
		Validate: []Validator{GoSyntax(), GoTypes()},
	})
	editErr, ok := AsError(err)
	if !ok || editErr == nil {
		t.Fatalf("expected editing.Error from GoTypes, got %v", err)
	}
	if strings.Contains(editErr.Path, root) || filepath.IsAbs(editErr.Path) {
		t.Fatalf("editErr.Path leaked absolute host path: %q", editErr.Path)
	}
	if editErr.Path != "pkg/helper.go" {
		t.Fatalf("expected sibling diagnostic path pkg/helper.go, got %q", editErr.Path)
	}
	for _, d := range editErr.Diagnostics {
		if strings.Contains(d.Path, root) || filepath.IsAbs(d.Path) {
			t.Fatalf("Diagnostic.Path leaked absolute host path: %+v", d)
		}
	}

	// Verify sanitizeEngineError neither leaks host root nor corrupts outside-workspace sibling paths.
	p := &prepared{
		abs: filepath.Join(root, "pkg/main.go"),
		key: CanonicalPath(filepath.Join(root, "pkg/main.go")),
		rel: "pkg/main.go",
	}
	outsideSibling := fmt.Sprintf("open %s-sibling/secret: permission denied", root)
	gotOutside := e.sanitizeEngineError(p, errors.New(outsideSibling))
	if strings.Contains(gotOutside, ".-sibling") || strings.Contains(gotOutside, root) {
		t.Fatalf("sanitizeEngineError corrupted or leaked outside sibling path: %q", gotOutside)
	}
	if !strings.Contains(gotOutside, "<outside-workspace>/secret") {
		t.Fatalf("expected <outside-workspace>/secret in %q", gotOutside)
	}
}
