package taskstate

import (
	"strings"
	"testing"
	"time"
)

// A user rewrite of an agent note stays the user's wording of agent text: it names the version it
// replaced, keeps the agent's cites as sources to review, and the Task State block marks it as needing
// review instead of presenting it as a [user] note. Rewriting it again chains to the previous rewrite and
// keeps the same review sources; a note the user created has neither.
func TestUserRewriteOfAgentTextNeedsReview(t *testing.T) {
	var s State
	at := time.Unix(0, 0).UTC()
	cites := []Cite{{Path: "cache.go", Blob: "b1"}}
	agent, err := s.WriteNote("", "uploads are not idempotent.", cites, at)
	if err != nil {
		t.Fatal(err)
	}
	original := EntryRef(EntryNote, agent.ID, agent)
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Note: &NoteEdit{ID: agent.ID, Text: "uploads are not idempotent"}}}})
	n := s.Notes[0]
	if n.By != ByUser || n.From != original || len(n.Cites) != 0 || len(n.Review) != 1 || n.Review[0] != cites[0] || !n.NeedsReview() {
		t.Fatalf("rewrite %+v, want By user, From %s, review cache.go", n, original)
	}
	block := Render("goal", s, nil)
	if !strings.Contains(block, "- n1 [user edit of agent text, needs review]: uploads are not idempotent (review cache.go)") ||
		strings.Contains(block, "- n1 [user]") {
		t.Fatalf("block does not mark the rewrite:\n%s", block)
	}

	first := EntryRef(EntryNote, n.ID, n)
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Note: &NoteEdit{ID: agent.ID, Text: "uploads are never idempotent"}}}})
	if again := s.Notes[0]; again.From != first || len(again.Review) != 1 || again.Review[0] != cites[0] {
		t.Fatalf("second rewrite %+v, want From the first rewrite and the same review sources", again)
	}

	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Note: &NoteEdit{ID: "u1", Text: "my own fact", Create: true}}}})
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Note: &NoteEdit{ID: "u1", Text: "my own fact, corrected"}}}})
	if own := s.Notes[1]; own.NeedsReview() || len(own.Review) != 0 {
		t.Fatalf("a user note edited by the user %+v needs review", own)
	}
	if block := Render("goal", s, nil); !strings.Contains(block, "- u1 [user]: my own fact, corrected") {
		t.Fatalf("user note not shown as the user's:\n%s", block)
	}
}

// The user taking over an agent suggestion (include, exclude, new note) records the version it replaced;
// including the user's own reference does not.
func TestUserTakeOverOfASuggestionNamesIt(t *testing.T) {
	var s State
	at := time.Unix(0, 0).UTC()
	sug, _, err := s.Suggest(RefPath, "docs/a.md", "explains the retry policy", at)
	if err != nil {
		t.Fatal(err)
	}
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Reference: &ReferenceEdit{Kind: RefPath, Target: "docs/a.md", State: RefIncluded}}}})
	r := s.References[0]
	if r.By != ByUser || r.From != EntryRef(EntryReference, sug.ID, sug) || r.Note != "explains the retry policy" {
		t.Fatalf("take-over %+v", r)
	}
	if block := Render("goal", s, nil); !strings.Contains(block, "explains the retry policy (taken over from an agent suggestion: needs review)") {
		t.Fatalf("block does not mark the take-over:\n%s", block)
	}
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Reference: &ReferenceEdit{Kind: RefPath, Target: "docs/b.md", State: RefIncluded}}}})
	s.Apply([]QueuedEdit{{At: at, Edit: Edit{Reference: &ReferenceEdit{Kind: RefPath, Target: "docs/b.md", State: RefExcluded}}}})
	if own := s.References[1]; own.NeedsReview() {
		t.Fatalf("the user's own reference %+v needs review", own)
	}
}

// ChangedEntries lists new and changed entry versions, not removed or unchanged ones; StartedChildren
// tells a task a run started from one the user started from a proposal; Deliveries lists changed marks.
func TestEntryDiffs(t *testing.T) {
	at := time.Unix(0, 0).UTC()
	var before State
	kept, _ := before.WriteNote("", "kept", nil, at)
	gone, _ := before.WriteNote("", "gone", nil, at)
	before.Tasks = []Task{{ID: "t1", Objective: "proposed", State: TaskProposed}}
	before.TaskResults = map[string]TaskMark{"c0": {Seq: 3, Status: "completed"}}

	after := before.Clone()
	if err := after.RemoveNote(gone.ID); err != nil {
		t.Fatal(err)
	}
	added, _ := after.WriteNote("", "added", nil, at)
	if err := after.StartTask("t1", "child1", at); err != nil {
		t.Fatal(err)
	}
	after.Tasks = append(after.Tasks, Task{ID: "t2", Objective: "direct", State: TaskStarted, SessionID: "child2"})
	after.MarkDelivered("c0", TaskMark{Seq: 9, Status: "completed"})
	after.MarkDelivered("c1", TaskMark{Seq: 2, Status: "failed"})

	changed := map[string]bool{}
	for _, e := range ChangedEntries(before, after) {
		changed[e.Ref] = true
	}
	if !changed[EntryRef(EntryNote, added.ID, after.Notes[1])] || changed[EntryRef(EntryNote, kept.ID, kept)] || len(changed) != 3 {
		t.Fatalf("changed %v, want the added note and the two tasks", changed)
	}
	children := StartedChildren(before, after)
	if len(children) != 2 || children[0].SessionID != "child1" || children[0].Proposal != EntryRef(EntryTask, "t1", before.Tasks[0]) ||
		children[1].SessionID != "child2" || children[1].Proposal != "" {
		t.Fatalf("children %+v", children)
	}
	if again := StartedChildren(after, after); len(again) != 0 {
		t.Fatalf("children of an unchanged state %+v", again)
	}
	d := Deliveries(before, after)
	if len(d) != 2 || d[0] != (Delivery{SessionID: "c0", Seq: 9}) || d[1] != (Delivery{SessionID: "c1", Seq: 2}) {
		t.Fatalf("deliveries %+v", d)
	}
}
