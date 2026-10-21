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

// A user add queued with its id keeps that id, whatever a run writes before the inbox is applied: a
// removal by that id removes the user's note, never the run's newer one, and a replayed add is not
// applied twice.
func TestQueuedUserNoteIDIsStable(t *testing.T) {
	var s State
	at := time.Unix(0, 0).UTC()
	for i := 0; i < 3; i++ {
		if _, err := s.WriteNote("", "earlier", nil, at); err != nil {
			t.Fatal(err)
		}
	}
	add := QueuedEdit{At: at, Edit: Edit{Note: &NoteEdit{ID: "u1a2b", Text: "user fact", Create: true}}}
	remove := QueuedEdit{At: at, Edit: Edit{Note: &NoteEdit{ID: "u1a2b", Remove: true}}}
	// The run writes before the queued add and removal are applied.
	agent, err := s.WriteNote("", "agent fact", nil, at)
	if err != nil {
		t.Fatal(err)
	}
	s.Apply([]QueuedEdit{add, add})
	if s.noteIndex("u1a2b") < 0 || len(s.Notes) != 5 {
		t.Fatalf("after add (applied twice): %+v", s.Notes)
	}
	s.Apply([]QueuedEdit{remove})
	if s.noteIndex("u1a2b") >= 0 || s.noteIndex(agent.ID) < 0 {
		t.Fatalf("removal hit the wrong note: %+v", s.Notes)
	}
	for _, bad := range []NoteEdit{{ID: "n9", Text: "x", Create: true}, {ID: "u", Text: "x", Create: true}, {ID: "u-1", Text: "x", Create: true}, {ID: "u1", Remove: true, Create: true}} {
		if err := (Edit{Note: &bad}).Validate(); err == nil {
			t.Fatalf("edit %+v validated", bad)
		}
	}
}

// The newest note is shown even when it cites four maximal paths; only older notes are counted.
func TestRenderShowsTheNewestNoteWithLongCites(t *testing.T) {
	var s State
	at := time.Unix(0, 0).UTC()
	if _, err := s.WriteNote("", "older", nil, at); err != nil {
		t.Fatal(err)
	}
	cites := make([]Cite, 4)
	for i := range cites {
		cites[i] = Cite{Path: strings.Repeat(string(rune('a'+i)), 1000) + ".go"}
	}
	if _, err := s.WriteNote("", strings.Repeat("n", MaxNoteTextBytes), cites, at); err != nil {
		t.Fatal(err)
	}
	block := Render("goal", s, nil)
	if !strings.Contains(block, "- n2 [agent]") {
		t.Fatalf("newest note not shown:\n%s", block)
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
