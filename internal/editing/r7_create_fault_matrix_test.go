package editing

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestR7CrashBeforePublicationHelper stages private bytes and exits without publishing. A later
// process must see no destination and may safely reuse the path with a new unique staging name.
func TestR7CrashBeforePublicationHelper(t *testing.T) {
	target := os.Getenv("LC_R7_CRASH_TARGET")
	if target == "" {
		return
	}
	e := newTestEngine(t, filepath.Dir(target))
	e.SetPublishFaultHooks(PublishFaultHooks{WriteStaged: func(_ string, f *os.File, data []byte) (int, error) {
		if _, err := f.Write(data[:len(data)/2]); err != nil {
			t.Fatal(err)
		}
		os.Exit(0) // Simulate death inside the real commit, before publication and settlement.
		return 0, nil
	}})
	_, err := e.Commit(context.Background(), Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{{
		Path: filepath.Base(target), Content: bytesPtr("staged before interruption\n"), Create: true,
	}}})
	t.Fatalf("staging interruption not reached: %v", err)
}

func r7RunCrashBeforePublication(t *testing.T, target string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestR7CrashBeforePublicationHelper$")
	cmd.Env = append(os.Environ(), "LC_R7_CRASH_TARGET="+target)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("crash helper: %v\n%s", err, out)
	}
}

func r7CountStagingLeftovers(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".lcx-create-") {
			n++
		}
	}
	return n
}

// r7ExplicitRecreate seeds a deleted path, then applies a fresh Create plan. Each case injects one
// fault into private staging or namespace publication. The destination must be absent or hold
// exactly the foreign bytes; it must never contain a prefix of the intended content.
func r7ExplicitRecreate(t *testing.T, hooks PublishFaultHooks, before func(root string)) (string, Result, CommitReceipt, error) {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "d.txt", "old-d\n")
	e := newTestEngine(t, root)
	actor := testActor()
	if _, err := e.Commit(context.Background(), Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "d.txt", Delete: true},
	}}); err != nil {
		t.Fatalf("seed delete: %v", err)
	}
	if before != nil {
		before(root)
	}
	e.SetPublishFaultHooks(hooks)
	content := []byte("recreated-content-that-must-not-be-partially-visible\n")
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
		{Path: "d.txt", Content: &content, Create: true},
	}})
	return root, res, rec, err
}

func r7AssertNoPartialDestination(t *testing.T, root, rel string, allowed ...[]byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	for _, want := range allowed {
		if string(raw) == string(want) {
			return
		}
	}
	t.Fatalf("destination %s exposed unexpected bytes %q", rel, raw)
}

func r7AssertCreateCleanFailure(t *testing.T, res Result, rec CommitReceipt, err error, want PublishState, staged, dirty bool) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	r7AssertSettledCount(t, res, rec, 1)
	f, r := r7Files(t, res, rec, "d.txt")
	if f.PublishState != want || r.PublishState != want {
		t.Fatalf("state = %s/%s, want %s", f.PublishState, r.PublishState, want)
	}
	if f.Published || r.Published || r.Wrote || f.Verified || r.Verified || f.NewRev != "" || r.NewSHA256 != "" {
		t.Fatalf("failed create claims an effect: %+v %+v", f, r)
	}
	if r.Staged != staged {
		t.Fatalf("Staged = %v, want %v", r.Staged, staged)
	}
	if rec.DirtyFailure != dirty || rec.Committed {
		t.Fatalf("receipt: %+v", rec)
	}
	if !dirty && rec.State != CommitStatePrePublishFailure {
		t.Fatalf("clean failure state = %s, want pre-publish failure", rec.State)
	}
}

// TestR7CreateRecreationFaultMatrix covers the portable half of the explicit-recreation matrix.
func TestR7CreateRecreationFaultMatrix(t *testing.T) {
	t.Run("staged_zero_write", func(t *testing.T) {
		root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{
			WriteStaged: func(string, *os.File, []byte) (int, error) { return 0, nil },
		}, nil)
		r7AssertCreateCleanFailure(t, res, rec, err, PublishFailedBeforeWrite, true, false)
		r7AssertNoPartialDestination(t, root, "d.txt")
		if n := r7CountStagingLeftovers(t, root); n != 0 {
			t.Fatalf("%d staging leftovers", n)
		}
	})

	t.Run("staged_partial_write", func(t *testing.T) {
		root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{
			WriteStaged: func(_ string, f *os.File, data []byte) (int, error) {
				return f.Write(data[:len(data)/2])
			},
		}, nil)
		r7AssertCreateCleanFailure(t, res, rec, err, PublishFailedBeforeWrite, true, false)
		r7AssertNoPartialDestination(t, root, "d.txt")
		if n := r7CountStagingLeftovers(t, root); n != 0 {
			t.Fatalf("%d staging leftovers", n)
		}
	})

	t.Run("staged_generic_write_error", func(t *testing.T) {
		root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{
			WriteStaged: func(string, *os.File, []byte) (int, error) {
				return 0, errors.New("injected staging failure")
			},
		}, nil)
		r7AssertCreateCleanFailure(t, res, rec, err, PublishFailedBeforeWrite, true, false)
		r7AssertNoPartialDestination(t, root, "d.txt")
		if n := r7CountStagingLeftovers(t, root); n != 0 {
			t.Fatalf("%d staging leftovers", n)
		}
	})

	t.Run("path_created_concurrently_at_publication", func(t *testing.T) {
		foreign := []byte("FOREIGN DESTINATION\n")
		root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{
			BeforePublish: func(dst string) error { return os.WriteFile(dst, foreign, 0o600) },
		}, nil)
		if err == nil || CodeOf(err) != CodeConflict {
			t.Fatalf("want conflict, got %v", err)
		}
		r7AssertCreateCleanFailure(t, res, rec, err, PublishExternalModification, true, false)
		r7AssertNoPartialDestination(t, root, "d.txt", foreign)
		if n := r7CountStagingLeftovers(t, root); n != 0 {
			t.Fatalf("%d staging leftovers", n)
		}
	})

	t.Run("child_exits_before_publication", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "d.txt", "old-d\n")
		e := newTestEngine(t, root)
		actor := testActor()
		if _, err := e.Commit(context.Background(), Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
			{Path: "d.txt", Delete: true},
		}}); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(root, "d.txt")
		r7RunCrashBeforePublication(t, target)
		if raw, err := os.ReadFile(target); err == nil {
			t.Fatalf("crash helper exposed destination bytes %q", raw)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("destination check: %v", err)
		}
		if n := r7CountStagingLeftovers(t, root); n == 0 {
			t.Fatal("expected private staging debris from abrupt death")
		}
		content := []byte("fresh-create-after-crash\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
			{Path: "d.txt", Content: &content, Create: true},
		}})
		if err != nil {
			t.Fatalf("fresh create after crash debris: %v", err)
		}
		f, r := r7Files(t, res, rec, "d.txt")
		if f.PublishState != PublishSucceededAndVerified || !f.Created || !f.Verified || r.NewSHA256 != sha256Ref(content, true) {
			t.Fatalf("fresh create: %+v %+v", f, r)
		}
		if got := readFileContent(t, root, "d.txt"); got != string(content) {
			t.Fatalf("recreated bytes: %q", got)
		}
	})
}

// TestR7ExplicitRecreateSuccessBaseline proves a committed delete leaves no revision fence that
// blocks a new no-replace create at the same path.
func TestR7ExplicitRecreateSuccessBaseline(t *testing.T) {
	root, res, rec, err := r7ExplicitRecreate(t, PublishFaultHooks{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r7AssertSettledCount(t, res, rec, 1)
	f, r := r7Files(t, res, rec, "d.txt")
	if f.PublishState != PublishSucceededAndVerified || !f.Created || !f.Verified || !r.Published || !r.Wrote {
		t.Fatalf("recreate: %+v %+v", f, r)
	}
	if got := readFileContent(t, root, "d.txt"); got != "recreated-content-that-must-not-be-partially-visible\n" {
		t.Fatalf("bytes: %q", got)
	}
}
