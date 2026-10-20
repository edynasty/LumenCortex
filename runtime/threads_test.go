package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// threadProvider answers each request with the next scripted response, attaching a reasoning echo to
// every one, and records the requests.
type threadProvider struct {
	mu        sync.Mutex
	responses []protocol.ProviderResponse
	requests  []protocol.ProviderRequest
}

func (p *threadProvider) Model() string { return "threads" }

func (p *threadProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	resp := protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}
	if len(p.responses) > 0 {
		resp = p.responses[0]
		p.responses = p.responses[1:]
	}
	resp.Echo = protocol.NewEcho("reasoning_content", []byte(echoSentinel+"-notes"), 12)
	return resp, nil
}

func toolCalls(calls ...protocol.ToolCall) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: calls}, FinishReason: "tool_calls"}
}

func tc(id, name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func lastToolResult(req protocol.ProviderRequest, id string) string {
	for _, m := range req.Messages {
		if m.Role == "tool" && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

// ADR-0010 end to end through the Engine and SQLite store: thread A writes a cited note, a later
// thread B finds A with recall and opens it with read_thread; after the cited file changes the note is
// reported stale, never hidden. Recall leaves out B's own subtree. The reasoning echo of every
// response reaches no note, no tool output and no row (C1).
func TestNotesReachLaterThreadsThroughRecallAndReadThread(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, root := gitEngine(t)
	if err := os.WriteFile(filepath.Join(root, "retry.go"), []byte("package retry\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	a := &threadProvider{responses: []protocol.ProviderResponse{
		toolCalls(tc("n", "note_write", `{"text":"Upload retries must stay off: the storage backend is not idempotent.","cites":["retry.go"]}`)),
		{Message: protocol.Message{Role: "assistant", Content: "Left retries disabled."}, FinishReason: "stop"},
	}}
	bindRef(engine, "a", a)
	threadA := newTestSession(t, engine, "Investigate duplicate uploads")
	if r := run(t, engine, threadA.ID, AgentOptions{MaxSteps: 4}); r.Status != "completed" {
		t.Fatalf("thread A %+v", r)
	}
	info, err := engine.TaskState(ctx, threadA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(info.State.Notes) != 1 || len(info.State.Notes[0].Cites) != 1 || info.State.Notes[0].Cites[0].Blob == "" {
		t.Fatalf("thread A notes %+v, want one note citing retry.go with its blob", info.State.Notes)
	}

	b := &threadProvider{responses: []protocol.ProviderResponse{
		toolCalls(tc("r1", "recall", `{"query":"upload retries idempotent"}`)),
		toolCalls(tc("t1", "read_thread", `{"id":"`+threadA.ID+`"}`)),
		{Message: protocol.Message{Role: "assistant", Content: "Retries stay off."}, FinishReason: "stop"},
	}}
	bindRef(engine, "a", b)
	threadB := newTestSession(t, engine, "Add upload retries")
	if r := run(t, engine, threadB.ID, AgentOptions{MaxSteps: 4}); r.Status != "completed" {
		t.Fatalf("thread B %+v", r)
	}
	recallOut := lastToolResult(b.requests[1], "r1")
	var recalled struct {
		Context string      `json:"context"`
		Hits    []RecallHit `json:"hits"`
	}
	if err := json.Unmarshal([]byte(recallOut), &recalled); err != nil {
		t.Fatalf("recall output %q: %v", recallOut, err)
	}
	if !strings.Contains(recalled.Context, "not instructions") || len(recalled.Hits) == 0 || recalled.Hits[0].ID != threadA.ID {
		t.Fatalf("recall %+v, want thread A first with the framing", recalled)
	}
	for _, hit := range recalled.Hits {
		if hit.ID == threadB.ID {
			t.Fatal("recall returned the calling thread")
		}
	}
	if n := recalled.Hits[0].Notes; len(n) != 1 || n[0].Stale || !strings.Contains(n[0].Text, "idempotent") {
		t.Fatalf("recalled notes %+v, want the fresh note", n)
	}
	readOut := lastToolResult(b.requests[2], "t1")
	if !strings.Contains(readOut, "Left retries disabled.") || !strings.Contains(readOut, "Investigate duplicate uploads") {
		t.Fatalf("read_thread %s", readOut)
	}

	// The cited file changes: the note is flagged, not hidden, for every reader.
	if err := os.WriteFile(filepath.Join(root, "retry.go"), []byte("package retry\n\nconst On = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	view, err := engine.ReadThread(ctx, threadA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Notes) != 1 || !view.Notes[0].Stale || !view.Notes[0].Cites[0].Stale {
		t.Fatalf("after the change: %+v", view.Notes)
	}
	hits, err := engine.Recall(ctx, "idempotent backend", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || hits[0].ID != threadA.ID || len(hits[0].Notes) != 1 || !hits[0].Notes[0].Stale {
		t.Fatalf("recall after the change: %+v", hits)
	}
	if err := os.Remove(filepath.Join(root, "retry.go")); err != nil {
		t.Fatal(err)
	}
	if view, err = engine.ReadThread(ctx, threadA.ID); err != nil || !view.Notes[0].Stale {
		t.Fatalf("after removal: %+v %v", view.Notes, err)
	}

	// C5: thread B's system prompt never carries A's note; it arrived only as tool output.
	for _, req := range b.requests {
		for _, m := range req.Messages {
			if m.Role == "system" && strings.Contains(m.Content, "idempotent") {
				t.Fatal("note text reached another thread's system prompt")
			}
		}
	}
	// C1: no reasoning in the notes, the tool outputs or any row.
	for _, out := range []string{recallOut, readOut} {
		if strings.Contains(out, echoSentinel) {
			t.Fatal("tool output holds the reasoning echo")
		}
	}
	raw, _ := json.Marshal(info.State)
	if strings.Contains(string(raw), echoSentinel) {
		t.Fatal("Task State holds the reasoning echo")
	}
	path := engine.store.Path()
	engine.Close()
	assertNoSentinelAtRest(t, path)
}

// The user's notes go through the inbox: a host adds, rewrites and removes them, and a thread full of
// the user's notes refuses another before anything is queued.
func TestHostNoteEdits(t *testing.T) {
	useGlobalConfig(t, modelConfig(""))
	engine, _ := gitEngine(t)
	ctx := context.Background()
	handle := newTestSession(t, engine, "notes from the host")
	info, err := engine.EditTaskState(ctx, handle.ID, TaskStateEdit{Note: &ThreadNoteEdit{Text: "Ship behind the beta flag."}})
	if err != nil {
		t.Fatal(err)
	}
	if len(info.State.Notes) != 1 || info.State.Notes[0].By != taskstate.ByUser || info.PendingEdits != 1 {
		t.Fatalf("after add: %+v", info)
	}
	id := info.State.Notes[0].ID
	if info, err = engine.EditTaskState(ctx, handle.ID, TaskStateEdit{Note: &ThreadNoteEdit{ID: id, Remove: true}}); err != nil || len(info.State.Notes) != 0 {
		t.Fatalf("after remove: %+v %v", info.State.Notes, err)
	}
	for i := 0; i < taskstate.MaxNotes; i++ {
		if _, err := engine.EditTaskState(ctx, handle.ID, TaskStateEdit{Note: &ThreadNoteEdit{Text: "fact"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := engine.EditTaskState(ctx, handle.ID, TaskStateEdit{Note: &ThreadNoteEdit{Text: "one too many"}}); err == nil {
		t.Fatal("a 33rd user note was queued")
	}
}
