package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/threadctx"
	"github.com/edynasty/LumenCortex/protocol"
)

// Plan T2-S1 (ROADMAP R1a-1): bounded readback of whole tool groups, recall past the newest 500
// sessions, and each note's applicability, through the Engine and the SQLite store.

// pagingProvider reads a thread's transcript back page by page with read_thread, following each
// page's nextBefore until there is none, then stops.
type pagingProvider struct {
	mu     sync.Mutex
	target string
	pages  []threadTranscript
	calls  int
	seen   []protocol.ProviderRequest
}

func (p *pagingProvider) requests() []protocol.ProviderRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen
}

func (p *pagingProvider) Model() string { return "paging" }

func (p *pagingProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, req)
	resp := toolCalls(tc("p0", "read_thread", `{"id":"`+p.target+`","transcript":true}`))
	if p.calls > 0 {
		var view ThreadView
		out := lastToolResult(req, fmt.Sprintf("p%d", p.calls-1))
		if err := json.Unmarshal([]byte(out), &view); err != nil || view.Transcript == nil {
			return protocol.ProviderResponse{}, fmt.Errorf("page %d: %q: %v", p.calls-1, out, err)
		}
		p.pages = append(p.pages, *view.Transcript)
		if view.Transcript.NextBefore < 0 {
			resp = protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "read it all"}, FinishReason: "stop"}
		} else {
			resp = toolCalls(tc(fmt.Sprintf("p%d", p.calls), "read_thread", fmt.Sprintf(`{"id":%q,"before":%d}`, p.target, view.Transcript.NextBefore)))
		}
	}
	p.calls++
	resp.Echo = protocol.NewEcho("reasoning_content", []byte(echoSentinel+"-paging"), 12)
	return resp, nil
}

// A thread of 25 tool turns that wrote no note is read back whole by a later thread: every message
// once, no page splitting a tool call from its result, the one group larger than a page returned
// oversize with its location, and no reasoning echo anywhere.
func TestReadThreadPagesATwentyFiveTurnTranscriptByToolGroup(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, root := gitEngine(t)
	if err := os.WriteFile(filepath.Join(root, "small.go"), []byte("package small\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(strings.Repeat("0123456789abcdef\n", 400)), 0o644); err != nil {
		t.Fatal(err)
	}
	const turns, oversizeTurn = 25, 12
	var responses []protocol.ProviderResponse
	for i := 0; i < turns; i++ {
		var calls []protocol.ToolCall
		if i == oversizeTurn {
			// The loop's per-step cap; eight results of ~5 KiB are one group over a 32 KiB page.
			for j := 0; j < 8; j++ {
				calls = append(calls, tc(fmt.Sprintf("big%d", j), "read_file", `{"path":"big.txt"}`))
			}
		} else {
			calls = []protocol.ToolCall{tc(fmt.Sprintf("a%d", i), "read_file", `{"path":"small.go"}`), tc(fmt.Sprintf("b%d", i), "read_file", `{"path":"small.go"}`)}
		}
		r := toolCalls(calls...)
		r.Message.Content = fmt.Sprintf("turn %d", i)
		if i == 0 {
			r.Message.Content = "Decision: the upload backoff caps at 30s; never retry a 409."
		}
		responses = append(responses, r)
	}
	responses = append(responses, protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"})
	a := &threadProvider{responses: responses}
	// Model c: a 64k context holds the whole transcript, so nothing is compacted away before it is read.
	ref := bindRef(engine, "c", a)
	threadA := newTestSession(t, engine, "Tune upload backoff")
	if r, err := engine.RunAgent(context.Background(), threadA.ID, AgentOptions{ModelRef: ref, MaxSteps: turns + 2}); r.Status != "completed" {
		t.Fatalf("thread A %+v: %v", r, err)
	}
	ctx := context.Background()
	info, err := engine.TaskState(ctx, threadA.ID)
	if err != nil || len(info.State.Notes) != 0 {
		t.Fatalf("thread A notes %+v %v, want none", info.State.Notes, err)
	}
	stored, err := engine.store.MessagesBefore(ctx, threadA.ID, -1, 500)
	if err != nil {
		t.Fatal(err)
	}

	b := &pagingProvider{target: threadA.ID}
	ref = bindRef(engine, "c", b)
	threadB := newTestSession(t, engine, "What did the backoff thread decide?")
	if r, err := engine.RunAgent(context.Background(), threadB.ID, AgentOptions{ModelRef: ref, MaxSteps: 20}); r.Status != "completed" {
		t.Fatalf("thread B %+v: %v", r, err)
	}
	if len(b.pages) < 3 {
		t.Fatalf("%d pages, want the transcript split over several", len(b.pages))
	}
	seen := map[int64]int{}
	decision, oversize := false, 0
	for i, page := range b.pages {
		calls, results := map[string]bool{}, map[string]bool{}
		size := 0
		for _, m := range page.Messages {
			seen[m.Seq]++
			size += len(m.Content)
			for _, c := range m.ToolCalls {
				calls[c.ID] = true
				size += len(c.Args)
			}
			if m.Role == "tool" {
				results[m.ToolCallID] = true
			}
			decision = decision || strings.Contains(m.Content, "never retry a 409")
		}
		for id := range calls {
			if !results[id] {
				t.Fatalf("page %d holds call %s without its result", i, id)
			}
		}
		for id := range results {
			if !calls[id] {
				t.Fatalf("page %d holds the result of %s without its call", i, id)
			}
		}
		if len(page.Messages) > transcriptMaxMessages || size > transcriptMaxBytes {
			t.Fatalf("page %d: %d messages, %d bytes", i, len(page.Messages), size)
		}
		if page.Oversize {
			oversize++
			if page.Complete || page.GroupLast-page.GroupFirst != 8 || page.NextBefore != page.GroupFirst {
				t.Fatalf("oversize page %+v", page)
			}
		}
		if i > 0 && page.Messages[len(page.Messages)-1].Seq >= b.pages[i-1].Messages[0].Seq {
			t.Fatalf("page %d is not older than page %d", i, i-1)
		}
	}
	if oversize != 1 {
		t.Fatalf("%d oversize pages, want the 8-read group alone", oversize)
	}
	if !decision {
		t.Fatal("the first turn's decision was not read back")
	}
	if len(seen) != len(stored) {
		t.Fatalf("read back %d distinct messages of %d", len(seen), len(stored))
	}
	for _, m := range stored {
		if seen[m.Seq] != 1 {
			t.Fatalf("message %d read %d times", m.Seq, seen[m.Seq])
		}
	}
	for _, req := range b.requests() {
		for _, m := range req.Messages {
			if strings.Contains(m.Content, echoSentinel) {
				t.Fatal("a transcript page holds the reasoning echo")
			}
		}
	}
	path := engine.store.Path()
	engine.Close()
	assertNoSentinelAtRest(t, path)
}

// A child task older than the newest 500 sessions is still found by what it is about, with its
// parent; a fenced session among the matches is not.
func TestRecallFindsAChildTaskPastTheNewestFiveHundred(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, _ := gitEngine(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-48 * time.Hour)
	create := func(id, goal string, at time.Time) {
		t.Helper()
		if err := engine.store.Create(ctx, session.Session{ID: id, Goal: goal, Status: "completed", CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	create("parent", "Migrate the billing ledger", base)
	create("child", "Rewrite LedgerCompactor so the billing migration can resume", base.Add(time.Minute))
	if err := engine.store.AddRelation(ctx, session.Relation{ParentSessionID: "parent", ChildSessionID: "child", Kind: "task"}); err != nil {
		t.Fatal(err)
	}
	create("withdrawn", "LedgerCompactor notes from a revoked source", base.Add(2*time.Minute))
	for i := 0; i < 600; i++ {
		create(fmt.Sprintf("later%03d", i), "unrelated dashboard work", base.Add(time.Duration(10+i)*time.Minute))
	}
	fenceSession(t, engine, "withdrawn")

	hits, err := engine.Recall(ctx, "LedgerCompactor resume", 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ID != "child" || hits[0].ParentID != "parent" || hits[0].Kind != "task" {
		t.Fatalf("hits %+v, want the child task with its parent first", hits)
	}
	for _, h := range hits {
		if h.ID == "withdrawn" {
			t.Fatal("recall returned a fenced session")
		}
	}
	view, err := engine.ReadThread(ctx, "parent")
	if err != nil || len(view.Children) != 1 || view.Children[0].ID != "child" {
		t.Fatalf("parent view %+v %v, want its child", view, err)
	}
}

// A note's applicability follows its cited file and its sources: checked while the file is
// unchanged, unknown after it changes, withheld once the file is gone or a source is fenced; a
// fenced session shows nothing of itself.
func TestNoteApplicabilityFollowsSourcesAndFences(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, root := gitEngine(t)
	if err := os.WriteFile(filepath.Join(root, "retry.go"), []byte("package retry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a := &threadProvider{responses: []protocol.ProviderResponse{
		toolCalls(
			tc("n1", "note_write", `{"text":"Upload retries must stay off.","cites":["retry.go"]}`),
			tc("n2", "note_write", `{"text":"The queue looks idempotent."}`),
		),
		{Message: protocol.Message{Role: "assistant", Content: "noted"}, FinishReason: "stop"},
	}}
	bindRef(engine, "a", a)
	threadA := newTestSession(t, engine, "Investigate duplicate uploads")
	if r := run(t, engine, threadA.ID, AgentOptions{MaxSteps: 3}); r.Status != "completed" {
		t.Fatalf("thread A %+v", r)
	}
	notes := func() map[string]ThreadNote {
		t.Helper()
		view, err := engine.ReadThread(ctx, threadA.ID)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]ThreadNote{}
		for _, n := range view.Notes {
			key := "uncited"
			if strings.Contains(n.Text, "retries") || len(n.Cites) > 0 {
				key = "cited"
			}
			out[key] = n
		}
		return out
	}
	got := notes()
	if c := got["cited"]; c.Applicability != threadctx.ApplicabilityChecked || c.Origin != session.OriginAgentGen || c.LineageIncomplete {
		t.Fatalf("cited note %+v", c)
	}
	if u := got["uncited"]; u.Applicability != threadctx.ApplicabilityUnverified || u.Text == "" {
		t.Fatalf("uncited agent note %+v, want unverified with its text", u)
	}

	if err := os.WriteFile(filepath.Join(root, "retry.go"), []byte("package retry\n\nconst On = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := notes()["cited"]; c.Applicability != threadctx.ApplicabilityUnknown || !c.Stale || c.Text == "" {
		t.Fatalf("after the change %+v, want unknown, stale, text kept", c)
	}
	if err := os.Remove(filepath.Join(root, "retry.go")); err != nil {
		t.Fatal(err)
	}
	// The host (the user's notes panel) keeps the text, labelled; an agent's read_thread does not.
	if c := notes()["cited"]; c.Applicability != threadctx.ApplicabilityMissing || !strings.Contains(c.Text, "retries") || len(c.Cites) != 1 {
		t.Fatalf("host view after removal %+v, want source_missing with the text and the cite", c)
	}
	raw, err := threadReader{engine: engine, sessionID: "reader", workspace: root}.ReadThread(ctx, threadA.ID, false, -1)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range raw.(ThreadView).Notes {
		if n.Applicability == threadctx.ApplicabilityMissing && (n.Text != "" || len(n.Cites) != 1) {
			t.Fatalf("agent view after removal %+v, want the text withheld and the cite kept", n)
		}
		if strings.Contains(n.Text, "retries") {
			t.Fatalf("agent read_thread returned a gone-source note's text: %+v", n)
		}
	}

	// Fencing the request the uncited note came from withdraws it and nothing else.
	info, err := engine.TaskState(ctx, threadA.ID)
	if err != nil {
		t.Fatal(err)
	}
	var uncited taskstate.Note
	for _, n := range info.State.Notes {
		if len(n.Cites) == 0 {
			uncited = n
		}
	}
	st, err := engine.SourceStatus(ctx, threadA.ID, taskstate.EntryRef(taskstate.EntryNote, uncited.ID, uncited))
	if err != nil || st.RootKind != session.SourceRequest || st.RootRef == "" || !st.LineageComplete || st.Withdrawn {
		t.Fatalf("source status %+v %v, want a complete lineage to its request", st, err)
	}
	engineSQL(t, engine, `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at)
		VALUES('source', ?, 1, 'revoked', 'active', 't', 't')`, session.SourceScopeRef(st.RootKind, st.RootRef))
	if u := notes()["uncited"]; u.Applicability != threadctx.ApplicabilityWithdrawn || u.Text != "" {
		t.Fatalf("after fencing its request %+v, want withdrawn with no text", u)
	}

	fenceSession(t, engine, threadA.ID)
	view, err := engine.ReadThread(ctx, threadA.ID)
	if err != nil || !view.Withdrawn || view.Goal != "" || view.Final != "" || len(view.Notes) != 0 {
		t.Fatalf("fenced thread %+v %v, want withdrawn with no content", view, err)
	}
	if hits, err := engine.Recall(ctx, "duplicate uploads retries", 5, ""); err != nil || len(hits) != 0 {
		t.Fatalf("recall of a fenced thread = %+v %v", hits, err)
	}
}

func fenceSession(t *testing.T, engine *Engine, id string) {
	t.Helper()
	engineSQL(t, engine, `INSERT INTO source_fences(scope_kind, scope_ref, epoch, reason, state, created_at, updated_at)
		VALUES('session', ?, 1, 'revoked', 'active', 't', 't')`, id)
}

func engineSQL(t *testing.T, engine *Engine, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
