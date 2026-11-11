package editing

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// xrevActor is a second session in the same workspace, so two commits share the engine's stripes,
// leases and CAS while keeping separate snapshot stores.
func xrevActor(session string) Actor { return Actor{Session: session, Run: session + "-run"} }

// xrevRegister reads name the way read_file anchors=true does for one session.
func xrevRegister(t *testing.T, engine *Engine, session, name string) *Snapshot {
	t.Helper()
	path := filepath.Join(engine.Root(), name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc := DecodeDoc(raw)
	return engine.Snapshots().Register(session, CanonicalPath(path), doc, 1, len(doc.Lines))
}

func xrevLines(total int) string {
	var b strings.Builder
	for line := 1; line <= total; line++ {
		b.WriteString("line " + strconv.Itoa(line) + "\n")
	}
	return b.String()
}

// XE-01: a session may issue parallel reads of the same file. S1 also reads the returned snapshot
// without the store lock. The published snapshot must be immutable, or the race detector sees the
// second Register rewrite Doc/Seen under S1's feet (and a concurrent map read/write can panic).
func TestXReviewSnapshotRegistrationRaceFree(t *testing.T) {
	store := NewSnapshotStore()
	doc := DecodeDoc([]byte("a\nb\nc\nd\n"))
	big := DecodeDoc([]byte(strings.Repeat("x", 1024)))
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			session := "s"
			if worker%2 == 1 {
				session = "other"
			}
			for range 300 {
				snap := store.Register(session, "/w/a", doc, 1, 3)
				_ = len(snap.Doc.Lines)
				_ = snap.SeenLine(2)
				if looked, ok := store.Lookup(session, "/w/a", doc.Rev); ok {
					_ = len(looked.Doc.Lines)
					_ = looked.SeenLine(1)
				}
				store.RegisterRev(session, "/w/b", "r1")
				if latest, ok := store.Latest(session, "/w/b"); ok {
					_ = latest.Rev
				}
				store.Register(session, "/w/c", big, 1, 1)
				store.ForgetPath(session, "/w/c")
			}
		}(worker)
	}
	wg.Wait()
}

// Immutability holds across the copy-on-register change: accumulation of shown lines is preserved,
// a previously returned snapshot is not rewritten, and Lookup returns the newest registration.
func TestXReviewSnapshotRegisterCopiesInsteadOfMutating(t *testing.T) {
	store := NewSnapshotStore()
	doc := DecodeDoc([]byte("1\n2\n3\n4\n"))
	first := store.Register("s", "/w/a", doc, 1, 2)
	second := store.Register("s", "/w/a", doc, 3, 4)
	if !first.SeenLine(1) || !first.SeenLine(2) || first.SeenLine(4) {
		t.Fatalf("previously returned snapshot was mutated: %v", first.Seen)
	}
	for line := 1; line <= 4; line++ {
		if !second.SeenLine(line) {
			t.Fatalf("accumulated seen set lost line %d: %v", line, second.Seen)
		}
	}
	if looked, ok := store.Lookup("s", "/w/a", doc.Rev); !ok || looked != second {
		t.Fatal("Lookup did not return the newest registration")
	}
}

// Combination 1: two sessions edit one file concurrently with different tools (edit_lines vs
// write_file) against the same revision. Exactly one lands; the other is refused as stale/conflict;
// the final bytes are one writer's, never a mix.
func TestXReviewConcurrentLineEditAndWholeFileWrite(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "f.txt", xrevLines(20))
	engine := newTestEngine(t, root)

	lineActor := xrevActor("agent-line")
	writeActor := xrevActor("agent-write")
	lineSnap := xrevRegister(t, engine, lineActor.Session, "f.txt")
	writeSnap := xrevRegister(t, engine, writeActor.Session, "f.txt")
	ops := resolveOps(t, lineSnap, AnchorOp{Op: OpReplace, Start: anchorAt(lineSnap.Doc, 5), Text: "LINE-EDIT"})

	whole := []byte(strings.Replace(xrevLines(20), "line 5\n", "WHOLE-WRITE\n", 1))

	start := make(chan struct{})
	var wg sync.WaitGroup
	var lineErr, writeErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, lineErr = engine.Commit(context.Background(), Plan{Actor: lineActor, Tool: "edit_lines",
			Changes: []FileChange{{Path: "f.txt", BaseRev: lineSnap.Rev, LineOps: ops}}})
	}()
	go func() {
		defer wg.Done()
		<-start
		_, writeErr = engine.Commit(context.Background(), Plan{Actor: writeActor, Tool: "write_file",
			Changes: []FileChange{{Path: "f.txt", BaseRev: writeSnap.Rev, Content: &whole, RejectStaleRead: true}}})
	}()
	close(start)
	wg.Wait()

	if (lineErr == nil) == (writeErr == nil) {
		t.Fatalf("exactly one writer must win: line=%v write=%v", lineErr, writeErr)
	}
	var refusal string
	if lineErr != nil {
		refusal = CodeOf(lineErr)
	} else {
		refusal = CodeOf(writeErr)
	}
	if refusal != CodeStale && refusal != CodeConflict && refusal != CodeConcurrentWrite {
		t.Fatalf("loser failed with %q, want stale/conflict", refusal)
	}
	got := readFileContent(t, root, "f.txt")
	want := "LINE-EDIT"
	if writeErr == nil {
		want = "WHOLE-WRITE"
	}
	if !strings.Contains(got, want) {
		t.Fatalf("winner's edit is missing:\n%s", got)
	}
	if strings.Contains(got, "LINE-EDIT") && strings.Contains(got, "WHOLE-WRITE") {
		t.Fatal("final file mixes both writers")
	}
	if lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n"); len(lines) != 20 {
		t.Fatalf("line count changed to %d, want 20", len(lines))
	}
}

// Combination 4: delete with a revision precondition racing a whole-file write. The newer content is
// never deleted; whichever order, the surviving bytes belong to the winner and the loser is refused.
func TestXReviewDeleteCASAgainstConcurrentWrite(t *testing.T) {
	for round := range 40 {
		root := t.TempDir()
		writeFile(t, root, "a.txt", "original\n")
		engine := newTestEngine(t, root)
		delActor := xrevActor("deleter")
		writeActor := xrevActor("writer")

		next := []byte("new content\n")
		start := make(chan struct{})
		var wg sync.WaitGroup
		var delErr, writeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, delErr = engine.Commit(context.Background(), Plan{Actor: delActor, Tool: "apply_patch",
				Changes: []FileChange{{Path: "a.txt", BaseRev: RevOf([]byte("original\n")), Delete: true}}})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, writeErr = engine.Commit(context.Background(), Plan{Actor: writeActor, Tool: "write_file",
				Changes: []FileChange{{Path: "a.txt", BaseRev: RevOf([]byte("original\n")), Content: &next}}})
		}()
		close(start)
		wg.Wait()

		raw, err := os.ReadFile(filepath.Join(root, "a.txt"))
		switch {
		case writeErr == nil:
			if err != nil || string(raw) != "new content\n" {
				t.Fatalf("round %d: writer won but content is %q (%v)", round, raw, err)
			}
		case delErr == nil:
			if !os.IsNotExist(err) {
				t.Fatalf("round %d: deleter won but file remains %q", round, raw)
			}
		default:
			t.Fatalf("round %d: both writers refused: delete=%v write=%v", round, delErr, writeErr)
		}
	}
}

// Combination 3: a multi-file atomic plan races a single-file write to one of its files. Either the
// whole plan lands or none of it does; the single write is never half-merged into the plan.
func TestXReviewMultiFileAtomicAgainstSingleWrite(t *testing.T) {
	for round := range 40 {
		root := t.TempDir()
		writeFile(t, root, "p1.txt", "p1 old\n")
		writeFile(t, root, "p2.txt", "p2 old\n")
		writeFile(t, root, "p3.txt", "p3 old\n")
		engine := newTestEngine(t, root)
		batch := xrevActor("batch")
		single := xrevActor("single")

		p1 := []byte("p1 new\n")
		p2 := []byte("p2 new\n")
		singleP2 := []byte("p2 single\n")
		start := make(chan struct{})
		var wg sync.WaitGroup
		var batchErr, singleErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, batchErr = engine.Commit(context.Background(), Plan{Actor: batch, Tool: "apply_patch", Changes: []FileChange{
				{Path: "p1.txt", BaseRev: RevOf([]byte("p1 old\n")), Content: &p1},
				{Path: "p2.txt", BaseRev: RevOf([]byte("p2 old\n")), Content: &p2},
				{Path: "p3.txt", BaseRev: RevOf([]byte("p3 old\n")), Delete: true},
			}})
		}()
		go func() {
			defer wg.Done()
			<-start
			_, singleErr = engine.Commit(context.Background(), Plan{Actor: single, Tool: "write_file",
				Changes: []FileChange{{Path: "p2.txt", BaseRev: RevOf([]byte("p2 old\n")), Content: &singleP2}}})
		}()
		close(start)
		wg.Wait()

		p1Now := readFileContent(t, root, "p1.txt")
		p2Now := readFileContent(t, root, "p2.txt")
		_, p3Err := os.Stat(filepath.Join(root, "p3.txt"))
		if batchErr == nil {
			if p1Now != "p1 new\n" || p2Now != "p2 new\n" || !os.IsNotExist(p3Err) {
				t.Fatalf("round %d: batch reported success but state is p1=%q p2=%q p3exists=%v",
					round, p1Now, p2Now, p3Err == nil)
			}
			continue
		}
		// The batch was refused: none of its files changed.
		p3Raw, err := os.ReadFile(filepath.Join(root, "p3.txt"))
		if p1Now != "p1 old\n" || err != nil || string(p3Raw) != "p3 old\n" {
			t.Fatalf("round %d: refused batch left a partial change p1=%q p3=%q (%v)", round, p1Now, p3Raw, err)
		}
		if singleErr == nil && p2Now != "p2 single\n" {
			t.Fatalf("round %d: single write won but p2=%q", round, p2Now)
		}
	}
}

// A hashline edit made after another session committed an unrelated line relocates and lands; an edit
// whose own line the other session changed is refused, never applied at guessed coordinates.
func TestXReviewHashlineAnchorsAfterOtherSessionCommit(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "g.txt", xrevLines(12))
	engine := newTestEngine(t, root)
	reader := xrevActor("reader")
	writer := xrevActor("writer")
	snap := xrevRegister(t, engine, reader.Session, "g.txt")

	insert := []byte("header inserted\n" + xrevLines(12))
	if _, err := engine.Commit(context.Background(), Plan{Actor: writer, Tool: "write_file",
		Changes: []FileChange{{Path: "g.txt", Content: &insert}}}); err != nil {
		t.Fatal(err)
	}

	// Unrelated shift: the anchored line is still relocatable.
	ops := resolveOps(t, snap, AnchorOp{Op: OpReplace, Start: anchorAt(snap.Doc, 5), Text: "RELOCATED"})
	if _, err := engine.Commit(context.Background(), Plan{Actor: reader, Tool: "edit_lines",
		Changes: []FileChange{{Path: "g.txt", BaseRev: snap.Rev, LineOps: ops}}}); err != nil {
		t.Fatalf("relocatable edit refused: %v", err)
	}
	if !strings.Contains(readFileContent(t, root, "g.txt"), "RELOCATED") {
		t.Fatal("relocated edit did not land")
	}

	// Now change the anchored line itself in another session; the same anchor must be refused.
	snap2 := xrevRegister(t, engine, reader.Session, "g.txt")
	ops2 := resolveOps(t, snap2, AnchorOp{Op: OpReplace, Start: anchorAt(snap2.Doc, 3), Text: "STALE"})
	rewrite := []byte(strings.Replace(readFileContent(t, root, "g.txt"), "line 3\n", "line 3 changed\n", 1))
	if _, err := engine.Commit(context.Background(), Plan{Actor: writer, Tool: "write_file",
		Changes: []FileChange{{Path: "g.txt", Content: &rewrite}}}); err != nil {
		t.Fatal(err)
	}
	_, err := engine.Commit(context.Background(), Plan{Actor: reader, Tool: "edit_lines",
		Changes: []FileChange{{Path: "g.txt", BaseRev: snap2.Rev, LineOps: ops2}}})
	if code := CodeOf(err); code != CodeStale {
		t.Fatalf("edit of a line another session changed: %v, want stale", err)
	}
	if strings.Contains(readFileContent(t, root, "g.txt"), "STALE") {
		t.Fatal("stale edit landed")
	}
}
