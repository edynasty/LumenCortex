package editing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// r7r2CommitRecorded runs one commit with a receipt recorder and counts the paths that reached the
// write stage.
func r7r2CommitRecorded(t *testing.T, e *Engine, plan Plan) (Result, CommitReceipt, error, []string) {
	t.Helper()
	var writes []string
	e.beforeWrite = func(path string) { writes = append(writes, filepath.Base(path)) }
	defer func() { e.beforeWrite = nil }()
	var rec CommitReceipt
	ctx := WithCommitRecorder(context.Background(), func(r CommitReceipt) { rec = r })
	res, err := e.Commit(ctx, plan)
	return res, rec, err, writes
}

// r7r2AssertRefusedUntouched checks a refusal that must happen before any write: the error code, no
// write attempt, every receipt row clean, and a clean receipt state.
func r7r2AssertRefusedUntouched(t *testing.T, res Result, rec CommitReceipt, err error, writes []string, code string) {
	t.Helper()
	if CodeOf(err) != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	if len(writes) != 0 {
		t.Fatalf("refusal reached the write stage for %v", writes)
	}
	if rec.State != CommitStatePrePublishFailure || rec.DirtyFailure || rec.Committed || res.CommitState != CommitStatePrePublishFailure {
		t.Fatalf("refusal is not a clean pre-publication failure: result=%s receipt=%+v", res.CommitState, rec)
	}
	for _, f := range rec.Files {
		if f.Published || f.Wrote || f.Verified || f.NewSHA256 != "" {
			t.Fatalf("refused path claims an effect: %+v", f)
		}
		switch f.PublishState {
		case PublishFailedBeforeWrite, PublishNotAttempted:
		default:
			t.Fatalf("refused path %s reported as %s", f.Path, f.PublishState)
		}
	}
}

// caseFolds reports whether the test directory's filesystem folds letter case. It is an oracle
// independent of the engine's probe: it creates a file and looks it up by its other spelling.
func caseFolds(t *testing.T, root string) bool {
	t.Helper()
	writeFile(t, root, "probe-case.txt", "x\n")
	defer os.Remove(filepath.Join(root, "probe-case.txt"))
	orig, err := os.Lstat(filepath.Join(root, "probe-case.txt"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.Lstat(filepath.Join(root, "PROBE-CASE.TXT"))
	return err == nil && os.SameFile(orig, other)
}

// TestR7R2HardLinkAliasRefusedBeforeWrite: two hard-linked names are one physical file. Before the
// fix the plan published the first spelling and then failed on the second (R7R2-PartialCommit-01).
func TestR7R2HardLinkAliasRefusedBeforeWrite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "A0\n")
	if err := os.Link(filepath.Join(root, "a.txt"), filepath.Join(root, "hl.txt")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	e := newTestEngine(t, root)
	rev := RevOf([]byte("A0\n"))
	res, rec, err, writes := r7r2CommitRecorded(t, e, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "hl.txt", BaseRev: rev, Edits: []TextEdit{{Old: "A0\n", New: "A0\nFIRST\n"}}},
		{Path: "a.txt", BaseRev: rev, Edits: []TextEdit{{Old: "A0\n", New: "A0\nSECOND\n"}}},
	}})
	r7r2AssertRefusedUntouched(t, res, rec, err, writes, CodeInvalid)
	if got := readFileContent(t, root, "a.txt"); got != "A0\n" {
		t.Fatalf("aliased file changed: %q", got)
	}
}

// TestR7R2CaseVariantOfExistingFileRefused: on a case-folding volume "A.txt" and "a.txt" are one
// file; on a case-sensitive volume they are two, and the plan must not be refused for the spelling.
func TestR7R2CaseVariantOfExistingFileRefused(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "A0\n")
	folds := caseFolds(t, root)
	if !folds {
		writeFile(t, root, "A.txt", "A0\n")
	}
	e := newTestEngine(t, root)
	rev := RevOf([]byte("A0\n"))
	res, rec, err, writes := r7r2CommitRecorded(t, e, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "A.txt", BaseRev: rev, Edits: []TextEdit{{Old: "A0\n", New: "A0\nFIRST\n"}}},
		{Path: "a.txt", BaseRev: rev, Edits: []TextEdit{{Old: "A0\n", New: "A0\nSECOND\n"}}},
	}})
	if folds {
		r7r2AssertRefusedUntouched(t, res, rec, err, writes, CodeInvalid)
		if got := readFileContent(t, root, "a.txt"); got != "A0\n" {
			t.Fatalf("case-aliased file changed: %q", got)
		}
		return
	}
	if err != nil || readFileContent(t, root, "A.txt") != "A0\nFIRST\n" || readFileContent(t, root, "a.txt") != "A0\nSECOND\n" {
		t.Fatalf("distinct case-sensitive files were not both committed: %v", err)
	}
}

// TestR7R2CaseVariantCreatesRefused: two new names that differ only in case collide on a
// case-folding volume. Before the fix the first create was published and the second failed.
func TestR7R2CaseVariantCreatesRefused(t *testing.T) {
	for _, tc := range []struct{ name, upper, lower string }{
		{"leaf", "N.txt", "n.txt"},
		{"existing_dir_spelling", "DIR/new.txt", "dir/new.txt"},
		{"new_dir", "Pkg/new.txt", "pkg/new.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
				t.Fatal(err)
			}
			folds := caseFolds(t, root)
			if tc.name == "existing_dir_spelling" && !folds {
				t.Skip("DIR is a distinct missing directory on a case-sensitive volume")
			}
			e := newTestEngine(t, root)
			up, low := []byte("UPPER\n"), []byte("lower\n")
			res, rec, err, writes := r7r2CommitRecorded(t, e, Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
				{Path: tc.upper, Content: &up, Create: true},
				{Path: tc.lower, Content: &low, Create: true},
			}})
			if folds {
				r7r2AssertRefusedUntouched(t, res, rec, err, writes, CodeInvalid)
				if _, statErr := os.Stat(filepath.Join(root, tc.lower)); !os.IsNotExist(statErr) {
					t.Fatalf("refused create left a file: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("case-sensitive volume refused distinct creates: %v", err)
			}
		})
	}
}

// TestR7R2StaleBaseIsFailedBeforeWrite: a stale base revision is the caller's read, found while
// preparing. It is not an external modification (R7R2-PartialCommit-02); the code stays stale and
// the session's view of the path is still invalidated.
func TestR7R2StaleBaseIsFailedBeforeWrite(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.txt", "b.txt", "c.txt"} {
		writeFile(t, root, name, "old-"+name+"\n")
	}
	e := newTestEngine(t, root)
	actor := testActor()
	e.RegisterRead(actor, filepath.Join(root, "c.txt"), RevOf([]byte("old-c.txt\n")))
	na, nb, nc := []byte("new-a\n"), []byte("new-b\n"), []byte("new-c\n")
	res, rec, err, writes := r7r2CommitRecorded(t, e, Plan{Actor: actor, Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", Content: &na},
		{Path: "b.txt", Content: &nb},
		{Path: "c.txt", Content: &nc, BaseRev: "zzzzzzzz"},
	}})
	r7r2AssertRefusedUntouched(t, res, rec, err, writes, CodeStale)
	fc, rc := r7Files(t, res, rec, "c.txt")
	if fc.PublishState != PublishFailedBeforeWrite || rc.PublishState != PublishFailedBeforeWrite || rc.Error == "" {
		t.Fatalf("stale base reported as %s/%s: %+v", fc.PublishState, rc.PublishState, rc)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "c.txt")); got != "" {
		t.Fatalf("stale view of c.txt kept: %q", got)
	}
}

// TestR7R2SelfReplayIsFailedBeforeWrite: replaying a plan after LC itself published A meets A's own
// new revision. That is not an external change.
func TestR7R2SelfReplayIsFailedBeforeWrite(t *testing.T) {
	requireExistingFileUpdate(t)
	root := t.TempDir()
	writeFile(t, root, "a.txt", "A0\n")
	e := newTestEngine(t, root)
	plan := Plan{Actor: testActor(), Tool: "apply_patch", Changes: []FileChange{
		{Path: "a.txt", BaseRev: RevOf([]byte("A0\n")), Edits: []TextEdit{{Old: "A0\n", New: "A1\n"}}},
	}}
	if _, _, err, _ := r7r2CommitRecorded(t, e, plan); err != nil {
		t.Fatal(err)
	}
	res, rec, err, writes := r7r2CommitRecorded(t, e, plan)
	r7r2AssertRefusedUntouched(t, res, rec, err, writes, CodeStale)
	if _, rc := r7Files(t, res, rec, "a.txt"); rc.PublishState != PublishFailedBeforeWrite {
		t.Fatalf("self replay reported as %s", rc.PublishState)
	}
	if got := readFileContent(t, root, "a.txt"); got != "A1\n" {
		t.Fatalf("replay changed a.txt: %q", got)
	}
}

// TestR7R2SessionViewConflictIsFailedBeforeWrite: a whole-file write refused because this session's
// last read is out of date is refused while preparing, before LC opens the file for writing.
func TestR7R2SessionViewConflictIsFailedBeforeWrite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "A0\n")
	e := newTestEngine(t, root)
	actor := testActor()
	e.RegisterRead(actor, filepath.Join(root, "a.txt"), RevOf([]byte("A0\n")))
	writeFile(t, root, "a.txt", "A-external\n")
	body := []byte("A-whole\n")
	res, rec, err, writes := r7r2CommitRecorded(t, e, Plan{Actor: actor, Tool: "write_file", Changes: []FileChange{
		{Path: "a.txt", Content: &body, AllowCreate: true, RejectStaleRead: true},
	}})
	r7r2AssertRefusedUntouched(t, res, rec, err, writes, CodeConflict)
	if _, rc := r7Files(t, res, rec, "a.txt"); rc.PublishState != PublishFailedBeforeWrite {
		t.Fatalf("prepare-time conflict reported as %s", rc.PublishState)
	}
	if got := e.LatestRev(actor, filepath.Join(root, "a.txt")); got != "" {
		t.Fatalf("out-of-date view of a.txt kept: %q", got)
	}
	if got := readFileContent(t, root, "a.txt"); got != "A-external\n" {
		t.Fatalf("external bytes changed: %q", got)
	}
}
