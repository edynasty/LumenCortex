package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// xrevToolLines is a small numbered file body.
func xrevToolLines(total int) string {
	var b strings.Builder
	for line := 1; line <= total; line++ {
		fmt.Fprintf(&b, "line %d\n", line)
	}
	return b.String()
}

func xrevEngine(t *testing.T, root string) *editing.Engine {
	t.Helper()
	engine, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// xrevSet builds one session's toolset over the shared engine, the way the runtime binds each run to
// the workspace's single editing engine.
func xrevSet(t *testing.T, root string, engine *editing.Engine, session string) *Set {
	t.Helper()
	set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: session, Editing: engine})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func xrevExecute(t *testing.T, set *Set, name string, args map[string]any) (string, error) {
	t.Helper()
	result, err := set.Execute(context.Background(), name, args, nil)
	if err != nil {
		return "", err
	}
	if result.OK {
		return "", nil
	}
	var failure struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(result.Content), &failure)
	if failure.Code == "" {
		failure.Code = "refused"
	}
	return failure.Code, nil
}

// XE-03: a path component swapped for a symlink to a file outside the workspace after Resolve must
// not let the write escape. The old Set.Open followed the swap, opened the outside inode and wrote it;
// the descriptor guard now re-resolves the path and refuses.
func TestXReviewWriteRefusesSymlinkSwapOutsideWorkspace(t *testing.T) {
	ws := t.TempDir()
	outside := t.TempDir()
	ws, _ = filepath.EvalSymlinks(ws)
	outside, _ = filepath.EvalSymlinks(outside)
	victim := filepath.Join(outside, "victim.txt")
	if err := os.WriteFile(victim, []byte("ORIGINAL"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(ws, "file.txt")
	if err := os.WriteFile(target, []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	set := xrevSet(t, ws, xrevEngine(t, ws), "a")

	swapped := false
	testHookBeforeOpen = func(path string) {
		if swapped || path != target {
			return
		}
		swapped = true
		_ = os.Remove(path)
		if err := os.Symlink(victim, path); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { testHookBeforeOpen = nil }()

	_, err := set.Execute(context.Background(), "write_file", map[string]any{"path": "file.txt", "content": "ATTACK"}, nil)
	if !swapped {
		t.Fatal("the swap hook never ran")
	}
	if err == nil {
		t.Fatal("write followed a symlink swapped in after resolve")
	}
	if got, readErr := os.ReadFile(victim); readErr != nil || string(got) != "ORIGINAL" {
		t.Fatalf("outside file was modified: %q (%v)", got, readErr)
	}
}

// XE-02: apply_patch's delete must carry the revision it was resolved against, the way every other
// mutation does. The test drives the race through the open hook: with the precondition, the delete
// branch reads the file (open #1) and captures its revision, then the commit re-reads it (open #2) and
// the hook changes the file just before that re-read, so the commit refuses as stale at prepare and
// writes nothing. Without the precondition the tool has no revision to compare and the commit's
// first open is prepare's own read; the resulting plan carries no plan-time revision, which is the
// unguarded window between the patch's stat and the commit (the engine's deleteCAS is only a second,
// later barrier).
func TestXReviewApplyPatchDeleteCarriesRevision(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "gone.txt")
	if err := os.WriteFile(target, []byte("v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	set := xrevSet(t, root, xrevEngine(t, root), "a")

	var mu sync.Mutex
	opens, mutated := 0, false
	testHookBeforeOpen = func(path string) {
		if path != target {
			return
		}
		mu.Lock()
		opens++
		n := opens
		mu.Unlock()
		if n == 2 && !mutated {
			mutated = true
			if err := os.WriteFile(target, []byte("v2\n"), 0600); err != nil {
				t.Error(err)
			}
		}
	}
	defer func() { testHookBeforeOpen = nil }()

	code, err := xrevExecute(t, set, "apply_patch", map[string]any{
		"patches": []any{map[string]any{"path": "gone.txt", "operation": "delete"}},
	})
	if err != nil {
		t.Fatalf("apply_patch: %v", err)
	}
	if !mutated {
		t.Fatalf("the file was not changed before the commit re-read (opens=%d)", opens)
	}
	if code != editing.CodeStale {
		t.Fatalf("delete of a concurrently rewritten file: %q, want stale", code)
	}
	if got, readErr := os.ReadFile(target); readErr != nil || string(got) != "v2\n" {
		t.Fatalf("new content was lost: %q (%v)", got, readErr)
	}
}

// Combination 1 at the tool layer: two agents edit the same line concurrently, one through edit_lines
// and one through write_file. Exactly one commits; the loser is refused as stale/conflict; the final
// file is one writer's, never a mix.
func TestXReviewToolConcurrentLineEditAndWholeWrite(t *testing.T) {
	for round := range 30 {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(xrevToolLines(16)), 0600); err != nil {
			t.Fatal(err)
		}
		engine := xrevEngine(t, root)
		lineSet := xrevSet(t, root, engine, "agent-line")
		writeSet := xrevSet(t, root, engine, "agent-write")

		lineRev, rendered := readAnchors(t, lineSet, "f.txt", nil)
		writeRev, _ := readAnchors(t, writeSet, "f.txt", nil)
		anchor := anchorIn(t, rendered, "line 5")
		whole := strings.Replace(xrevToolLines(16), "line 5\n", "WHOLE-WRITE\n", 1)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var lineCode, writeCode string
		var lineErr, writeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			lineCode, lineErr = xrevExecute(t, lineSet, "edit_lines", map[string]any{
				"path": "f.txt", "rev": lineRev,
				"ops": []any{map[string]any{"op": "replace", "start": anchor, "text": "LINE-EDIT"}},
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			writeCode, writeErr = xrevExecute(t, writeSet, "write_file", map[string]any{
				"path": "f.txt", "content": whole, "base_rev": writeRev,
			})
		}()
		close(start)
		wg.Wait()
		if lineErr != nil || writeErr != nil {
			t.Fatalf("round %d: tool error: line=%v write=%v", round, lineErr, writeErr)
		}

		lineWon := lineCode == ""
		writeWon := writeCode == ""
		if lineWon == writeWon {
			t.Fatalf("round %d: exactly one must win: line=%q write=%q", round, lineCode, writeCode)
		}
		loser := lineCode
		if lineWon {
			loser = writeCode
		}
		if loser != editing.CodeStale && loser != editing.CodeConflict {
			t.Fatalf("round %d: loser code %q, want stale/conflict", round, loser)
		}
		got, err := os.ReadFile(filepath.Join(root, "f.txt"))
		if err != nil {
			t.Fatal(err)
		}
		want := "LINE-EDIT"
		if writeWon {
			want = "WHOLE-WRITE"
		}
		if !strings.Contains(string(got), want) {
			t.Fatalf("round %d: winner's content missing:\n%s", round, got)
		}
		if strings.Contains(string(got), "LINE-EDIT") && strings.Contains(string(got), "WHOLE-WRITE") {
			t.Fatalf("round %d: final file mixes both writers", round)
		}
	}
}

// Combination 3 at the tool layer: an apply_patch batch that updates two files and deletes a third
// races a single write_file to one of them. The batch is all-or-nothing.
func TestXReviewToolMultiFilePatchAgainstSingleWrite(t *testing.T) {
	for round := range 30 {
		root := t.TempDir()
		for name, body := range map[string]string{"p1.txt": "p1 old\n", "p2.txt": "p2 old\n", "p3.txt": "p3 old\n"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
		}
		engine := xrevEngine(t, root)
		batchSet := xrevSet(t, root, engine, "batch")
		singleSet := xrevSet(t, root, engine, "single")
		singleRev, _ := readAnchors(t, singleSet, "p2.txt", nil)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var batchCode, singleCode string
		var batchErr, singleErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			batchCode, batchErr = xrevExecute(t, batchSet, "apply_patch", map[string]any{"patches": []any{
				map[string]any{"path": "p1.txt", "operation": "update", "edits": []any{map[string]any{"old_text": "p1 old\n", "new_text": "p1 new\n"}}},
				map[string]any{"path": "p2.txt", "operation": "update", "edits": []any{map[string]any{"old_text": "p2 old\n", "new_text": "p2 new\n"}}},
				map[string]any{"path": "p3.txt", "operation": "delete"},
			}})
		}()
		go func() {
			defer wg.Done()
			<-start
			singleCode, singleErr = xrevExecute(t, singleSet, "write_file", map[string]any{
				"path": "p2.txt", "content": "p2 single\n", "base_rev": singleRev,
			})
		}()
		close(start)
		wg.Wait()
		if singleErr != nil {
			t.Fatalf("round %d: single write tool error: %v", round, singleErr)
		}
		if batchErr != nil {
			// A pre-validation refusal (the single write landed first and the batch's old_text no
			// longer matches) writes nothing, which is the same all-or-nothing outcome as a
			// commit-level refusal.
			batchCode = "refused"
		}

		p1 := readTestFile(t, root, "p1.txt")
		p2 := readTestFile(t, root, "p2.txt")
		p3, p3Err := os.ReadFile(filepath.Join(root, "p3.txt"))
		if batchCode == "" {
			if p1 != "p1 new\n" || p2 != "p2 new\n" || p3Err == nil {
				t.Fatalf("round %d: batch succeeded but state is p1=%q p2=%q p3=%q (%v)", round, p1, p2, p3, p3Err)
			}
			continue
		}
		if p1 != "p1 old\n" || p3Err != nil || string(p3) != "p3 old\n" {
			t.Fatalf("round %d: refused batch left a partial change p1=%q p3=%q (%v)", round, p1, p3, p3Err)
		}
		if singleCode == "" && p2 != "p2 single\n" {
			t.Fatalf("round %d: single write won but p2=%q", round, p2)
		}
	}
}

// Combination 2: an eval artifact export while an agent edits the same path. A lease held by another
// session refuses the export; a stale read of the target refuses it; without either, a concurrent
// export and edit never mix bytes and the survivor is exactly one writer's payload.
func TestXReviewArtifactExportAgainstEditing(t *testing.T) {
	t.Run("lease", func(t *testing.T) {
		root := t.TempDir()
		engine := xrevEngine(t, root)
		editor := xrevSet(t, root, engine, "agent-a")
		exporter := xrevSet(t, root, engine, "agent-b")
		path := filepath.Join(root, "out.bin")
		if err := os.WriteFile(path, []byte("owner content"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := engine.Leases().Acquire(root, editing.CanonicalPath(path), "agent-a", false, time.Minute); err != nil {
			t.Fatal(err)
		}
		if err := exporter.WriteArtifact(context.Background(), "out.bin", []byte("ARTIFACT")); err == nil {
			t.Fatal("artifact export ignored another session's write lease")
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != "owner content" {
			t.Fatalf("lease owner's file changed: %q (%v)", got, err)
		}
		_ = editor
	})

	t.Run("stale-read", func(t *testing.T) {
		root := t.TempDir()
		engine := xrevEngine(t, root)
		editor := xrevSet(t, root, engine, "agent-a")
		exporter := xrevSet(t, root, engine, "agent-b")
		if err := os.WriteFile(filepath.Join(root, "out.bin"), []byte("v1\n"), 0600); err != nil {
			t.Fatal(err)
		}
		// The exporting session reads the target, then the editor commits a newer revision.
		if _, err := exporter.Execute(context.Background(), "read_file", map[string]any{"path": "out.bin"}, nil); err != nil {
			t.Fatal(err)
		}
		rev, _ := readAnchors(t, editor, "out.bin", nil)
		if code, err := xrevExecute(t, editor, "write_file", map[string]any{"path": "out.bin", "content": "agent content\n", "base_rev": rev}); err != nil || code != "" {
			t.Fatalf("editor write: %q %v", code, err)
		}
		err := exporter.WriteArtifact(context.Background(), "out.bin", []byte("ARTIFACT"))
		editErr, ok := editing.AsError(err)
		if !ok || (editErr.Code != editing.CodeConflict && editErr.Code != editing.CodeStale) {
			t.Fatalf("stale artifact export: %v, want conflict/stale", err)
		}
		if got, err := os.ReadFile(filepath.Join(root, "out.bin")); err != nil || string(got) != "agent content\n" {
			t.Fatalf("editor's content lost: %q (%v)", got, err)
		}
	})

	t.Run("concurrent", func(t *testing.T) {
		for round := range 20 {
			root := t.TempDir()
			engine := xrevEngine(t, root)
			editor := xrevSet(t, root, engine, "agent-a")
			exporter := xrevSet(t, root, engine, "agent-b")
			if err := os.WriteFile(filepath.Join(root, "out.bin"), []byte("base\n"), 0600); err != nil {
				t.Fatal(err)
			}
			rev, _ := readAnchors(t, editor, "out.bin", nil)
			artifact := []byte("ARTIFACT-BYTES\n")

			start := make(chan struct{})
			var wg sync.WaitGroup
			var editCode, exportErr string
			var editToolErr, exportToolErr error
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				editCode, editToolErr = xrevExecute(t, editor, "write_file", map[string]any{
					"path": "out.bin", "content": "AGENT-BYTES\n", "base_rev": rev,
				})
			}()
			go func() {
				defer wg.Done()
				<-start
				err := exporter.WriteArtifact(context.Background(), "out.bin", artifact)
				if err != nil {
					exportErr = editing.CodeOf(err)
					if exportErr == "" {
						exportToolErr = err
					}
				}
			}()
			close(start)
			wg.Wait()
			if editToolErr != nil || exportToolErr != nil {
				t.Fatalf("round %d: tool error: edit=%v export=%v", round, editToolErr, exportToolErr)
			}
			got, err := os.ReadFile(filepath.Join(root, "out.bin"))
			if err != nil {
				t.Fatal(err)
			}
			switch string(got) {
			case "AGENT-BYTES\n", "ARTIFACT-BYTES\n":
			default:
				t.Fatalf("round %d: final bytes %q are a mix or a third value (edit=%q export=%q)", round, got, editCode, exportErr)
			}
			// Without a lease the two writers serialize through the same commit path: a later export
			// may legitimately replace the editor's bytes, but the result is always exactly one
			// writer's payload (checked above) and the editor's stale BaseRev is still refused.
			_ = editCode
			_ = exportErr
		}
	})
}

// Combination 5: an edit_go rename across two files races a write to one target file. The rename is
// atomic across its files and the call sites stay consistent: a renamed declaration never coexists
// with a stale call site.
func TestXReviewGoRenameAcrossFilesAgainstWrite(t *testing.T) {
	for round := range 15 {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package demo\n\nfunc Foo() int { return 1 }\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package demo\n\nfunc Use() int {\n\treturn Foo()\n}\n"), 0600); err != nil {
			t.Fatal(err)
		}
		engine := xrevEngine(t, root)
		renamer := xrevSet(t, root, engine, "renamer")
		writer := xrevSet(t, root, engine, "writer")
		writeRev, _ := readAnchors(t, writer, "b.go", nil)

		start := make(chan struct{})
		var wg sync.WaitGroup
		var renameCode, writeCode string
		var renameErr, writeErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			renameCode, renameErr = xrevExecute(t, renamer, "edit_go", map[string]any{
				"path": "a.go",
				"ops":  []any{map[string]any{"op": "rename", "symbol": "demo.Foo", "new_name": "Bar"}},
			})
		}()
		go func() {
			defer wg.Done()
			<-start
			writeCode, writeErr = xrevExecute(t, writer, "write_file", map[string]any{
				"path": "b.go", "content": "package demo\n\nfunc Use() int {\n\treturn Foo() + 0\n}\n", "base_rev": writeRev,
			})
		}()
		close(start)
		wg.Wait()
		if renameErr != nil || writeErr != nil {
			t.Fatalf("round %d: tool error: rename=%v write=%v", round, renameErr, writeErr)
		}
		_ = renameCode
		_ = writeCode

		a := readTestFile(t, root, "a.go")
		b := readTestFile(t, root, "b.go")
		if strings.Contains(a, "func Bar(") {
			if !strings.Contains(b, "Bar()") || strings.Contains(b, "Foo()") {
				t.Fatalf("round %d: declaration renamed but call site is inconsistent:\na=%s\nb=%s", round, a, b)
			}
		} else if strings.Contains(b, "Bar(") {
			t.Fatalf("round %d: call site renamed but declaration is not:\na=%s\nb=%s", round, a, b)
		}
	}
}
