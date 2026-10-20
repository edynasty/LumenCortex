package taskstate

import (
	"strings"
	"testing"
	"time"
)

// ADR-0010 C6: the notes take at most their sub-budget of the block, newest first, and the rest are
// counted; the block as a whole stays within MaxBlockBytes.
func TestRenderNotesWithinTheirBudget(t *testing.T) {
	var s State
	at := time.Unix(0, 0).UTC()
	for i := 0; i < MaxNotes; i++ {
		if _, err := s.WriteNote("", strings.Repeat("x", 200)+" #"+strings.Repeat("y", i%3), nil, at); err != nil {
			t.Fatal(err)
		}
	}
	block := Render(strings.Repeat("g", MaxBriefBytes), s, nil)
	if len(block) > MaxBlockBytes {
		t.Fatalf("block %d bytes > %d", len(block), MaxBlockBytes)
	}
	start := strings.Index(block, "\nNotes (")
	if start < 0 {
		t.Fatal("no notes section")
	}
	section := block[start:]
	if len(section) > notesRenderBytes+512 {
		t.Fatalf("notes section %d bytes", len(section))
	}
	if !strings.Contains(section, "- n32 [agent]") || strings.Contains(section, "- n1 [agent]") {
		t.Fatalf("not newest first:\n%s", section)
	}
	if !strings.Contains(section, "older notes not shown") {
		t.Fatalf("hidden notes not counted:\n%s", section)
	}
}

// A run cannot grow past MaxNotes; the user can, by displacing the oldest agent note, and a user edit
// rewrites any note as the user's.
func TestNoteLimitsAndUserEdits(t *testing.T) {
	var s State
	at := time.Unix(0, 0).UTC()
	for i := 0; i < MaxNotes; i++ {
		if _, err := s.WriteNote("", "agent note", nil, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.WriteNote("", "one more", nil, at); err == nil || !strings.Contains(err.Error(), "n1") {
		t.Fatalf("full thread: %v, want a refusal naming the oldest agent note", err)
	}
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Note: &NoteEdit{Text: "user fact"}}}})
	if len(s.Notes) != MaxNotes || s.Notes[0].ID != "n2" || s.Notes[MaxNotes-1].By != ByUser || s.Notes[MaxNotes-1].ID != "n33" {
		t.Fatalf("user add on a full thread: first=%+v last=%+v", s.Notes[0], s.Notes[MaxNotes-1])
	}
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Note: &NoteEdit{ID: "n2", Text: "corrected by the user"}}}})
	if s.Notes[0].By != ByUser || s.Notes[0].Text != "corrected by the user" {
		t.Fatalf("user rewrite: %+v", s.Notes[0])
	}
	if err := s.RemoveNote("n2"); err != ErrUserNote {
		t.Fatalf("run removing a user note: %v", err)
	}
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Note: &NoteEdit{ID: "n2", Remove: true}}}})
	if s.noteIndex("n2") >= 0 {
		t.Fatal("user removal did not apply")
	}
	for _, bad := range []NoteEdit{{Text: ""}, {Text: strings.Repeat("z", MaxNoteTextBytes+1)}, {ID: "n1", Remove: true, Text: "x"}, {Remove: true}} {
		if err := (Edit{Note: &bad}).Validate(); err == nil {
			t.Fatalf("edit %+v validated", bad)
		}
	}
}

func TestNormalizeCitePaths(t *testing.T) {
	got, err := NormalizeCitePaths([]string{"./a/b.go", "a/b.go", "c\\d.go"})
	if err != nil || len(got) != 2 || got[0] != "a/b.go" || got[1] != "c/d.go" {
		t.Fatalf("%v %v", got, err)
	}
	for _, bad := range [][]string{{"../x"}, {"/etc/passwd"}, {"."}, {"a", "b", "c", "d", "e"}} {
		if _, err := NormalizeCitePaths(bad); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}
