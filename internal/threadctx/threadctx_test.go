package threadctx

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/taskstate"
)

func TestTokensSplitIdentifiersAndCJK(t *testing.T) {
	got := Tokens("Fix ParseConfig in settings.Load: 修复配置解析 for HTTP2Server")
	for _, want := range []string{"fix", "parseconfig", "parse", "config", "settings", "load", "修复", "复配", "配置", "置解", "解析", "http2server", "http", "server"} {
		if !slices.Contains(got, want) {
			t.Errorf("Tokens lacks %q: %v", want, got)
		}
	}
	for _, unwanted := range []string{"in", "for", "f"} {
		if slices.Contains(got, unwanted) {
			t.Errorf("Tokens kept %q: %v", unwanted, got)
		}
	}
}

func TestRecallRanksByFieldWeightRarityAndRecency(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	threads := []Thread{
		// "retry" everywhere: common, so it barely discriminates.
		{ID: "final-only", Goal: "tidy logging", Final: "Added retry around the flaky upload path.", UpdatedAt: now},
		{ID: "user-note", Goal: "upload work", Notes: []taskstate.Note{{ID: "n1", By: taskstate.ByUser, Text: "Never retry uploads: the backend is not idempotent."}}, UpdatedAt: now.Add(-24 * time.Hour)},
		{ID: "goal", Goal: "Make uploads retry safely", UpdatedAt: now.Add(-48 * time.Hour)},
		{ID: "unrelated", Goal: "rename the CLI flags", Final: "done", UpdatedAt: now},
	}
	hits := Recall("upload retry", threads, now, 8)
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	if want := []string{"user-note", "goal", "final-only"}; !slices.Equal(ids, want) {
		t.Fatalf("order %v, want %v", ids, want)
	}
	if len(hits[0].Notes) != 1 || hits[0].Notes[0].ID != "n1" || hits[0].Notes[0].By != taskstate.ByUser {
		t.Fatalf("matching note not carried: %+v", hits[0].Notes)
	}
	if !strings.Contains(hits[2].Snippet, "retry") {
		t.Fatalf("snippet %q does not show the match", hits[2].Snippet)
	}

	// Same text, different age: the newer thread ranks first.
	aged := []Thread{
		{ID: "old", Goal: "migrate sqlite driver", UpdatedAt: now.Add(-120 * 24 * time.Hour)},
		{ID: "new", Goal: "migrate sqlite driver", UpdatedAt: now.Add(-1 * time.Hour)},
	}
	if hits := Recall("sqlite driver", aged, now, 8); len(hits) != 2 || hits[0].ID != "new" {
		t.Fatalf("recency: %+v", hits)
	}
}

func TestRecallMatchesChineseAndBounds(t *testing.T) {
	now := time.Now()
	var threads []Thread
	for i := 0; i < 20; i++ {
		threads = append(threads, Thread{ID: string(rune('a' + i)), Goal: "修复配置解析的错误", UpdatedAt: now})
	}
	threads = append(threads, Thread{ID: "other", Goal: "更新文档", UpdatedAt: now})
	hits := Recall("配置解析", threads, now, 50)
	if len(hits) != MaxRecallHits {
		t.Fatalf("hits %d, want the bound %d", len(hits), MaxRecallHits)
	}
	for _, h := range hits {
		if h.ID == "other" {
			t.Fatal("unrelated thread matched")
		}
	}
	if Recall("the and of", threads, now, 5) != nil {
		t.Fatal("a query of stop words matched")
	}
}

func TestMarkStaleFlagsChangedAndMissingCites(t *testing.T) {
	n := taskstate.Note{ID: "n1", Text: "x", Cites: []taskstate.Cite{{Path: "a.go", Blob: "aaa"}, {Path: "b.go", Blob: "bbb"}}}
	v := NoteView(n)
	MarkStale(&v, n, map[string]string{"a.go": "aaa", "b.go": "bbb"})
	if v.Stale || v.Cites[0].Stale || v.Cites[1].Stale {
		t.Fatalf("unchanged cites marked stale: %+v", v)
	}
	MarkStale(&v, n, map[string]string{"a.go": "aaa", "b.go": "ccc"})
	if !v.Stale || v.Cites[0].Stale || !v.Cites[1].Stale {
		t.Fatalf("changed cite: %+v", v)
	}
	MarkStale(&v, n, map[string]string{"a.go": "", "b.go": "bbb"})
	if !v.Stale || !v.Cites[0].Stale {
		t.Fatalf("missing cite: %+v", v)
	}
}

// Apply's precedence: withdrawn over a missing source over a changed one; an uncited agent note (or the
// user's rewrite of one) is unverified, an uncited user note unknown. A withdrawn note shows nothing to
// any reader; a note whose source is gone loses its text for an agent and keeps it, labelled, for a host.
func TestApplyDecidesApplicabilityAndWithholdsText(t *testing.T) {
	cited := taskstate.Note{ID: "n1", Text: "cited", By: taskstate.ByAgent, Cites: []taskstate.Cite{{Path: "a.go", Blob: "aaa"}}}
	agent := taskstate.Note{ID: "n2", Text: "agent says", By: taskstate.ByAgent}
	user := taskstate.Note{ID: "u1", Text: "user decided", By: taskstate.ByUser}
	rewrite := taskstate.Note{ID: "n3", Text: "user worded", By: taskstate.ByUser, From: "ts:note:n2@x"}
	ok := SourceFacts{Origin: "agent_generated", LineageComplete: true}
	for _, c := range []struct {
		name     string
		note     taskstate.Note
		current  map[string]string
		facts    SourceFacts
		agent    bool
		want     string
		keepText bool
	}{
		{"unchanged cite", cited, map[string]string{"a.go": "aaa"}, ok, true, ApplicabilityChecked, true},
		{"changed cite", cited, map[string]string{"a.go": "bbb"}, ok, true, ApplicabilityUnknown, true},
		{"gone cite, agent", cited, map[string]string{"a.go": ""}, ok, true, ApplicabilityMissing, false},
		{"gone cite, host", cited, map[string]string{"a.go": ""}, ok, false, ApplicabilityMissing, true},
		{"withdrawn over gone", cited, map[string]string{"a.go": ""}, SourceFacts{Withdrawn: true}, true, ApplicabilityWithdrawn, false},
		{"withdrawn, host", cited, map[string]string{"a.go": "aaa"}, SourceFacts{Withdrawn: true, LineageComplete: true}, false, ApplicabilityWithdrawn, false},
		{"uncited agent", agent, nil, ok, true, ApplicabilityUnverified, true},
		{"uncited user", user, nil, SourceFacts{}, true, ApplicabilityUnknown, true},
		{"user rewrite of agent text", rewrite, nil, ok, true, ApplicabilityUnverified, true},
	} {
		v := NoteView(c.note)
		Apply(&v, c.note, c.current, c.facts, c.agent)
		if v.Applicability != c.want || (v.Text != "") != c.keepText {
			t.Errorf("%s: applicability %q text %q, want %q keep=%v", c.name, v.Applicability, v.Text, c.want, c.keepText)
		}
		if c.want == ApplicabilityWithdrawn && (len(v.Cites) != 0 || v.Stale) {
			t.Errorf("%s: withdrawn note keeps cites %+v", c.name, v.Cites)
		}
		if v.LineageIncomplete == c.facts.LineageComplete {
			t.Errorf("%s: lineageIncomplete %v for facts %+v", c.name, v.LineageIncomplete, c.facts)
		}
	}
}

func TestSnippetIsBoundedAtRuneBoundaries(t *testing.T) {
	text := strings.Repeat("前言", 200) + " target word " + strings.Repeat("后记", 200)
	s := snippet(text, []string{"target"})
	if len(s) > MaxSnippetBytes+len("…") || !strings.Contains(s, "target") {
		t.Fatalf("snippet %d bytes %q", len(s), s)
	}
	if !strings.HasPrefix(s, "…") {
		t.Fatalf("a cut start is not marked: %q", s[:10])
	}
}
