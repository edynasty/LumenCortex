package goedit

import (
 "os"
 "path/filepath"
 "strings"
 "testing"
 "github.com/edynasty/LumenCortex/internal/editing"
)

func TestReviewMethodRenameResolvesReceiver(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a.go")
	original := `package demo
 type A struct{}
 type B struct{}
 func (A) Foo() int {return 1}
 func (B) Foo() int {return 2}
 func (B) Bar() int {return 3}
 func Both() int {return A{}.Foo()*10+B{}.Foo()}
 `
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = commit(t, dirSource{root: root}, target, e, Op{Kind: OpRename, Symbol: "A.Foo", NewName: "Bar"})
	if err != nil {
		t.Fatal(err)
	}
	got := readFile(t, target)
	if !strings.Contains(got, "A{}.Bar()*10+B{}.Foo()") {
		t.Fatalf("rename changed receiver behavior:\n%s", got)
	}
}
