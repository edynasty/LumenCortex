package threadctx

import (
	"fmt"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func cite(path, blob string) taskstate.Cite { return taskstate.Cite{Path: path, Blob: blob} }

// A note gives its paths only while every one of its cites is current and none of its sources is
// withdrawn; a thread left with nothing to locate has no entry.
func TestCatalogKeepsOnlyCurrentUnwithdrawnCites(t *testing.T) {
	notes := []taskstate.Note{
		{ID: "n1", Text: "old", Cites: []taskstate.Cite{cite("a.go", "A"), cite("b.go", "B")}},
		{ID: "n2", Text: "new", Cites: []taskstate.Cite{cite("c.go", "C")}},
		{ID: "n3", Text: "uncited agent text"},
		{ID: "n4", Text: "user worded", By: taskstate.ByUser, From: "ts:note:n9@x", Review: []taskstate.Cite{cite("r.go", "R")}},
	}
	current := map[string]string{"a.go": "A", "b.go": "B", "c.go": "C", "r.go": "R"}
	thread := CatalogThread{ID: "s1", Resolution: taskstate.ResolutionDone, Notes: notes, NoteFacts: map[string]SourceFacts{}}

	got := Catalog([]CatalogThread{thread}, current, 0)
	if len(got) != 1 || fmt.Sprint(got[0].CitedPaths) != "[{c.go C} {a.go A} {b.go B}]" || got[0].Kind != LocatorThread || got[0].Resolution != "done" || got[0].Truncated {
		t.Fatalf("all current = %+v, want c then a, b (newest note first), no review cites", got)
	}
	changed := map[string]string{"a.go": "A", "b.go": "B2", "c.go": "C", "r.go": "R"}
	if got := Catalog([]CatalogThread{thread}, changed, 0); len(got) != 1 || fmt.Sprint(got[0].CitedPaths) != "[{c.go C}]" {
		t.Fatalf("b.go changed = %+v, want n1's paths gone, a.go included", got)
	}
	gone := map[string]string{"a.go": "A", "b.go": "B", "c.go": ""}
	thread.NoteFacts = map[string]SourceFacts{"n1": {Withdrawn: true}}
	if got := Catalog([]CatalogThread{thread}, gone, 0); len(got) != 0 {
		t.Fatalf("n1 withdrawn and c.go gone = %+v, want no entry", got)
	}
}

func TestCatalogBoundsPathsChecksAndEntries(t *testing.T) {
	var cites []taskstate.Cite
	current := map[string]string{}
	for i := 0; i < 3; i++ {
		p := fmt.Sprintf("p%d.go", i)
		cites = append(cites, cite(p, "x"))
		current[p] = "x"
	}
	long := strings.Repeat("d/", MaxLocatorPathBytes/2) + "f.go"
	current[long], current["q, r.go"] = "x", "x"
	notes := []taskstate.Note{
		{ID: "n1", Cites: cites},
		{ID: "n2", Cites: []taskstate.Cite{cite("p9.go", "x"), cite(long, "x"), cite("q, r.go", "x")}},
	}
	current["p9.go"] = "x"
	thread := CatalogThread{ID: "s", Kind: "execution", Resolution: "weird", Notes: notes, CheckIDs: []string{"a", "b", "a", "c", "bad id", "d", "e"}}
	got := Catalog([]CatalogThread{thread}, current, 0)[0]
	if len(got.CitedPaths) != MaxLocatorPaths || got.CitedPaths[0].Path != "p9.go" || !got.Truncated {
		t.Fatalf("paths = %+v truncated=%v, want %d, the long and comma paths left out", got.CitedPaths, got.Truncated, MaxLocatorPaths)
	}
	for _, p := range got.CitedPaths {
		if p.Path == long || strings.Contains(p.Path, ",") {
			t.Fatalf("unrenderable path kept: %q", p.Path)
		}
	}
	if fmt.Sprint(got.Checks) != "[{a unknown} {b unknown} {c unknown} {d unknown}]" {
		t.Fatalf("checks = %+v, want 4 distinct, every status unknown, the spaced id dropped", got.Checks)
	}
	if got.Kind != LocatorExecution || got.Resolution != "" {
		t.Fatalf("kind %q resolution %q, want execution and an unknown resolution dropped", got.Kind, got.Resolution)
	}
	if k := Catalog([]CatalogThread{{ID: "x", Kind: "something new", CheckIDs: []string{"c"}}}, nil, 0)[0].Kind; k != LocatorOther {
		t.Fatalf("unknown kind rendered as %q", k)
	}

	var many []CatalogThread
	for i := 0; i < 20; i++ {
		many = append(many, CatalogThread{ID: fmt.Sprintf("s%02d", i), CheckIDs: []string{"c"}})
	}
	entries := Catalog(many, nil, 0)
	if len(entries) != MaxCatalogEntries || entries[0].Ref != "s00" {
		t.Fatalf("%d entries from 20 threads, want the first %d in order", len(entries), MaxCatalogEntries)
	}
	if len(Catalog(many, nil, 3)) != 3 {
		t.Fatal("limit not applied")
	}
}

func TestRenderCatalogIsBoundedAndCountsWhatDoesNotFit(t *testing.T) {
	if RenderCatalog(nil) != "" {
		t.Fatal("an empty catalog rendered text")
	}
	blob := strings.Repeat("f", 40)
	var entries []LocatorEntry
	for i := 0; i < MaxCatalogEntries; i++ {
		e := LocatorEntry{Ref: fmt.Sprintf("session_%02d", i), Kind: LocatorThread, Resolution: "open", Checks: []LocatorCheck{{ID: "go-test", Status: CheckStatusUnknown}}}
		for j := 0; j < MaxLocatorPaths; j++ {
			e.CitedPaths = append(e.CitedPaths, LocatorPath{Path: fmt.Sprintf("internal/pkg%d/file_%d_with_a_long_name.go", i, j), Blob: blob})
		}
		entries = append(entries, e)
	}
	text := RenderCatalog(entries)
	lines := strings.Split(text, "\n")
	if len(text) > MaxCatalogBytes || lines[0] != CatalogHeader || !strings.HasPrefix(lines[len(lines)-1], "(+") {
		t.Fatalf("%d bytes, %d lines, last %q", len(text), len(lines), lines[len(lines)-1])
	}
	shown := len(lines) - 2
	if want := fmt.Sprintf("(+%d more)", MaxCatalogEntries-shown); lines[len(lines)-1] != want || shown == 0 {
		t.Fatalf("count line %q, want %q", lines[len(lines)-1], want)
	}
	if lines[1] != "- ref=session_00 kind=thread resolution=open paths=internal/pkg0/file_0_with_a_long_name.go@"+blob+
		",internal/pkg0/file_1_with_a_long_name.go@"+blob+",internal/pkg0/file_2_with_a_long_name.go@"+blob+
		",internal/pkg0/file_3_with_a_long_name.go@"+blob+" checks=go-test:unknown" {
		t.Fatalf("line format %q", lines[1])
	}
}
