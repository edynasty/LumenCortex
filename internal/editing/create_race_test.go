package editing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateRaceForeignFilePreserved solidifies the F-01 review probe: when an external writer
// creates the target file between prepare ("file is absent") and writeCAS Open, the commit must
// fail cleanly with CodeConflict (DirtyFailure == false) and MUST NOT unlink the foreign file.
func TestCreateRaceForeignFilePreserved(t *testing.T) {
	cases := []struct {
		name   string
		change func(content *[]byte) FileChange
	}{
		{
			name: "content_allow_create_write_file",
			change: func(c *[]byte) FileChange {
				return FileChange{Path: "gen/out.txt", Content: c, AllowCreate: true}
			},
		},
		{
			name: "content_create_apply_patch",
			change: func(c *[]byte) FileChange {
				return FileChange{Path: "gen/out.txt", Content: c, Create: true}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			e, err := New(Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "gen", "out.txt")
			external := []byte("EXTERNAL WRITER CONTENT\n")
			e.beforeWrite = func(path string) {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, external, 0o644); err != nil {
					t.Fatal(err)
				}
			}

			var receipt CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) {
				receipt = r
			})
			content := []byte("agent content\n")
			_, commitErr := e.Commit(ctx, Plan{
				Actor:   Actor{Session: "s1"},
				Tool:    "write_file",
				Changes: []FileChange{tc.change(&content)},
			})
			if commitErr == nil {
				t.Fatalf("expected commit to be refused with CodeConflict")
			}
			got, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatalf("foreign file destroyed after refused create (%v): %v", commitErr, readErr)
			}
			if string(got) != string(external) {
				t.Fatalf("foreign file content mutated: got %q, want %q", got, external)
			}
			if CodeOf(commitErr) != CodeConflict {
				t.Fatalf("expected CodeConflict, got code=%q err=%v", CodeOf(commitErr), commitErr)
			}
			if receipt.DirtyFailure {
				t.Fatalf("expected DirtyFailure=false on O_EXCL refusal, got %+v", receipt)
			}
		})
	}
}

// TestCreateRaceOversizedForeignFilePreserved verifies the MaxFileBytes branch: if an external
// process creates an oversized (> MaxFileBytes) file before writeCAS opens it, writeCAS refuses
// via O_EXCL before ever reading or unlinking the foreign file.
func TestCreateRaceOversizedForeignFilePreserved(t *testing.T) {
	root := t.TempDir()
	e, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "gen", "huge.bin")
	oversized := make([]byte, MaxFileBytes+128)
	for i := range oversized {
		oversized[i] = 'X'
	}
	e.beforeWrite = func(path string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, oversized, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	content := []byte("small agent file\n")
	_, commitErr := e.Commit(context.Background(), Plan{
		Actor:   Actor{Session: "s1"},
		Tool:    "write_file",
		Changes: []FileChange{{Path: "gen/huge.bin", Content: &content, AllowCreate: true}},
	})
	if commitErr == nil {
		t.Fatalf("expected commit refusal when oversized foreign file exists")
	}
	info, statErr := os.Stat(target)
	if statErr != nil {
		t.Fatalf("oversized foreign file destroyed: %v", statErr)
	}
	if info.Size() != int64(len(oversized)) {
		t.Fatalf("oversized foreign file size changed: got %d, want %d", info.Size(), len(oversized))
	}
}

// TestCreateCleanupNeverDeletesReplacedFile verifies that even when O_CREATE|O_EXCL genuinely
// created the file, if another process unlinks and replaces that path before cleanup (during
// writeCAS failure or multi-file rollback), the replacement file is NOT deleted.
func TestCreateCleanupNeverDeletesReplacedFile(t *testing.T) {
	t.Run("writeCAS_failure_after_external_replace", func(t *testing.T) {
		root := t.TempDir()
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "created.txt")
		foreignReplacement := []byte("FOREIGN REPLACEMENT AFTER CREATE\n")
		e.afterTruncate = func(path string) error {
			if err := os.Remove(path); err != nil {
				return err
			}
			if err := os.WriteFile(path, foreignReplacement, 0o644); err != nil {
				return err
			}
			return errors.New("injected write failure after external replacement")
		}
		content := []byte("agent initial create\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Changes: []FileChange{{Path: "created.txt", Content: &content, AllowCreate: true}},
		})
		if commitErr == nil {
			t.Fatalf("expected injected write failure")
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("replaced foreign file was deleted during writeCAS cleanup: %v", readErr)
		}
		if string(got) != string(foreignReplacement) {
			t.Fatalf("replaced foreign file content = %q, want %q", got, foreignReplacement)
		}
	})

	t.Run("multi_file_rollback_after_external_recreate", func(t *testing.T) {
		root := t.TempDir()
		existingPath := filepath.Join(root, "b_existing.txt")
		if err := os.WriteFile(existingPath, []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		createdPath := filepath.Join(root, "a_created.txt")
		foreignRecreated := []byte("FOREIGN RECREATED FILE\n")
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b_existing.txt" {
				// First file a_created.txt has already been created and closed.
				// External process replaces a_created.txt AND mutates b_existing.txt to trigger rollback!
				_ = os.Remove(createdPath)
				_ = os.WriteFile(createdPath, foreignRecreated, 0o644)
				_ = os.WriteFile(existingPath, []byte("concurrent-b\n"), 0o644)
			}
		}
		aContent := []byte("agent-a\n")
		bContent := []byte("agent-b\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor: Actor{Session: "s1"},
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a_created.txt", Content: &aContent, Create: true},
				{Path: "b_existing.txt", Content: &bContent},
			},
		})
		if commitErr == nil {
			t.Fatalf("expected second file CAS conflict to trigger rollback")
		}
		got, readErr := os.ReadFile(createdPath)
		if readErr != nil {
			t.Fatalf("foreign recreated file deleted by rollback: %v", readErr)
		}
		if string(got) != string(foreignRecreated) {
			t.Fatalf("foreign recreated file content = %q, want %q", got, foreignRecreated)
		}
	})
}

// TestCreateCleanupRemovesOwnUnmodifiedCreatedFile verifies that when this Commit call genuinely
// created a file via O_CREATE|O_EXCL and no external writer replaced it, writeCAS failure or
// multi-file rollback cleanly removes the created file.
func TestCreateCleanupRemovesOwnUnmodifiedCreatedFile(t *testing.T) {
	t.Run("writeCAS_own_file_cleaned", func(t *testing.T) {
		root := t.TempDir()
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		e.afterTruncate = func(string) error {
			return errors.New("disk full")
		}
		content := []byte("agent create\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Changes: []FileChange{{Path: "own.txt", Content: &content, AllowCreate: true}},
		})
		if commitErr == nil {
			t.Fatalf("expected write failure")
		}
		if _, statErr := os.Lstat(filepath.Join(root, "own.txt")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expected own created file to be cleaned up, statErr=%v", statErr)
		}
	})

	t.Run("rollback_own_file_cleaned", func(t *testing.T) {
		root := t.TempDir()
		existingPath := filepath.Join(root, "b_existing.txt")
		if err := os.WriteFile(existingPath, []byte("old-b\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		e.beforeWrite = func(path string) {
			if filepath.Base(path) == "b_existing.txt" {
				_ = os.WriteFile(existingPath, []byte("concurrent-b\n"), 0o644)
			}
		}
		aContent := []byte("agent-a\n")
		bContent := []byte("agent-b\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor: Actor{Session: "s1"},
			Tool:  "apply_patch",
			Changes: []FileChange{
				{Path: "a_own.txt", Content: &aContent, Create: true},
				{Path: "b_existing.txt", Content: &bContent},
			},
		})
		if commitErr == nil {
			t.Fatalf("expected CAS conflict on second file")
		}
		if _, statErr := os.Lstat(filepath.Join(root, "a_own.txt")); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("expected own created file a_own.txt to be removed on rollback, statErr=%v", statErr)
		}
	})
}

// TestModifyCASConflictAndSymlinkSwapNeverDeleteFile verifies that CAS conflict and size-limit
// errors on existing files (create == false), as well as symlink replacement of a newly created
// file, never unlink the target path or its symlink target.
func TestModifyCASConflictAndSymlinkSwapNeverDeleteFile(t *testing.T) {
	t.Run("existing_file_cas_conflict_preserved", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "existing.txt")
		if err := os.WriteFile(target, []byte("v1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		externalV2 := []byte("EXTERNAL V2\n")
		e.beforeWrite = func(string) {
			_ = os.WriteFile(target, externalV2, 0o644)
		}
		next := []byte("agent v3\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Changes: []FileChange{{Path: "existing.txt", Content: &next}},
		})
		if CodeOf(commitErr) != CodeConflict {
			t.Fatalf("expected CodeConflict, got %v", commitErr)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != string(externalV2) {
			t.Fatalf("existing file after CAS conflict: err=%v content=%q", err, got)
		}
	})

	t.Run("created_file_swapped_for_symlink_preserved", func(t *testing.T) {
		root := t.TempDir()
		victim := filepath.Join(root, "victim.txt")
		if err := os.WriteFile(victim, []byte("precious victim\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		e, err := New(Options{Root: root})
		if err != nil {
			t.Fatal(err)
		}
		created := filepath.Join(root, "newlink.txt")
		e.afterTruncate = func(path string) error {
			_ = os.Remove(path)
			if err := os.Symlink(victim, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			return errors.New("injected write error after symlink swap")
		}
		next := []byte("agent data\n")
		_, commitErr := e.Commit(context.Background(), Plan{
			Actor:   Actor{Session: "s1"},
			Tool:    "write_file",
			Changes: []FileChange{{Path: "newlink.txt", Content: &next, AllowCreate: true}},
		})
		if commitErr == nil {
			t.Fatalf("expected injected write error")
		}
		info, lerr := os.Lstat(created)
		if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("expected replacement symlink at %s to remain untouched, lerr=%v mode=%v", created, lerr, info)
		}
		got, rerr := os.ReadFile(victim)
		if rerr != nil || string(got) != "precious victim\n" {
			t.Fatalf("victim file altered: err=%v got=%q", rerr, got)
		}
	})

	t.Run("file_identity_unit_semantics", func(t *testing.T) {
		dir := t.TempDir()
		p1 := filepath.Join(dir, "f1.txt")
		p2 := filepath.Join(dir, "f2.txt")
		f1, err := os.OpenFile(p1, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer f1.Close()
		id1, err := captureFileIdentity(f1)
		if err != nil {
			t.Fatal(err)
		}
		st1, err := os.Lstat(p1)
		if err != nil || !id1.matchesInfo(st1) {
			t.Fatalf("expected id1 to match its own Lstat (err=%v)", err)
		}
		if err := os.WriteFile(p2, []byte("other"), 0o644); err != nil {
			t.Fatal(err)
		}
		st2, err := os.Lstat(p2)
		if err != nil {
			t.Fatal(err)
		}
		if id1.matchesInfo(st2) {
			t.Fatalf("distinct file matched id1")
		}
	})
}
