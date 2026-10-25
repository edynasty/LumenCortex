package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/threadctx"
	"github.com/edynasty/LumenCortex/protocol"
)

// Plan T2-S2 through the Engine and SQLite: the locator catalog carries ids, enums, current path@blob
// pairs and configured check ids only. The free text a thread holds - its goal, notes, final answer, a
// user reference's note, its check's log - never enters it; a note whose cited file changes stops
// contributing; more than four paths are cut and marked; a fenced thread and a check the
// configuration no longer defines are gone.
func TestLocatorCatalogCarriesNoFreeTextAndFollowsSources(t *testing.T) {
	const sentinel = "CATALOG-SENTINEL-8c1"
	useChecks(t, `{"status": {"kind": "test", "command": "echo `+sentinel+`-log; true"}}`)
	engine, root := gitEngine(t)
	ctx := context.Background()
	for _, f := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("package x // "+f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "Fix the uploader retry " + sentinel + "-goal"})
	if err != nil {
		t.Fatal(err)
	}
	p := &threadProvider{responses: []protocol.ProviderResponse{
		toolCalls(
			tc("n1", "note_write", `{"text":"`+sentinel+`-note1 the retry lives in a and b","cites":["a.go","b.go"]}`),
		),
		toolCalls(
			tc("n2", "note_write", `{"text":"`+sentinel+`-note2 backoff is in c, d, e","cites":["c.go","d.go","e.go"]}`),
			tc("n3", "note_write", `{"text":"`+sentinel+`-note3 no cite at all"}`),
		),
		{Message: protocol.Message{Role: "assistant", Content: sentinel + "-final"}, FinishReason: "stop"},
	}}
	if r, err := engine.runAgentWith(ctx, handle.ID, p, AgentOptions{Input: "go", MaxSteps: 5}); err != nil || r.Status != "completed" {
		t.Fatalf("thread A %+v %v", r, err)
	}
	refNote := sentinel + "-reference"
	if _, err := engine.EditTaskState(ctx, handle.ID, TaskStateEdit{Reference: &taskstate.ReferenceEdit{Kind: taskstate.RefPath, Target: "f.go", Note: &refNote, State: taskstate.RefIncluded}}); err != nil {
		t.Fatal(err)
	}
	if rows, err := engine.CheckResults(ctx, handle.ID, -1, 0); err != nil || len(rows) == 0 {
		t.Fatalf("thread A ran no check: %v %v", rows, err)
	}

	catalog := func() LocatorCatalog {
		t.Helper()
		c, err := engine.LocatorCatalog(ctx, "uploader retry", "")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(c)
		if strings.Contains(string(raw), sentinel) {
			t.Fatalf("free text reached the catalog: %s", raw)
		}
		assertLocatorLines(t, c.Text, handle.ID)
		return c
	}
	paths := func(e threadctx.LocatorEntry) string {
		var out []string
		for _, p := range e.CitedPaths {
			out = append(out, p.Path)
		}
		return strings.Join(out, ",")
	}

	c := catalog()
	if len(c.Entries) != 1 || c.Entries[0].Ref != handle.ID || c.Entries[0].Kind != threadctx.LocatorThread {
		t.Fatalf("entries %+v, want thread A", c.Entries)
	}
	a := c.Entries[0]
	if paths(a) != "c.go,d.go,e.go,a.go" || !a.Truncated || len(a.Checks) != 1 || a.Checks[0] != (threadctx.LocatorCheck{ID: "status", Status: threadctx.CheckStatusUnknown}) {
		t.Fatalf("entry %+v, want newest note's paths first, cut at four and marked, the check unknown", a)
	}

	// a.go changes: note 1 no longer locates anything; b.go no longer needs cutting.
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package x // changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a = catalog().Entries[0]; paths(a) != "c.go,d.go,e.go" || a.Truncated {
		t.Fatalf("after a.go changed: %+v", a)
	}
	// c.go is removed: note 2 is gone too; only the check still locates the thread.
	if err := os.Remove(filepath.Join(root, "c.go")); err != nil {
		t.Fatal(err)
	}
	if a = catalog().Entries[0]; len(a.CitedPaths) != 0 || len(a.Checks) != 1 {
		t.Fatalf("after c.go removed: %+v", a)
	}

	// The configuration stops defining the check: the thread has nothing left to locate.
	useGlobalConfig(t, boundCatalog("test/b0"))
	if c = catalog(); len(c.Entries) != 0 || c.Text != "" {
		t.Fatalf("with nothing to locate: %+v", c)
	}

	// A fenced thread is never a candidate, whatever it could locate.
	useChecks(t, `{"status": {"kind": "test", "command": "true"}}`)
	if len(catalog().Entries) != 1 {
		t.Fatal("the check entry did not come back with the configuration")
	}
	fenceSession(t, engine, handle.ID)
	if c = catalog(); len(c.Entries) != 0 {
		t.Fatalf("fenced thread in the catalog: %+v", c)
	}
}

// assertLocatorLines checks every rendered line against the catalog's grammar: nothing but the header,
// ids, fixed enums, path@blob pairs, check:status pairs and the truncation mark.
func assertLocatorLines(t *testing.T, text string, refs ...string) {
	t.Helper()
	if text == "" {
		return
	}
	lines := strings.Split(text, "\n")
	if lines[0] != threadctx.CatalogHeader {
		t.Fatalf("header %q", lines[0])
	}
	line := regexp.MustCompile(`^- ref=(\S+) kind=(thread|subagent|execution|other)( resolution=(open|done|dropped))?` +
		`( paths=[^,@\s]+@[0-9a-f]{40,64}(,[^,@\s]+@[0-9a-f]{40,64})*)?( checks=[^,:\s]+:unknown(,[^,:\s]+:unknown)*)?( truncated)?$`)
	allowed := map[string]bool{}
	for _, r := range refs {
		allowed[r] = true
	}
	for _, l := range lines[1:] {
		m := line.FindStringSubmatch(l)
		if m == nil && !regexp.MustCompile(`^\(\+\d+ more\)$`).MatchString(l) {
			t.Fatalf("catalog line outside the grammar: %q", l)
		}
		if m != nil && !allowed[m[1]] {
			t.Fatalf("catalog ref %q is not a known thread", m[1])
		}
	}
}
