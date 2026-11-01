package notebook

import (
	"strings"
	"testing"
)

func TestParseSplitsSectionsOutsideFences(t *testing.T) {
	body := "# Plan\n\nintro line\n\n" +
		"## Checklist\n\n- [ ] one\n- [x] two\n\n" +
		"## Findings\n\n```md\n## not a heading\n- [ ] not an item\n```\n\n" +
		"## Empty\n\n" +
		"### Deeper\n\nstill Findings' neighbour\n"
	doc := Parse(body)
	if doc.Preamble != "# Plan\n\nintro line\n\n" {
		t.Fatalf("preamble = %q", doc.Preamble)
	}
	var titles []string
	for _, s := range doc.Sections {
		titles = append(titles, s.Title)
	}
	want := []string{"Checklist", "Findings", "Empty"}
	if strings.Join(titles, ",") != strings.Join(want, ",") {
		t.Fatalf("sections = %v, want %v", titles, want)
	}
	if got := doc.Sections[2].Body; got != "\n### Deeper\n\nstill Findings' neighbour\n" {
		t.Fatalf("Empty body = %q", got)
	}
	if got := doc.Render(); got != body {
		t.Fatalf("round trip changed the body:\n%q\n%q", got, body)
	}
	if got := Headings(body); strings.Join(got, ",") != "Checklist,Findings,Empty" {
		t.Fatalf("headings = %v", got)
	}
}

func TestParseHandlesCRLFAndNoPreamble(t *testing.T) {
	doc := Parse("## A\r\none\r\n## B\r\ntwo\r\n")
	if doc.Preamble != "" {
		t.Fatalf("preamble = %q", doc.Preamble)
	}
	if len(doc.Sections) != 2 || doc.Sections[0].Body != "one\n" || doc.Sections[1].Body != "two\n" {
		t.Fatalf("sections = %#v", doc.Sections)
	}
	if got := doc.Render(); got != "## A\none\n## B\ntwo\n" {
		t.Fatalf("render = %q", got)
	}
}

func TestSectionReadsTheFirstDuplicate(t *testing.T) {
	body := "## A\nfirst\n## A\nsecond\n"
	if text, ok := SectionText(body, "A"); !ok || text != "first" {
		t.Fatalf("section A = %q, %v; want the first one", text, ok)
	}
	if _, ok := SectionText(body, "Missing"); ok {
		t.Fatal("missing section reported present")
	}
	if text, ok := SectionText("## Empty\n", "Empty"); !ok || text != "" {
		t.Fatalf("empty section = %q, %v", text, ok)
	}
}

func TestUpsertSectionReplacesOrAppends(t *testing.T) {
	body := "# Plan\n\n## Plan\n\nold plan\n\n## Handoff\n\ndone\n"
	cases := []struct {
		name    string
		body    string
		section string
		content string
		want    string
	}{
		{"replace middle", body, "Plan", "new plan", "# Plan\n\n## Plan\n\nnew plan\n\n## Handoff\n\ndone\n"},
		{"replace last", body, "Handoff", "next", "# Plan\n\n## Plan\n\nold plan\n\n## Handoff\n\nnext\n"},
		{"append missing", body, "Findings", "found it", "# Plan\n\n## Plan\n\nold plan\n\n## Handoff\n\ndone\n\n## Findings\n\nfound it\n"},
		{"clear section", body, "Plan", "", "# Plan\n\n## Plan\n\n## Handoff\n\ndone\n"},
		{"no sections yet", "# Notes\n", "Plan", "step one", "# Notes\n\n## Plan\n\nstep one\n"},
		{"empty body", "", "Plan", "step one", "## Plan\n\nstep one\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := UpsertSection(c.body, c.section, c.content); got != c.want {
				t.Fatalf("UpsertSection = %q, want %q", got, c.want)
			}
		})
	}
}

func TestAppendKeepsOneBlankLine(t *testing.T) {
	cases := []struct{ body, content, want string }{
		{"", "one", "one\n"},
		{"# T\n", "one", "# T\n\none\n"},
		{"# T\n\n## A\nx\n", "one\n", "# T\n\n## A\nx\n\none\n"},
		{"# T", "one", "# T\n\none\n"},
		{"# T\n", "  \n", "# T\n"},
	}
	for _, c := range cases {
		if got := Append(c.body, c.content); got != c.want {
			t.Fatalf("Append(%q, %q) = %q, want %q", c.body, c.content, got, c.want)
		}
	}
}

func TestChecklistExtractsItemsWithTheirSection(t *testing.T) {
	body := "# Plan\n\n- [ ] preamble item\n\n## Checklist\n\n- [ ] first\n* [x] second\n  + [X] third\n" +
		"```\n- [ ] fenced\n```\n## Verification\n- [ ] run tests\n- not an item\n"
	items := Checklist(body)
	want := []Item{
		{Section: "", Done: false, Text: "preamble item"},
		{Section: "Checklist", Done: false, Text: "first"},
		{Section: "Checklist", Done: true, Text: "second"},
		{Section: "Checklist", Done: true, Text: "third"},
		{Section: "Verification", Done: false, Text: "run tests"},
	}
	if len(items) != len(want) {
		t.Fatalf("items = %#v", items)
	}
	for i := range want {
		if items[i] != want[i] {
			t.Fatalf("item %d = %#v, want %#v", i, items[i], want[i])
		}
	}
}

func TestHeadingRequiresTwoHashesAndSpace(t *testing.T) {
	body := "# Title\n##A\n##\n##\tTabbed\n## Ok\n#### Deep\n"
	if got := Headings(body); len(got) != 1 || got[0] != "Ok" {
		t.Fatalf("headings = %v", got)
	}
}
