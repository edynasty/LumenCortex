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
