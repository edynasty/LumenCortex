package editing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// platformExchangesExistingFiles: renameSwapPlatform returns ENOSYS on windows and on Unix other
// than darwin and linux (OPUS-V-01).
const platformExchangesExistingFiles = false

// assertExistingFileUpdateRefused requires a clean unsupported_filesystem refusal of the update of
// rel that left the original file (same file identity, same bytes) and no private staging entry.
func assertExistingFileUpdateRefused(t *testing.T, root, rel string, before os.FileInfo, want string, err error, res Result, rec CommitReceipt) {
	t.Helper()
	if CodeOf(err) != CodeUnsupportedFilesystem || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("update of %s: err=%v, want unsupported_filesystem with nothing written", rel, err)
	}
	if rec.Committed || rec.DirtyFailure || res.CommitState != CommitStatePrePublishFailure || rec.State != CommitStatePrePublishFailure {
		t.Fatalf("refused update state: res=%+v rec=%+v", res, rec)
	}
	f, r := r7Files(t, res, rec, rel)
	if f.PublishState != PublishUnsupported || r.PublishState != PublishUnsupported || f.Changed || f.Published || r.Published ||
		r.Wrote || f.Verified || r.Verified || f.RolledBack || r.RolledBack || f.NewRev != "" || r.NewSHA256 != "" || f.Error == "" {
		t.Fatalf("refused update claims an effect: %+v %+v", f, r)
	}
	path := filepath.Join(root, rel)
	after, statErr := os.Lstat(path)
	raw, readErr := os.ReadFile(path)
	if statErr != nil || readErr != nil || !os.SameFile(before, after) || string(raw) != want {
		t.Fatalf("%s not kept: same file=%v bytes=%q stat=%v read=%v", rel, statErr == nil && os.SameFile(before, after), raw, statErr, readErr)
	}
	entries, dirErr := os.ReadDir(root)
	if dirErr != nil {
		t.Fatal(dirErr)
	}
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name(), ".lcx-") {
			t.Fatalf("private staging entry left after a refused update: %s", ent.Name())
		}
	}
}

// TestOpusV01ExistingFileUpdateRefusedWithoutExchange uses the platform's real primitives. Without
// an atomic exchange an update of an existing file is refused before publication and the original
// stays; in a batch the first refused file stops the commit, so later files are not attempted.
func TestOpusV01ExistingFileUpdateRefusedWithoutExchange(t *testing.T) {
	t.Run("single_update", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		before, err := os.Lstat(filepath.Join(root, "a.txt"))
		if err != nil {
			t.Fatal(err)
		}
		e := newTestEngine(t, root)
		next := []byte("new-a\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "write_file", Changes: []FileChange{
			{Path: "a.txt", Content: &next},
		}})
		assertExistingFileUpdateRefused(t, root, "a.txt", before, "old-a\n", err, res, rec)
		if got := e.LatestRev(testActor(), filepath.Join(root, "a.txt")); got == RevOf(next) {
			t.Fatalf("refused content registered as a snapshot: %q", got)
		}
	})

	t.Run("batch_stops_at_first_update", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "old-a\n")
		writeFile(t, root, "b.txt", "old-b\n")
		before, err := os.Lstat(filepath.Join(root, "a.txt"))
		if err != nil {
			t.Fatal(err)
		}
		e := newTestEngine(t, root)
		na, nb := []byte("new-a\n"), []byte("new-b\n")
		var rec CommitReceipt
		ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
		res, err := e.Commit(ctx, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
			{Path: "a.txt", Content: &na},
			{Path: "b.txt", Content: &nb},
		}})
		assertExistingFileUpdateRefused(t, root, "a.txt", before, "old-a\n", err, res, rec)
		fb, rb := r7Files(t, res, rec, "b.txt")
		r7AssertNotAttempted(t, fb, rb)
		if got := readFileContent(t, root, "b.txt"); got != "old-b\n" {
			t.Fatalf("b.txt attempted after the refusal: %q", got)
		}
	})
}
