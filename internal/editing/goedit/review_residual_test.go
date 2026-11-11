package goedit

import (
	"github.com/edynasty/LumenCortex/internal/editing"
	"os"
	"path/filepath"
	"testing"
)

func reviewRename(t *testing.T, original, symbol, newName string) (string, error) {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, "a.go")
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	e, err := editing.New(editing.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = commit(t, dirSource{root: root}, target, e, Op{Kind: OpRename, Symbol: symbol, NewName: newName})
	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(got), err
}

func TestReviewMethodRenameRejectsPromotedSelectorCapture(t *testing.T) {
	original := "package demo\ntype A struct{}\nfunc (A) Foo() int { return 1 }\ntype C struct{ A }\nfunc (C) Bar() int { return 2 }\nfunc Use() int { return C{}.Foo() }\n"
	got, err := reviewRename(t, original, "A.Foo", "Bar")
	if err == nil {
		t.Fatal("rename rebound promoted method call")
	}
	if got != original {
		t.Fatalf("refused rename mutated source:\n%s", got)
	}
}

func TestReviewMethodRenameRejectsPromotedFieldCapture(t *testing.T) {
	original := "package demo\ntype A struct{}\nfunc (A) Foo() int { return 1 }\ntype C struct{ A; Bar func() int }\nfunc Use() int { return C{Bar: func() int { return 2 }}.Foo() }\n"
	got, err := reviewRename(t, original, "A.Foo", "Bar")
	if err == nil {
		t.Fatal("rename rebound promoted method call to field")
	}
	if got != original {
		t.Fatalf("refused rename mutated source:\n%s", got)
	}
}

func TestReviewMethodRenameRejectsReversePromotedCapture(t *testing.T) {
	original := "package demo\ntype A struct{}\nfunc (A) Foo() int { return 1 }\ntype F struct{}\nfunc (F) Bar() int { return 2 }\ntype E struct{ F }\ntype C struct{ A; E }\nfunc Use() int { return C{}.Bar() }\n"
	got, err := reviewRename(t, original, "A.Foo", "Bar")
	if err == nil {
		t.Fatal("rename rebound a shallower promoted selector")
	}
	if got != original {
		t.Fatalf("refused rename mutated source:\n%s", got)
	}
}

func TestReviewMethodRenameRejectsReversePromotedInterfaceCapture(t *testing.T) {
	original := "package demo\ntype A struct{}\nfunc (A) Foo() int { return 1 }\ntype F struct{}\nfunc (F) Bar() int { return 2 }\ntype E struct{ F }\ntype C struct{ A; E }\nfunc Use() int { var i interface{ Bar() int } = C{}; return i.Bar() }\n"
	got, err := reviewRename(t, original, "A.Foo", "Bar")
	if err == nil {
		t.Fatal("rename changed interface dispatch")
	}
	if got != original {
		t.Fatalf("refused rename mutated source:\n%s", got)
	}
}

func TestReviewMethodRenameRejectsAnonymousPromotedCapture(t *testing.T) {
	original := "package demo\ntype A struct{}\nfunc (A) Foo() int { return 1 }\ntype F struct{}\nfunc (F) Bar() int { return 2 }\ntype E struct{ F }\nfunc Use() int { return struct{ A; E }{}.Bar() }\n"
	got, err := reviewRename(t, original, "A.Foo", "Bar")
	if err == nil {
		t.Fatal("rename rebound anonymous struct selector")
	}
	if got != original {
		t.Fatalf("refused rename mutated source:\n%s", got)
	}
}

func TestReviewPackageRenameRejectsUniverseCapture(t *testing.T) {
	original := "package demo\nfunc Size(s string) int { return 0 }\nfunc Use() int { return len(\"ab\") }\n"
	got, err := reviewRename(t, original, "Size", "len")
	if err == nil {
		t.Fatal("package declaration captured universe builtin")
	}
	if got != original {
		t.Fatalf("refused rename mutated source:\n%s", got)
	}
}
