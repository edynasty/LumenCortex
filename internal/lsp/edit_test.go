package lsp

import (
	"strings"
	"testing"
)

func TestApplyTextEditsUsesUTF16Positions(t *testing.T) {
	// The first line holds an emoji: one code point, two UTF-16 code units, four UTF-8 bytes.
	source := "a😀b := 1\nc := 2\n"
	// Character 3 is the "b" after the emoji: a byte offset would land inside it, a code point
	// count would land on the space.
	next, err := ApplyTextEdits(source, []TextEdit{
		{Range: Range{Start: Position{Line: 0, Character: 3}, End: Position{Line: 0, Character: 4}}, NewText: "B"},
		{Range: Range{Start: Position{Line: 1, Character: 0}, End: Position{Line: 1, Character: 1}}, NewText: "d"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if next != "a😀B := 1\nd := 2\n" {
		t.Fatalf("next=%q", next)
	}
}

func TestApplyTextEditsRefusesBadEdits(t *testing.T) {
	source := "abc\ndef\n"
	cases := map[string]TextEdit{
		"line past the end":     {Range: Range{Start: Position{Line: 5}, End: Position{Line: 5}}, NewText: "x"},
		"character past line":   {Range: Range{Start: Position{Line: 0, Character: 9}, End: Position{Line: 0, Character: 9}}, NewText: "x"},
		"end before start":      {Range: Range{Start: Position{Line: 1}, End: Position{Line: 0}}, NewText: "x"},
		"negative character":    {Range: Range{Start: Position{Line: 0, Character: -1}}, NewText: "x"},
		"start past end of all": {Range: Range{Start: Position{Line: 9, Character: 0}}, NewText: "x"},
	}
	for name, edit := range cases {
		if _, err := ApplyTextEdits(source, []TextEdit{edit}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ApplyTextEdits(source, []TextEdit{
		{Range: Range{Start: Position{Line: 0, Character: 0}, End: Position{Line: 0, Character: 3}}, NewText: "x"},
		{Range: Range{Start: Position{Line: 0, Character: 2}, End: Position{Line: 0, Character: 3}}, NewText: "y"},
	}); err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("overlapping edits: %v", err)
	}
}

func TestWorkspaceEditFileEdits(t *testing.T) {
	edit := WorkspaceEdit{
		Changes: map[string][]TextEdit{
			"file:///ws/b.go": {{Range: Range{Start: Position{Line: 0}, End: Position{Line: 0, Character: 1}}, NewText: "B"}},
		},
		DocumentChanges: []DocumentChange{
			{TextDocument: &VersionedTextDocumentIdentifier{URI: "file:///ws/a.go"},
				Edits: []TextEdit{{Range: Range{Start: Position{Line: 0}, End: Position{Line: 0, Character: 1}}, NewText: "A"}}},
			{TextDocument: &VersionedTextDocumentIdentifier{URI: "file:///ws/a.go"},
				Edits: []TextEdit{{Range: Range{Start: Position{Line: 1}, End: Position{Line: 1, Character: 1}}, NewText: "C"}}},
		},
	}
	files, err := edit.FileEdits()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "/ws/a.go" || len(files[0].Edits) != 2 || files[1].Path != "/ws/b.go" {
		t.Fatalf("files=%+v", files)
	}

	resource := WorkspaceEdit{DocumentChanges: []DocumentChange{{Kind: "rename"}}}
	if _, err := resource.FileEdits(); err == nil || !strings.Contains(err.Error(), "resource operation") {
		t.Fatalf("resource operation: %v", err)
	}

	remote := WorkspaceEdit{Changes: map[string][]TextEdit{"untitled:Untitled-1": nil}}
	if _, err := remote.FileEdits(); err == nil || !strings.Contains(err.Error(), "not a file path") {
		t.Fatalf("non-file URI: %v", err)
	}
}
