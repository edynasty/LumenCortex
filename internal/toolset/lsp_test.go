package toolset

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/protocol"
)

// fakeLSPServer is the testdata language server, built once for the package: the go tool does not
// build testdata with ./..., and the guard tests must talk to a real process over real pipes.
var fakeLSPServer string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "lc-toolset-lsp-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_, file, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(file)
	path := filepath.Join(dir, "fake-lsp")
	build := exec.Command("go", "build", "-o", path, "../lsp/testdata/fakeserver")
	build.Dir = pkgDir
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build ../lsp/testdata/fakeserver: %v\n%s", err, out)
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	fakeLSPServer = path
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func fileURIOf(path string) string {
	abs, _ := filepath.Abs(path)
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}).String()
}

// renameEdit writes the workspace edit the fake server answers a rename with, and returns its path.
func renameEdit(t *testing.T, edit map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(edit)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "edit.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// replaceEdit is the common shape: replace the first `old` in each named file with newName.
func replaceEdit(files map[string]string, newName string) map[string]any {
	changes := map[string]any{}
	for path, old := range files {
		changes[fileURIOf(path)] = []map[string]any{{
			"range": map[string]any{
				"start": map[string]any{"line": 0, "character": 0},
				"end":   map[string]any{"line": 0, "character": len(old)},
			},
			"newText": newName,
		}}
	}
	return map[string]any{"changes": changes}
}

func lspSet(t *testing.T, opts Options, serverArgs ...string) *Set {
	t.Helper()
	if opts.Workspace == "" {
		opts.Workspace = t.TempDir()
	}
	manager, err := lsp.NewManager(lsp.Options{
		Workspace: opts.Workspace,
		Servers: []lsp.LanguageServer{{
			Language: "go", Name: "fake", Command: fakeLSPServer, Args: serverArgs, Extensions: []string{".go"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	opts.LSP = manager.ForRun(true)
	if opts.Policy == "" {
		opts.Policy = PolicyFull
	}
	set, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

func toolNames(specs []protocol.ToolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// The lsp_* tools are offered only when a server can really serve the workspace (owner requirement 6).
func TestLSPToolsAreOfferedOnlyWithAUsableServer(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "main.go"), "package main\n")

	withServer := toolNames(lspSet(t, Options{Workspace: workspace}).Specs(nil))
	for _, name := range []string{"lsp_hover", "lsp_definition", "lsp_references", "lsp_document_symbols",
		"lsp_workspace_symbols", "lsp_diagnostics", "lsp_rename"} {
		if !slices.Contains(withServer, name) {
			t.Fatalf("%s missing with a usable server: %v", name, withServer)
		}
	}

	// No server binary: the workspace has the file, but nothing can answer.
	missing := &Set{}
	missingManager, err := lsp.NewManager(lsp.Options{Workspace: workspace, Servers: []lsp.LanguageServer{
		{Language: "go", Command: "lcx-no-such-language-server", Extensions: []string{".go"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = missingManager.Close() })
	missing, err = New(Options{Workspace: workspace, Policy: PolicyFull, LSP: missingManager.ForRun(true)})
	if err != nil {
		t.Fatal(err)
	}
	if names := toolNames(missing.Specs(nil)); slices.Contains(names, "lsp_hover") {
		t.Fatalf("lsp tools offered without a server binary: %v", names)
	}

	// No file of the language: the server could start, but nothing here needs it.
	plain := t.TempDir()
	writeFile(t, filepath.Join(plain, "notes.txt"), "hello\n")
	if names := toolNames(lspSet(t, Options{Workspace: plain}).Specs(nil)); slices.Contains(names, "lsp_hover") {
		t.Fatalf("lsp tools offered without a file of the language: %v", names)
	}

	// No manager at all (an Engine that never built one) offers nothing either.
	bare, err := New(Options{Workspace: workspace, Policy: PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	if names := toolNames(bare.Specs(nil)); slices.Contains(names, "lsp_hover") {
		t.Fatalf("lsp tools offered without a manager: %v", names)
	}
}

// A call that cannot reach a server answers with a result that names the fallback, never an opaque error.
func TestLSPToolWithoutAServerAdvisesSearch(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "main.go"), "package main\n")
	// The command exists (it is the test's fake server), but it crashes during initialize.
	marker := filepath.Join(t.TempDir(), "crash-once")
	writeFile(t, marker, "")
	set := lspSet(t, Options{Workspace: workspace}, "-crash", "initialize", "-crash-once", marker)
	result, err := set.Execute(context.Background(), "lsp_hover", map[string]any{"path": "main.go", "line": 1, "column": 1}, nil)
	if err != nil {
		t.Fatalf("a failed server must be a result, not a Go error: %v", err)
	}
	if result.OK {
		t.Fatalf("result=%+v", result)
	}
	var payload struct {
		Code string `json:"code"`
		Hint string `json:"hint"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("content %s: %v", result.Content, err)
	}
	if payload.Code != "unavailable" || !strings.Contains(payload.Hint, "search_text") {
		t.Fatalf("payload=%+v", payload)
	}
}

func TestLSPRenameAppliesThroughTheFileGuard(t *testing.T) {
	workspace := t.TempDir()
	a := filepath.Join(workspace, "a.go")
	b := filepath.Join(workspace, "b.go")
	writeFile(t, a, "Old := 1\n")
	writeFile(t, b, "Old()\n")
	editFile := renameEdit(t, replaceEdit(map[string]string{a: "Old", b: "Old"}, "New"))
	set := lspSet(t, Options{Workspace: workspace}, "-rename-file", editFile)

	spec, ok := specOf(set, "lsp_rename")
	if !ok {
		t.Fatal("lsp_rename is not registered")
	}
	if spec.Permission != "write" || !spec.MutatesWorkspace {
		t.Fatalf("lsp_rename spec=%+v, want permission write and MutatesWorkspace", spec)
	}

	result, err := set.Execute(context.Background(), "lsp_rename", map[string]any{
		"path": "a.go", "line": 1, "column": 1, "new_name": "New",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || !result.MutatesWorkspace {
		t.Fatalf("result=%+v", result)
	}
	if got := readFile(t, a); got != "New := 1\n" {
		t.Fatalf("a.go=%q", got)
	}
	if got := readFile(t, b); got != "New()\n" {
		t.Fatalf("b.go=%q", got)
	}
	var payload struct {
		FilesChanged int `json:"filesChanged"`
	}
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.FilesChanged != 2 {
		t.Fatalf("content=%s", result.Content)
	}
}

// A rename that touches LumenCortex state is refused as a whole: the ordinary file it also names keeps
// its content.
func TestLSPRenameRefusesAProtectedPathEntirely(t *testing.T) {
	home := t.TempDir()
	stateDir := filepath.Join(home, ".local", "state", "lumencortex", "workspaces", "proj-1")
	db := filepath.Join(stateDir, "lumencortex.db")
	writeFile(t, db, "STATE-BYTES")
	mainFile := filepath.Join(home, "main.go")
	writeFile(t, mainFile, "Old := 1\n")

	editFile := renameEdit(t, replaceEdit(map[string]string{mainFile: "Old", db: "STATE"}, "New"))
	set := lspSet(t, Options{
		Workspace: home,
		Protected: Protected{Trees: []string{stateDir}},
	}, "-rename-file", editFile)

	result, err := set.Execute(context.Background(), "lsp_rename", map[string]any{
		"path": "main.go", "line": 1, "column": 1, "new_name": "New",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.MutatesWorkspace {
		t.Fatalf("result=%+v", result)
	}
	if !strings.Contains(result.Content, "refused") || !strings.Contains(result.Content, "search_text") {
		t.Fatalf("content=%s", result.Content)
	}
	if got := readFile(t, mainFile); got != "Old := 1\n" {
		t.Fatalf("the rename was applied partially: main.go=%q", got)
	}
	if got := readFile(t, db); got != "STATE-BYTES" {
		t.Fatalf("state changed: %q", got)
	}
}

// A rename whose path leads outside the workspace through a symlink is refused, and the file outside
// keeps its content.
func TestLSPRenameRefusesASymlinkEscape(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	target := filepath.Join(outside, "outside.go")
	writeFile(t, target, "Old := 1\n")
	link := filepath.Join(workspace, "link.go")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	mainFile := filepath.Join(workspace, "main.go")
	writeFile(t, mainFile, "Old := 1\n")

	editFile := renameEdit(t, replaceEdit(map[string]string{mainFile: "Old", link: "Old"}, "New"))
	set := lspSet(t, Options{Workspace: workspace}, "-rename-file", editFile)
	result, err := set.Execute(context.Background(), "lsp_rename", map[string]any{
		"path": "main.go", "line": 1, "column": 1, "new_name": "New",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.OK || result.MutatesWorkspace {
		t.Fatalf("result=%+v", result)
	}
	if got := readFile(t, target); got != "Old := 1\n" {
		t.Fatalf("the file outside the workspace changed: %q", got)
	}
	if got := readFile(t, mainFile); got != "Old := 1\n" {
		t.Fatalf("the rename was applied partially: %q", got)
	}
}

// A read-only run is not offered lsp_rename and cannot run it: a rename is a file modification.
func TestLSPRenameFollowsThePermission(t *testing.T) {
	workspace := t.TempDir()
	writeFile(t, filepath.Join(workspace, "main.go"), "Old := 1\n")
	readOnly := lspSet(t, Options{Workspace: workspace, Policy: PolicyReadOnly})
	if names := toolNames(readOnly.Specs(nil)); slices.Contains(names, "lsp_rename") {
		t.Fatalf("lsp_rename offered to a read-only run: %v", names)
	}
	if !slices.Contains(toolNames(readOnly.Specs(nil)), "lsp_hover") {
		t.Fatal("the read tools must stay available to a read-only run")
	}
	result, err := readOnly.Execute(context.Background(), "lsp_rename", map[string]any{
		"path": "main.go", "line": 1, "column": 1, "new_name": "New",
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Denied || result.OK {
		t.Fatalf("result=%+v, want denied", result)
	}
	workspacePolicy := lspSet(t, Options{Workspace: workspace, Policy: PolicyWorkspace})
	if !slices.Contains(toolNames(workspacePolicy.Specs(nil)), "lsp_rename") {
		t.Fatal("a workspace run must be offered lsp_rename")
	}
}

// A workspace edit that names a resource operation, or a file the workspace does not hold, is refused
// without writing anything.
func TestLSPRenameRefusesAnUnusableEdit(t *testing.T) {
	workspace := t.TempDir()
	mainFile := filepath.Join(workspace, "main.go")
	writeFile(t, mainFile, "Old := 1\n")

	t.Run("resource operation", func(t *testing.T) {
		editFile := renameEdit(t, map[string]any{"documentChanges": []map[string]any{{
			"kind":   "rename",
			"oldUri": fileURIOf(mainFile),
			"newUri": fileURIOf(filepath.Join(workspace, "renamed.go")),
		}}})
		set := lspSet(t, Options{Workspace: workspace}, "-rename-file", editFile)
		result, err := set.Execute(context.Background(), "lsp_rename", map[string]any{
			"path": "main.go", "line": 1, "column": 1, "new_name": "New",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.OK {
			t.Fatalf("result=%+v", result)
		}
		if got := readFile(t, mainFile); got != "Old := 1\n" {
			t.Fatalf("main.go=%q", got)
		}
	})

	t.Run("missing target", func(t *testing.T) {
		editFile := renameEdit(t, replaceEdit(map[string]string{
			mainFile:                            "Old",
			filepath.Join(workspace, "gone.go"): "Old",
		}, "New"))
		set := lspSet(t, Options{Workspace: workspace}, "-rename-file", editFile)
		result, err := set.Execute(context.Background(), "lsp_rename", map[string]any{
			"path": "main.go", "line": 1, "column": 1, "new_name": "New",
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if result.OK || result.MutatesWorkspace {
			t.Fatalf("result=%+v", result)
		}
		if got := readFile(t, mainFile); got != "Old := 1\n" {
			t.Fatalf("main.go=%q", got)
		}
	})
}

func specOf(set *Set, name string) (protocol.ToolSpec, bool) {
	for _, spec := range set.Specs(nil) {
		if spec.Name == name {
			return spec, true
		}
	}
	return protocol.ToolSpec{}, false
}
