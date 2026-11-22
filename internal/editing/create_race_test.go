package editing

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCreateRaceForeignFilePreserved: an external writer creates the target between prepare and
// publication. No-replace publication refuses with CodeConflict and never unlinks or overwrites the
// foreign file.
func TestCreateRaceForeignFilePreserved(t *testing.T) {
	cases := []struct {
		name   string
		change func(*[]byte) FileChange
	}{
		{name: "create_flag", change: func(content *[]byte) FileChange {
			return FileChange{Path: "gen/out.txt", Content: content, Create: true}
		}},
		{name: "allow_create", change: func(content *[]byte) FileChange {
			return FileChange{Path: "gen/out.txt", Content: content, AllowCreate: true}
		}},
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
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { receipt = r })
			content := []byte("agent content\n")
			res, commitErr := e.Commit(ctx, Plan{
				Actor:   Actor{Session: "s1"},
				Tool:    "write_file",
				Changes: []FileChange{tc.change(&content)},
			})
			if commitErr == nil {
				t.Fatalf("expected commit refusal")
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
			if receipt.DirtyFailure || receipt.Committed {
				t.Fatalf("clean create refusal: %+v", receipt)
			}
			f, r := r7Files(t, res, receipt, "gen/out.txt")
			if f.PublishState != PublishExternalModification || f.Published || r.Wrote || f.Verified || r.NewSHA256 != "" {
				t.Fatalf("receipt claims effect: %+v %+v", f, r)
			}
			assertNoCreateTempLeftovers(t, root)
		})
	}
}

// TestCreateRaceOversizedForeignFilePreserved: a foreign file over MaxFileBytes must be preserved
// too; no-replace publication refuses before reading or replacing it.
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

// TestCreateStagingFailureNeverPublishes: a private staging write failure removes the temp and
// leaves the destination absent.
func TestCreateStagingFailureNeverPublishes(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(*os.File, []byte) (int, error)
	}{
		{name: "error", hook: func(*os.File, []byte) (int, error) { return 0, errors.New("disk full") }},
		{name: "short", hook: func(f *os.File, data []byte) (int, error) { return f.Write(data[:1]) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			e, err := New(Options{Root: root})
			if err != nil {
				t.Fatal(err)
			}
			e.SetPublishFaultHooks(PublishFaultHooks{WriteStaged: func(_ string, f *os.File, data []byte) (int, error) {
				return tc.hook(f, data)
			}})
			var rec CommitReceipt
			ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
			content := []byte("agent create\n")
			res, commitErr := e.Commit(ctx, Plan{Actor: Actor{Session: "s1"}, Tool: "write_file", Changes: []FileChange{
				{Path: "own.txt", Content: &content, Create: true},
			}})
			if commitErr == nil {
				t.Fatal("expected staging failure")
			}
			f, r := r7Files(t, res, rec, "own.txt")
			if f.PublishState != PublishFailedBeforeWrite || f.Published || r.Wrote || f.Verified || r.NewSHA256 != "" || r.Staged != true {
				t.Fatalf("staging failure: %+v %+v", f, r)
			}
			if _, statErr := os.Lstat(filepath.Join(root, "own.txt")); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("destination exists after failed staging: %v", statErr)
			}
			assertNoCreateTempLeftovers(t, root)
		})
	}
}

// TestCreatePrefixForeignReplacementSurvivesLaterConflict: R7 keeps a published create and never
// removes a foreign replacement of it when a later path fails.
func TestCreatePrefixForeignReplacementSurvivesLaterConflict(t *testing.T) {
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
			_ = os.Remove(createdPath)
			_ = os.WriteFile(createdPath, foreignRecreated, 0o644)
			_ = os.WriteFile(existingPath, []byte("concurrent-b\n"), 0o644)
		}
	}
	aContent := []byte("agent-a\n")
	bContent := []byte("agent-b\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, commitErr := e.Commit(ctx, Plan{
		Actor: Actor{Session: "s1"},
		Tool:  "apply_patch",
		Changes: []FileChange{
			{Path: "a_created.txt", Content: &aContent, Create: true},
			{Path: "b_existing.txt", Content: &bContent},
		},
	})
	if commitErr == nil {
		t.Fatalf("expected second file conflict")
	}
	if got, readErr := os.ReadFile(createdPath); readErr != nil || string(got) != string(foreignRecreated) {
		t.Fatalf("foreign recreated file lost: err=%v got=%q", readErr, got)
	}
	fa, ra := r7Files(t, res, rec, "a_created.txt")
	if fa.PublishState != PublishExternalModification || !fa.Published || !ra.Wrote || fa.Verified || fa.NewRev != "" || ra.NewSHA256 != "" || fa.RolledBack || ra.RolledBack {
		t.Fatalf("foreign replacement not reported exactly: %+v %+v", fa, ra)
	}
	if res.CommitState != CommitStatePartiallyPublished || !rec.DirtyFailure {
		t.Fatalf("state: %+v %+v", res, rec)
	}
}

// TestCreateNeverFollowsSymlinkSwap: a symlink placed at the target before publication is refused;
// neither the link nor its victim is removed or followed.
func TestCreateNeverFollowsSymlinkSwap(t *testing.T) {
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
	e.SetPublishFaultHooks(PublishFaultHooks{BeforePublish: func(dst string) error {
		if err := os.Symlink(victim, dst); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return nil
	}})
	next := []byte("agent data\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, commitErr := e.Commit(ctx, Plan{
		Actor:   Actor{Session: "s1"},
		Tool:    "write_file",
		Changes: []FileChange{{Path: "newlink.txt", Content: &next, Create: true}},
	})
	if commitErr == nil {
		t.Fatalf("expected publication refusal against a symlink")
	}
	info, lerr := os.Lstat(created)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement symlink removed: lerr=%v mode=%v", lerr, info)
	}
	got, rerr := os.ReadFile(victim)
	if rerr != nil || string(got) != "precious victim\n" {
		t.Fatalf("victim file altered: err=%v got=%q", rerr, got)
	}
	f, r := r7Files(t, res, rec, "newlink.txt")
	if f.Published || r.Wrote || f.Verified || r.NewSHA256 != "" {
		t.Fatalf("symlink contention claimed publication: %+v %+v", f, r)
	}
}

// TestModifyCASConflictPreservesExistingFile: an in-place update never rewrites or unlinks a file
// changed by an external writer.
func TestModifyCASConflictPreservesExistingFile(t *testing.T) {
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
	e.beforeWrite = func(string) { _ = os.WriteFile(target, externalV2, 0o644) }
	next := []byte("agent v3\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, commitErr := e.Commit(ctx, Plan{
		Actor:   Actor{Session: "s1"},
		Tool:    "write_file",
		Changes: []FileChange{{Path: "existing.txt", Content: &next}},
	})
	if CodeOf(commitErr) != CodeConflict {
		t.Fatalf("expected CodeConflict, got %v", commitErr)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, externalV2) {
		t.Fatalf("existing file after CAS conflict: err=%v content=%q", err, got)
	}
	f, r := r7Files(t, res, rec, "existing.txt")
	if f.PublishState != PublishExternalModification || f.Published || r.Wrote || f.Verified || r.NewSHA256 != "" {
		t.Fatalf("CAS conflict claims effect: %+v %+v", f, r)
	}
}

// TestFileIdentityUnitSemantics pins the identity primitive used for no-op and settlement checks.
func TestFileIdentityUnitSemantics(t *testing.T) {
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
}
