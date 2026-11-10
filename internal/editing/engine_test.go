package editing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Hashline scenarios of the roadmap's E3 prototype, ported to the engine: a fresh edit, a stale
// anchor, a relocation across an unrelated external change, a refusal when the edited range itself
// moved, an atomic multi-op batch, overlap rejection, byte-exact encoding preservation, the
// compare-and-swap race, and an edit without a read.

const hashBase = "package demo\n\nfunc A() int {\n\treturn 1\n}\n\nfunc B() int {\n\treturn 1\n}\n"

func testActor() Actor { return Actor{Session: "session-1", Run: "run-1"} }

// newTestEngine builds an engine over root with no event bus.
func newTestEngine(t *testing.T, root string) *Engine {
	t.Helper()
	engine, err := New(Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFileContent(t *testing.T, root, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// readSnapshot registers a full read of a file, the way read_file with anchors does.
func readSnapshot(t *testing.T, engine *Engine, name string) *Snapshot {
	t.Helper()
	path := filepath.Join(engine.Root(), name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := DecodeDoc(raw)
	return engine.Snapshots().Register(testActor().Session, canonicalPath(path), doc, 1, len(doc.Lines))
}

func resolveOps(t *testing.T, snap *Snapshot, ops ...AnchorOp) []LineOp {
	t.Helper()
	lineOps, err := ParseOps(snap, ops)
	if err != nil {
		t.Fatalf("ParseOps: %v", err)
	}
	return lineOps
}

func anchorAt(doc Doc, line int) string { return FormatAnchor(doc, line) }

func commitLineOps(t *testing.T, engine *Engine, name string, rev string, ops []LineOp) (Result, error) {
	t.Helper()
	return engine.Commit(context.Background(), Plan{
		Actor:   testActor(),
		Tool:    "edit_lines",
		Changes: []FileChange{{Path: name, BaseRev: rev, LineOps: ops}},
	})
}

// S1: a fresh edit on the snapshot just read changes only the addressed line.
func TestHashlineFreshEditOnSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 8), Text: "\treturn 2"})
	result, err := commitLineOps(t, engine, "demo.go", snap.Rev, ops)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := readFileContent(t, root, "demo.go")
	if strings.Count(got, "return 1") != 1 || !strings.Contains(got, "func B() int {\n\treturn 2") {
		t.Fatalf("only B must change:\n%s", got)
	}
	if len(result.Files) != 1 || result.Files[0].NewRev == snap.Rev || result.Files[0].OldRev != snap.Rev {
		t.Fatalf("result = %+v", result)
	}
	// The new revision is what a later edit can anchor against.
	if _, ok := engine.Snapshots().Lookup(testActor().Session, canonicalPath(filepath.Join(root, "demo.go")), result.Files[0].NewRev); !ok {
		t.Fatal("the commit did not register its new revision")
	}
}

// S2: an anchored line changed externally is refused, and the file is left as the other writer made it.
func TestHashlineRefusesChangedAnchoredLine(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	writeFile(t, root, "demo.go", strings.Replace(hashBase, "func B() int {\n\treturn 1", "func B() int {\n\treturn 7", 1))
	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 8), Text: "\treturn 2"})
	_, err := commitLineOps(t, engine, "demo.go", snap.Rev, ops)
	editErr, ok := AsError(err)
	if !ok || editErr.Code != CodeStale {
		t.Fatalf("err = %v, want stale", err)
	}
	if got := readFileContent(t, root, "demo.go"); !strings.Contains(got, "return 7") {
		t.Fatalf("the other writer's change was lost: %s", got)
	}
}

// S3: an unrelated insertion above is relocated; the duplicate "\treturn 1" lines do not confuse it.
func TestHashlineRelocatesAcrossUnrelatedInsertion(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	moved := strings.Replace(hashBase, "package demo\n", "package demo\n\nimport \"fmt\"\n\nvar _ = fmt.Sprint\n", 1)
	writeFile(t, root, "demo.go", moved)
	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 8), Text: "\treturn 2"})
	result, err := commitLineOps(t, engine, "demo.go", snap.Rev, ops)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := readFileContent(t, root, "demo.go")
	if !strings.Contains(got, "func A() int {\n\treturn 1") || !strings.Contains(got, "func B() int {\n\treturn 2") {
		t.Fatalf("the edit must land on B after the insertion:\n%s", got)
	}
	if len(result.Files) != 1 || result.Files[0].OldRev != RevOf([]byte(moved)) {
		t.Fatalf("result = %+v", result)
	}
}

// S4: a line inserted inside the edited range refuses the relocation, because applying it would
// silently drop the new line.
func TestHashlineRefusesInsertionInsideEditedRange(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	writeFile(t, root, "demo.go", strings.Replace(hashBase, "func A() int {\n\treturn 1\n}", "func A() int {\n\tlog()\n\treturn 1\n}", 1))
	ops := resolveOps(t, snap, AnchorOp{
		Op: OpReplace, Start: anchorAt(snap.Doc, 3), End: anchorAt(snap.Doc, 5), Text: "func A() int { return 3 }",
	})
	_, err := commitLineOps(t, engine, "demo.go", snap.Rev, ops)
	editErr, ok := AsError(err)
	if !ok || editErr.Code != CodeStale {
		t.Fatalf("err = %v, want stale", err)
	}
	if got := readFileContent(t, root, "demo.go"); !strings.Contains(got, "log()") {
		t.Fatalf("the inserted line was dropped: %s", got)
	}
}

// S5: several ops on one snapshot apply atomically, each in the original numbering.
func TestHashlineAppliesBatchAgainstOneSnapshot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	ops := resolveOps(t, snap,
		AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 4), Text: "\treturn 10"},
		AnchorOp{Op: OpInsertAfter, Start: anchorAt(snap.Doc, 5), Text: "\n// between"},
		AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 8), Text: "\treturn 20"},
	)
	if _, err := commitLineOps(t, engine, "demo.go", snap.Rev, ops); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := readFileContent(t, root, "demo.go")
	want := "package demo\n\nfunc A() int {\n\treturn 10\n}\n\n// between\n\nfunc B() int {\n\treturn 20\n}\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

// S5b: overlapping ops are refused before anything is written.
func TestHashlineRefusesOverlappingOps(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	_, err := ParseOps(snap, []AnchorOp{
		{Op: OpReplace, Start: anchorAt(snap.Doc, 3), End: anchorAt(snap.Doc, 5), Text: "x"},
		{Op: OpDelete, Start: anchorAt(snap.Doc, 4)},
	})
	editErr, ok := AsError(err)
	if !ok || editErr.Code != CodeOverlap {
		t.Fatalf("err = %v, want overlap", err)
	}
	if got := readFileContent(t, root, "demo.go"); got != hashBase {
		t.Fatalf("a refused batch changed the file: %q", got)
	}
}

// S6: BOM, CRLF and a missing final newline are preserved byte for byte.
func TestHashlinePreservesEncoding(t *testing.T) {
	root := t.TempDir()
	const original = "\xef\xbb\xbfline one\r\nline two\r\nline three"
	writeFile(t, root, "notes.txt", original)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "notes.txt")

	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 2), Text: "line 2"})
	if _, err := commitLineOps(t, engine, "notes.txt", snap.Rev, ops); err != nil {
		t.Fatalf("commit: %v", err)
	}
	const want = "\xef\xbb\xbfline one\r\nline 2\r\nline three"
	if got := readFileContent(t, root, "notes.txt"); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// S7: a writer that lands between validation and the write is caught by the compare-and-swap, and
// its change is preserved.
func TestHashlineRefusesConcurrentWriteWithCompareAndSwap(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")
	engine.beforeWrite = func(path string) {
		if err := os.WriteFile(path, []byte(hashBase+"// concurrent\n"), 0o644); err != nil {
			t.Errorf("concurrent write: %v", err)
		}
	}

	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 8), Text: "\treturn 2"})
	_, err := commitLineOps(t, engine, "demo.go", snap.Rev, ops)
	editErr, ok := AsError(err)
	if !ok || editErr.Code != CodeConflict {
		t.Fatalf("err = %v, want conflict", err)
	}
	got := readFileContent(t, root, "demo.go")
	if !strings.Contains(got, "// concurrent") || !strings.Contains(got, "\treturn 1\n}\n// concurrent") {
		t.Fatalf("the concurrent change was lost:\n%s", got)
	}
}

// S7b: a whole-file write computed from a stale read is refused by its base revision instead of
// silently dropping the other writer's change.
func TestWholeFileWriteRefusesStaleBaseRevision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	writeFile(t, root, "demo.go", hashBase+"// concurrent\n")
	stale := []byte(strings.Replace(hashBase, "return 1\n}\n\nfunc B", "return 9\n}\n\nfunc B", 1))
	_, err := engine.Commit(context.Background(), Plan{
		Actor:   testActor(),
		Tool:    "write_file",
		Changes: []FileChange{{Path: "demo.go", BaseRev: snap.Rev, Content: &stale}},
	})
	editErr, ok := AsError(err)
	if !ok || editErr.Code != CodeStale {
		t.Fatalf("err = %v, want stale", err)
	}
	if got := readFileContent(t, root, "demo.go"); !strings.Contains(got, "// concurrent") {
		t.Fatalf("the concurrent change was lost:\n%s", got)
	}
}

// S8: an edit whose revision this session never read is refused.
func TestHashlineRefusesUnknownRevision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)

	_, err := commitLineOps(t, engine, "demo.go", "aaaaaaaa", []LineOp{{Kind: OpDelete, Start: 1, End: 1}})
	editErr, ok := AsError(err)
	if !ok || editErr.Code != CodeUnknownRev {
		t.Fatalf("err = %v, want unknown_rev", err)
	}
	if got := readFileContent(t, root, "demo.go"); got != hashBase {
		t.Fatalf("a refused edit changed the file: %q", got)
	}
}

// An anchor whose hash is not the snapshot's line is refused, and so is a line never shown.
func TestHashlineRefusesWrongOrUnseenAnchor(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "demo.go", hashBase)
	engine := newTestEngine(t, root)
	snap := readSnapshot(t, engine, "demo.go")

	_, err := ParseOps(snap, []AnchorOp{{Op: OpDelete, Start: "8:zzz"}})
	if code := CodeOf(err); code != CodeAnchorMismatch {
		t.Fatalf("err = %v, want anchor_mismatch", err)
	}

	// A partial read: only line 1 was shown. (Seen lines accumulate for a revision, so a fresh store
	// models a session that only ever read the first line.)
	engine.Snapshots().Reset()
	partial := engine.Snapshots().Register(testActor().Session, canonicalPath(filepath.Join(root, "demo.go")), snap.Doc, 1, 1)
	_, err = ParseOps(partial, []AnchorOp{{Op: OpDelete, Start: anchorAt(snap.Doc, 8)}})
	if code := CodeOf(err); code != CodeNotSeen {
		t.Fatalf("err = %v, want not_seen", err)
	}
}

// UTF-8 only: a file the engine cannot decode is refused without being written.
func TestCommitRefusesNonUTF8File(t *testing.T) {
	root := t.TempDir()
	raw := []byte("package demo\n\xff\xfe\n")
	if err := os.WriteFile(filepath.Join(root, "bin.go"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	engine := newTestEngine(t, root)
	splices := []Splice{{Start: 0, End: 0, Text: "// x\n"}}
	_, err := engine.Commit(context.Background(), Plan{Actor: testActor(), Tool: "edit_lines",
		Changes: []FileChange{{Path: "bin.go", Splices: splices}}})
	if code := CodeOf(err); code != CodeNotUTF8 {
		t.Fatalf("err = %v, want not_utf8", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "bin.go")); string(got) != string(raw) {
		t.Fatal("a refused write changed the file")
	}
}
