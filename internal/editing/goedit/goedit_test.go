package goedit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// The structural cases of the roadmap's E4 prototype, ported to the package: symbol-addressed body
// replacement that leaves every other byte alone, a scope-aware parameter rename against a shadowed
// name, a refused capture, a multi-file rename, an import plus its use validated by the type checker,
// refused syntax, refused types, and a symbol path that does not silently pick a target.

const fileA = `package demo

import (
	"fmt"
)

type T struct{ n int }

// Foo keeps this comment.
func (t *T) Foo(x int) int {
	y := x * 2 // double
	{
		x := 100 // shadowed: must NOT be renamed
		_ = x
	}
	return y + x
}

func helper(x int) string { return fmt.Sprint(x) }
`

const fileB = `package demo

func useHelper() string { return helper(1) + helper(2) }
`

// dirSource is a Source over a directory of files.
type dirSource struct{ root string }

func (s dirSource) Read(path string) ([]byte, error) { return os.ReadFile(path) }

func (s dirSource) List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		out = append(out, filepath.Join(dir, entry.Name()))
	}
	return out, nil
}

// fixture writes the prototype's two files and returns the source and the a.go path.
func fixture(t *testing.T) (dirSource, string, *editing.Engine) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(fileA), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte(fileB), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	return dirSource{root: root}, filepath.Join(root, "a.go"), engine
}

// commit plans the ops and commits them with the Go syntax and type validators.
func commit(t *testing.T, src dirSource, target string, engine *editing.Engine, ops ...Op) (editing.Result, error) {
	t.Helper()
	plan, err := Plan(src, target, ops)
	if err != nil {
		return editing.Result{}, err
	}
	return engine.Commit(context.Background(), editing.Plan{
		Actor:    editing.Actor{Session: "s"},
		Tool:     "edit_go",
		Changes:  plan.Changes,
		Validate: []editing.Validator{editing.GoTypes()},
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A1: the body of (*T).Foo is replaced and every byte outside it, comments included, is unchanged.
func TestReplaceBodyBySymbol(t *testing.T) {
	src, target, engine := fixture(t)
	if _, err := commit(t, src, target, engine, Op{Kind: OpReplaceBody, Symbol: "(*T).Foo", Text: "{\n\treturn x * 3\n}"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := readFile(t, target)
	if !strings.Contains(got, "// Foo keeps this comment.") || !strings.Contains(got, "return x * 3") {
		t.Fatalf("body not replaced in place:\n%s", got)
	}
	if strings.Contains(got, "y := x * 2") {
		t.Fatalf("old body remains:\n%s", got)
	}
	if other := readFile(t, filepath.Join(filepath.Dir(target), "b.go")); other != fileB {
		t.Fatalf("b.go changed:\n%s", other)
	}
}

// A2: renaming the parameter x to count renames its uses only; the shadowed x and helper's x survive.
func TestRenameParamIsScopeAware(t *testing.T) {
	src, target, engine := fixture(t)
	if _, err := commit(t, src, target, engine, Op{Kind: OpRenameParam, Symbol: "(*T).Foo", Name: "x", NewName: "count"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := readFile(t, target)
	for _, want := range []string{"Foo(count int)", "y := count * 2", "x := 100", "return y + count", "func helper(x int)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// A naive textual rename would break the package; the scope-aware one keeps it type-correct.
	if _, err := Plan(src, target, []Op{{Kind: OpFind, Symbol: "helper"}}); err != nil {
		t.Fatalf("the package no longer resolves: %v", err)
	}
}

// A3: a rename whose new name is already declared in the body is refused before any write.
func TestRenameParamRefusesCapture(t *testing.T) {
	src, target, _ := fixture(t)
	_, err := Plan(src, target, []Op{{Kind: OpRenameParam, Symbol: "(*T).Foo", Name: "x", NewName: "y"}})
	editErr, ok := editing.AsError(err)
	if !ok || editErr.Code != editing.CodeAmbiguous || !strings.Contains(editErr.Detail, "capture") {
		t.Fatalf("err = %v, want a capture refusal", err)
	}
	if got := readFile(t, target); got != fileA {
		t.Fatal("a refused rename changed the file")
	}
}

// A4: a package-level rename reaches every file and every use.
func TestRenamePackageSymbolAcrossFiles(t *testing.T) {
	src, target, engine := fixture(t)
	plan, err := Plan(src, target, []Op{{Kind: OpRename, Symbol: "helper", NewName: "render"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(plan.Changes) != 2 {
		t.Fatalf("rename touched %d files, want 2: %+v", len(plan.Changes), plan.Changes)
	}
	if _, err := engine.Commit(context.Background(), editing.Plan{
		Actor: editing.Actor{Session: "s"}, Tool: "edit_go", Changes: plan.Changes,
		Validate: []editing.Validator{editing.GoTypes()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := readFile(t, filepath.Join(filepath.Dir(target), "b.go")); strings.Count(got, "render(") != 2 {
		t.Fatalf("call sites not renamed:\n%s", got)
	}
	if got := readFile(t, target); !strings.Contains(got, "func render(x int)") {
		t.Fatalf("declaration not renamed:\n%s", got)
	}
}

// A5: an added import and a body that uses it pass the type check.
func TestAddImportAndUseIt(t *testing.T) {
	src, target, engine := fixture(t)
	if _, err := commit(t, src, target, engine,
		Op{Kind: OpAddImport, ImportPath: "strings"},
		Op{Kind: OpReplaceBody, Symbol: "helper", Text: "{ return strings.Repeat(fmt.Sprint(x), 1) }"},
	); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := readFile(t, target); !strings.Contains(got, `"strings"`) {
		t.Fatalf("import missing:\n%s", got)
	}
}

// A6: a replacement with a syntax error is refused before the write.
func TestReplacementWithSyntaxErrorIsRefused(t *testing.T) {
	src, target, engine := fixture(t)
	_, err := commit(t, src, target, engine, Op{Kind: OpReplaceBody, Symbol: "helper", Text: "{ return fmt.Sprint(x "})
	if code := editing.CodeOf(err); code != editing.CodeSyntax {
		t.Fatalf("err = %v, want syntax", err)
	}
	if got := readFile(t, target); got != fileA {
		t.Fatal("a refused edit changed the file")
	}
}

// A7: a replacement that does not type check is refused before the write.
func TestReplacementWithTypeErrorIsRefused(t *testing.T) {
	src, target, engine := fixture(t)
	_, err := commit(t, src, target, engine, Op{Kind: OpReplaceBody, Symbol: "helper", Text: "{ return x }"})
	if code := editing.CodeOf(err); code != editing.CodeType {
		t.Fatalf("err = %v, want type", err)
	}
	if got := readFile(t, target); got != fileA {
		t.Fatal("a refused edit changed the file")
	}
}

// A8: an unqualified method path does not silently pick a target.
func TestUnqualifiedMethodPathIsRefused(t *testing.T) {
	src, target, _ := fixture(t)
	_, err := Plan(src, target, []Op{{Kind: OpReplaceBody, Symbol: "Foo", Text: "{}"}})
	if code := editing.CodeOf(err); code != editing.CodeUnknownSymbol {
		t.Fatalf("err = %v, want unknown_symbol", err)
	}
}

// A method rename updates the declaration and its call sites.
func TestRenameMethodUpdatesCallSites(t *testing.T) {
	root := t.TempDir()
	const a = `package demo

type T struct{}

func (t *T) Foo() int { return 1 }
`
	const b = `package demo

func use() int {
	t := &T{}
	return t.Foo()
}
`
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte(b), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := commit(t, dirSource{root: root}, filepath.Join(root, "a.go"), engine,
		Op{Kind: OpRename, Symbol: "(*T).Foo", NewName: "Bar"}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "a.go")); !strings.Contains(got, "func (t *T) Bar()") {
		t.Fatalf("declaration not renamed:\n%s", got)
	}
	if got := readFile(t, filepath.Join(root, "b.go")); !strings.Contains(got, "t.Bar()") {
		t.Fatalf("call site not renamed:\n%s", got)
	}
}

// Struct member operations land at the right line and keep the type valid.
func TestStructMemberOperations(t *testing.T) {
	root := t.TempDir()
	const a = `package demo

type Config struct {
	Name string
}
`
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(a), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	src := dirSource{root: root}
	target := filepath.Join(root, "a.go")
	if _, err := commit(t, src, target, engine, Op{Kind: OpAddMember, Symbol: "Config", Name: "Size", Text: "Size int"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := readFile(t, target); !strings.Contains(got, "\tName string\n\tSize int\n") {
		t.Fatalf("member not added:\n%s", got)
	}
	if _, err := commit(t, src, target, engine, Op{Kind: OpReplaceMember, Symbol: "Config", Name: "Size", Text: "Size int64"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := readFile(t, target); !strings.Contains(got, "\tSize int64\n") {
		t.Fatalf("member not replaced:\n%s", got)
	}
	if _, err := commit(t, src, target, engine, Op{Kind: OpRemoveMember, Symbol: "Config", Name: "Name"}); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := readFile(t, target); strings.Contains(got, "Name string") || !strings.Contains(got, "\tSize int64\n") {
		t.Fatalf("member not removed cleanly:\n%s", got)
	}
}

// A preview reports the diff and writes nothing.
func TestPreviewWritesNothing(t *testing.T) {
	src, target, engine := fixture(t)
	plan, err := Plan(src, target, []Op{{Kind: OpReplaceBody, Symbol: "(*T).Foo", Text: "{ return 1 }"}})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	result, err := engine.Commit(context.Background(), editing.Plan{
		Actor: editing.Actor{Session: "s"}, Tool: "edit_go", Changes: plan.Changes, Preview: true,
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(result.Diffs) != 1 || !strings.Contains(result.Diffs[0], "-func (t *T) Foo(x int) int {") || !strings.Contains(result.Diffs[0], "+func (t *T) Foo(x int) int { return 1 }") {
		t.Fatalf("diffs = %#v", result.Diffs)
	}
	if got := readFile(t, target); got != fileA {
		t.Fatal("a preview wrote the file")
	}
}
