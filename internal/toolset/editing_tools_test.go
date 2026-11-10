package toolset

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
	"github.com/edynasty/LumenCortex/protocol"
)

func newEditingSet(t *testing.T, root string) *Set {
	t.Helper()
	set, err := New(Options{Workspace: root, Policy: PolicyFull, SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func callTool(t *testing.T, set *Set, name string, args map[string]any) protocol.ToolResult {
	t.Helper()
	result, err := set.Execute(context.Background(), name, args, nil)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}

// readAnchors reads a file the way the model does before editing, and returns the header revision.
func readAnchors(t *testing.T, set *Set, path string, extra map[string]any) (string, string) {
	t.Helper()
	args := map[string]any{"path": path, "anchors": true}
	for key, value := range extra {
		args[key] = value
	}
	result := callTool(t, set, "read_file", args)
	if !result.OK {
		t.Fatalf("read_file = %s", result.Content)
	}
	header := strings.SplitN(result.Content, "\n", 2)[0]
	rev := ""
	for _, field := range strings.Fields(header) {
		if strings.HasPrefix(field, "rev=") {
			rev = strings.TrimPrefix(field, "rev=")
		}
	}
	if rev == "" {
		t.Fatalf("no revision in header %q", header)
	}
	return rev, result.Content
}

// anchorIn finds the "N:hhh|" anchor of the first row whose text contains want.
func anchorIn(t *testing.T, rendered, want string) string {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		if !strings.Contains(line, "|") {
			continue
		}
		anchor, text, _ := strings.Cut(line, "|")
		if strings.Contains(text, want) {
			if _, _, ok := strings.Cut(anchor, ":"); ok {
				return anchor
			}
		}
	}
	t.Fatalf("no row containing %q in:\n%s", want, rendered)
	return ""
}

// edit_lines addresses lines of the revision read_file showed and returns the new revision plus fresh
// anchors around the change.
func TestEditLinesThroughToolset(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "demo.go", "package demo\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	set := newEditingSet(t, root)
	rev, rendered := readAnchors(t, set, "demo.go", nil)
	start := anchorIn(t, rendered, "return a - b")

	result := callTool(t, set, "edit_lines", map[string]any{
		"path": "demo.go", "rev": rev,
		"ops": []any{map[string]any{"op": "replace", "start": start, "text": "\treturn a + b"}},
	})
	if !result.OK {
		t.Fatalf("edit_lines = %s", result.Content)
	}
	var out struct {
		Path    string `json:"path"`
		Rev     string `json:"rev"`
		Applied int    `json:"applied"`
		Anchors string `json:"anchors"`
	}
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Applied != 1 || out.Rev == "" || out.Rev == rev {
		t.Fatalf("result = %s", result.Content)
	}
	if !strings.Contains(out.Anchors, "return a + b") || !strings.Contains(out.Anchors, "rev="+out.Rev) {
		t.Fatalf("anchors = %q", out.Anchors)
	}
	if got := readTestFile(t, root, "demo.go"); !strings.Contains(got, "return a + b") {
		t.Fatalf("file = %q", got)
	}
	if !result.MutatesWorkspace {
		t.Fatal("an edit must report a workspace change")
	}

	// The fresh anchors are editable without another read.
	next := anchorIn(t, out.Anchors, "return a + b")
	result = callTool(t, set, "edit_lines", map[string]any{
		"path": "demo.go", "rev": out.Rev,
		"ops": []any{map[string]any{"op": "replace", "start": next, "text": "\treturn a * b"}},
	})
	if !result.OK {
		t.Fatalf("second edit_lines = %s", result.Content)
	}
	if got := readTestFile(t, root, "demo.go"); !strings.Contains(got, "return a * b") {
		t.Fatalf("file = %q", got)
	}
}

// A stale anchored line is refused with the code and the remediation, and the other writer's change
// stays.
func TestEditLinesRefusesStaleAnchorThroughToolset(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "demo.go", "package demo\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n")
	set := newEditingSet(t, root)
	rev, rendered := readAnchors(t, set, "demo.go", nil)
	start := anchorIn(t, rendered, "return a - b")

	changed := "package demo\n\nfunc Add(a, b int) int {\n\treturn a % b\n}\n"
	writeTestFile(t, root, "demo.go", changed)
	result := callTool(t, set, "edit_lines", map[string]any{
		"path": "demo.go", "rev": rev,
		"ops": []any{map[string]any{"op": "replace", "start": start, "text": "\treturn a + b"}},
	})
	if result.OK {
		t.Fatalf("the edit was applied over a changed line: %s", result.Content)
	}
	var failure struct {
		Code string `json:"code"`
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(result.Content), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != editing.CodeStale || !strings.Contains(failure.Hint, "re-read") {
		t.Fatalf("failure = %s", result.Content)
	}
	if got := readTestFile(t, root, "demo.go"); got != changed {
		t.Fatalf("the other writer's change was lost: %q", got)
	}
}

// A plain read records the revision but not the lines, so edit_lines asks for an anchored read.
func TestEditLinesNeedsAnchoredRead(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "alpha\nbeta\n")
	set := newEditingSet(t, root)
	plain := callTool(t, set, "read_file", map[string]any{"path": "a.txt"})
	var read struct {
		Rev string `json:"rev"`
	}
	if err := json.Unmarshal([]byte(plain.Content), &read); err != nil {
		t.Fatal(err)
	}
	if read.Rev == "" {
		t.Fatalf("a plain read did not report its revision: %s", plain.Content)
	}
	result := callTool(t, set, "edit_lines", map[string]any{
		"path": "a.txt", "rev": read.Rev,
		"ops": []any{map[string]any{"op": "delete", "start": "1:abc"}},
	})
	if result.OK {
		t.Fatal("an unanchored revision was editable")
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(result.Content), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != editing.CodeUnknownRev {
		t.Fatalf("code = %q in %s", failure.Code, result.Content)
	}
}

// write_file refuses a whole-file write computed from an old read once the file changed (E3 S7b).
func TestWriteFileRefusesStaleReadThroughToolset(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "alpha\n")
	set := newEditingSet(t, root)
	if result := callTool(t, set, "read_file", map[string]any{"path": "a.txt"}); !result.OK {
		t.Fatalf("read = %s", result.Content)
	}
	writeTestFile(t, root, "a.txt", "alpha\nconcurrent\n")

	result := callTool(t, set, "write_file", map[string]any{"path": "a.txt", "content": "rewritten\n"})
	if result.OK {
		t.Fatalf("a stale whole-file write succeeded: %s", result.Content)
	}
	var failure struct {
		Code string `json:"code"`
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(result.Content), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != editing.CodeConflict || !strings.Contains(failure.Hint, "re-read") {
		t.Fatalf("failure = %s", result.Content)
	}
	if got := readTestFile(t, root, "a.txt"); got != "alpha\nconcurrent\n" {
		t.Fatalf("the concurrent write was lost: %q", got)
	}

	// Re-reading makes the write legal again, and so does passing the revision that is current.
	if result := callTool(t, set, "read_file", map[string]any{"path": "a.txt"}); !result.OK {
		t.Fatalf("re-read = %s", result.Content)
	}
	result = callTool(t, set, "write_file", map[string]any{"path": "a.txt", "content": "rewritten\n"})
	if !result.OK {
		t.Fatalf("write after re-read = %s", result.Content)
	}
	if got := readTestFile(t, root, "a.txt"); got != "rewritten\n" {
		t.Fatalf("file = %q", got)
	}
}

// A create is never refused by a stale read: there was no read of it.
func TestWriteFileCreatesWithoutARead(t *testing.T) {
	root := t.TempDir()
	set := newEditingSet(t, root)
	result := callTool(t, set, "write_file", map[string]any{"path": "new/created.txt", "content": "hello\n"})
	if !result.OK {
		t.Fatalf("create = %s", result.Content)
	}
	if got := readTestFile(t, root, "new/created.txt"); got != "hello\n" {
		t.Fatalf("file = %q", got)
	}
}

// Four agents edit different lines of one file through the real tool registry: every edit lands, or
// is refused explicitly, and no update is lost.
func TestEditLinesConcurrentAgentsThroughToolset(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	for line := 1; line <= 40; line++ {
		b.WriteString("line ")
		b.WriteString(stringRune(line))
		b.WriteString("\n")
	}
	writeTestFile(t, root, "f.txt", b.String())
	set := newEditingSet(t, root)
	rev, rendered := readAnchors(t, set, "f.txt", nil)

	targets := []struct {
		anchor string
		text   string
	}{
		{anchorIn(t, rendered, "line "+stringRune(5)), "EDIT five"},
		{anchorIn(t, rendered, "line "+stringRune(15)), "EDIT fifteen"},
		{anchorIn(t, rendered, "line "+stringRune(25)), "EDIT twentyfive"},
		{anchorIn(t, rendered, "line "+stringRune(35)), "EDIT thirtyfive"},
	}
	results := make([]protocol.ToolResult, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], _ = set.Execute(context.Background(), "edit_lines", map[string]any{
				"path": "f.txt", "rev": rev,
				"ops": []any{map[string]any{"op": "replace", "start": targets[i].anchor, "text": targets[i].text}},
			}, nil)
		}(i)
	}
	wg.Wait()
	for i, result := range results {
		if !result.OK {
			t.Fatalf("agent %d was refused: %s", i, result.Content)
		}
	}
	got := readTestFile(t, root, "f.txt")
	for _, target := range targets {
		if !strings.Contains(got, target.text) {
			t.Fatalf("lost update: %q missing in\n%s", target.text, got)
		}
	}
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 40 {
		t.Fatalf("line count = %d", len(lines))
	}
}

func stringRune(n int) string {
	return string([]rune{rune('a' + (n % 26)), rune('a' + (n/26)%26)})
}

// edit_go commits a structural edit through the registry, and preview writes nothing.
func TestEditGoThroughToolset(t *testing.T) {
	root := t.TempDir()
	const source = `package demo

// Add adds.
func Add(a, b int) int {
	return a - b
}
`
	writeTestFile(t, root, "demo.go", source)
	set := newEditingSet(t, root)

	preview := callTool(t, set, "edit_go", map[string]any{
		"path": "demo.go", "preview": true,
		"ops": []any{map[string]any{"op": "replace_body", "symbol": "demo.Add", "text": "{\n\treturn a + b\n}"}},
	})
	if !preview.OK {
		t.Fatalf("preview = %s", preview.Content)
	}
	var previewOut struct {
		Preview bool     `json:"preview"`
		Diffs   []string `json:"diffs"`
	}
	if err := json.Unmarshal([]byte(preview.Content), &previewOut); err != nil {
		t.Fatal(err)
	}
	if !previewOut.Preview || len(previewOut.Diffs) != 1 ||
		!strings.Contains(previewOut.Diffs[0], "+\treturn a + b") || !strings.Contains(previewOut.Diffs[0], "-\treturn a - b") {
		t.Fatalf("preview = %s", preview.Content)
	}
	if got := readTestFile(t, root, "demo.go"); got != source {
		t.Fatal("a preview wrote the file")
	}

	result := callTool(t, set, "edit_go", map[string]any{
		"path": "demo.go",
		"ops":  []any{map[string]any{"op": "replace_body", "symbol": "demo.Add", "text": "{\n\treturn a + b\n}"}},
	})
	if !result.OK {
		t.Fatalf("edit_go = %s", result.Content)
	}
	got := readTestFile(t, root, "demo.go")
	if !strings.Contains(got, "return a + b") || !strings.Contains(got, "// Add adds.") {
		t.Fatalf("file = %q", got)
	}
	if !result.MutatesWorkspace {
		t.Fatal("edit_go did not report a mutation")
	}

	// A symbol that does not exist is refused with a code the model can act on.
	refused := callTool(t, set, "edit_go", map[string]any{
		"path": "demo.go",
		"ops":  []any{map[string]any{"op": "replace_body", "symbol": "Nope", "text": "{}"}},
	})
	if refused.OK {
		t.Fatal("an unknown symbol was accepted")
	}
	var failure struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(refused.Content), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != editing.CodeUnknownSymbol {
		t.Fatalf("code = %q", failure.Code)
	}
}

// read_file's default output keeps the shape existing callers parse.
func TestReadFileDefaultOutputIsCompatible(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "a.txt", "hello\n")
	set := newEditingSet(t, root)
	result := callTool(t, set, "read_file", map[string]any{"path": "a.txt"})
	var out struct {
		Path      string `json:"path"`
		Content   string `json:"content"`
		Bytes     int    `json:"bytes"`
		Truncated bool   `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(result.Content), &out); err != nil {
		t.Fatal(err)
	}
	if out.Path != "a.txt" || out.Content != "hello\n" || out.Bytes != 6 || out.Truncated {
		t.Fatalf("read = %s", result.Content)
	}
	// The anchored view pages and names the revision.
	paged, _ := readAnchors(t, set, "a.txt", map[string]any{"start_line": 1, "end_line": 1})
	if paged == "" {
		t.Fatal("no revision")
	}
}

func writeTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, root, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
