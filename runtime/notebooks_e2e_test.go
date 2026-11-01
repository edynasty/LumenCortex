package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// notebookProvider records every request it is asked to answer and replies with its script.
type notebookProvider struct {
	mu        sync.Mutex
	requests  []protocol.ProviderRequest
	responses []protocol.ProviderResponse
}

func (p *notebookProvider) Model() string { return "notebook-script" }

func (p *notebookProvider) Complete(_ context.Context, request protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, request)
	if len(p.responses) == 0 {
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
	}
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

// messages returns every message of every request, in order.
func (p *notebookProvider) messages() []protocol.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []protocol.Message
	for _, request := range p.requests {
		out = append(out, request.Messages...)
	}
	return out
}

func toolCallResponse(id, name, arguments string) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
		{ID: id, Name: name, Arguments: json.RawMessage(arguments)},
	}}}
}

func finalResponse(text string) protocol.ProviderResponse {
	return protocol.ProviderResponse{Message: protocol.Message{Content: text}}
}

// TestNotebooksSurviveCloseReopenAndContextReset: a notebook written through the Engine API is durable
// across a Close and a reopen, and a run whose context was reset still reads it.
func TestNotebooksSurviveCloseReopenAndContextReset(t *testing.T) {
	workspace := t.TempDir()
	ctx := context.Background()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "durable notebook"})
	if err != nil {
		t.Fatal(err)
	}
	body := "# Plan\n\n## Handoff\nSENTINEL-HANDOFF-4f21 continue with internal/session\n"
	created, err := engine.CreateNotebook(ctx, handle.ID, "plan", body)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	infos, err := reopened.Notebooks(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].Version != 1 || infos[0].Title != "plan" {
		t.Fatalf("after reopen: %#v", infos)
	}
	if doc, err := reopened.Notebook(ctx, handle.ID, created.ID); err != nil || doc.Body != body {
		t.Fatalf("after reopen: %#v, %v", doc, err)
	}

	if _, err := reopened.ResetContext(ctx, handle.ID); err != nil {
		t.Fatal(err)
	}
	provider := &notebookProvider{responses: []protocol.ProviderResponse{
		toolCallResponse("1", "notebook_read", `{"notebook":"plan","section":"Handoff"}`),
		finalResponse("read it"),
	}}
	result, err := reopened.runAgentWith(ctx, handle.ID, provider, AgentOptions{Policy: "read-only", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("run = %#v", result)
	}
	var found bool
	for _, message := range provider.messages() {
		if message.Role == "tool" && strings.Contains(message.Content, "SENTINEL-HANDOFF-4f21") {
			found = true
		}
	}
	if !found {
		t.Fatal("the run after a context reset could not read the notebook")
	}
}

// TestNotebookIndexRowCarriesNoBody: the run-start index lists titles, versions and sections, and no
// request of the run ever holds a notebook body the model did not ask for.
func TestNotebookIndexRowCarriesNoBody(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	ctx := context.Background()
	for i := range 8 {
		title := fmt.Sprintf("notebook-%d", i)
		body := fmt.Sprintf("# %s\n\n## Plan\n\nSENTINEL-BODY-%d\n\n## Handoff\n\ncontinue\n", title, i)
		if _, err := engine.CreateNotebook(ctx, sessionID, title, body); err != nil {
			t.Fatal(err)
		}
	}
	provider := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("nothing to do")}}
	if _, err := engine.runAgentWith(ctx, sessionID, provider, AgentOptions{Policy: "read-only", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}

	var index *protocol.Message
	for i, message := range provider.messages() {
		if message.Origin == protocol.OriginNotebook {
			index = &provider.messages()[i]
			break
		}
	}
	if index == nil {
		t.Fatal("the run got no notebook index row")
	}
	if len(index.Content) > 600 {
		t.Fatalf("index row is %d bytes", len(index.Content))
	}
	for _, want := range []string{"Working notebooks", "notebook-7 (v1, updated ", "sections: Plan, Handoff", "more (notebook_list)", "notebook_read"} {
		if !strings.Contains(index.Content, want) {
			t.Fatalf("index row %q does not mention %q", index.Content, want)
		}
	}
	for _, message := range provider.messages() {
		if strings.Contains(message.Content, "SENTINEL-BODY-") {
			t.Fatalf("a request carried a notebook body: %q", message.Content)
		}
	}
	t.Logf("index row for 8 notebooks: %d bytes", len(index.Content))

	// A session without notebooks gets no row.
	other, err := engine.NewSession(ctx, SessionOptions{Goal: "no notebooks"})
	if err != nil {
		t.Fatal(err)
	}
	second := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("ok")}}
	if _, err := engine.runAgentWith(ctx, other.ID, second, AgentOptions{Policy: "read-only", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	for _, message := range second.messages() {
		if message.Origin == protocol.OriginNotebook {
			t.Fatalf("a session without notebooks got an index row: %q", message.Content)
		}
	}
}

// TestNotebookIndexRowIsBounded: 8 notebooks with long titles and many sections still produce one
// bounded row, never a second row.
func TestNotebookIndexRowIsBounded(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	ctx := context.Background()
	for i := range 8 {
		title := strings.Repeat("t", 60) + fmt.Sprint(i)
		var body strings.Builder
		body.WriteString("# x\n")
		for s := range 12 {
			fmt.Fprintf(&body, "\n## Section %d\n\ntext\n", s)
		}
		if _, err := engine.CreateNotebook(ctx, sessionID, title, body.String()); err != nil {
			t.Fatal(err)
		}
	}
	provider := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("ok")}}
	if _, err := engine.runAgentWith(ctx, sessionID, provider, AgentOptions{Policy: "read-only", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	rows := 0
	for _, message := range provider.messages() {
		if message.Origin == protocol.OriginNotebook {
			rows++
			if len(message.Content) > 600 {
				t.Fatalf("index row is %d bytes", len(message.Content))
			}
		}
	}
	if rows != 1 {
		t.Fatalf("index rows = %d, want 1", rows)
	}
}

// TestSaveNotebookConcurrentWritersHaveOneWinner: the Engine's compare-and-set has exactly one winner
// when several hosts write the same version at once.
func TestSaveNotebookConcurrentWritersHaveOneWinner(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	ctx := context.Background()
	if _, err := engine.CreateNotebook(ctx, sessionID, "plan", "start\n"); err != nil {
		t.Fatal(err)
	}
	const writers = 8
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = engine.SaveNotebook(ctx, sessionID, "plan", fmt.Sprintf("writer %d\n", i), 1)
		}(i)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		if err == nil {
			won++
			continue
		}
		var conflict *NotebookConflict
		if !errors.As(err, &conflict) || !errors.Is(err, ErrNotebookConflict) || conflict.CurrentVersion != 2 {
			t.Fatalf("loser = %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("winners = %d, want 1", won)
	}
	if doc, err := engine.Notebook(ctx, sessionID, "plan"); err != nil || doc.Version != 2 {
		t.Fatalf("final notebook = %#v, %v", doc, err)
	}
}

// TestNotebookEventsCarryNoBody: notebook.updated and notebook.deleted report the id, version and size
// only, never the body.
func TestNotebookEventsCarryNoBody(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	ctx := context.Background()
	stream, cancel := engine.Events(32)
	defer cancel()

	doc, err := engine.CreateNotebook(ctx, sessionID, "plan", "# Plan\n\n## Handoff\nSENTINEL-EVENT-BODY\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.SaveNotebook(ctx, sessionID, doc.ID, "# Plan\n\n## Handoff\nSENTINEL-EVENT-BODY-2\n", doc.Version); err != nil {
		t.Fatal(err)
	}
	if err := engine.DeleteNotebook(ctx, sessionID, doc.ID); err != nil {
		t.Fatal(err)
	}
	seen := map[string]json.RawMessage{}
	for drained := false; !drained; {
		select {
		case event := <-stream:
			seen[event.Type] = event.Data
			if strings.Contains(string(event.Data), "SENTINEL-EVENT-BODY") {
				t.Fatalf("event %s carries a body: %s", event.Type, event.Data)
			}
		default:
			drained = true
		}
	}
	raw, ok := seen["notebook.updated"]
	if !ok {
		t.Fatalf("no notebook.updated event: %v", seen)
	}
	var updated struct {
		NotebookID string `json:"notebookId"`
		Version    int64  `json:"version"`
		Bytes      int    `json:"bytes"`
	}
	decodeJSON(t, raw, &updated)
	if updated.NotebookID != doc.ID || updated.Version != 2 || updated.Bytes == 0 {
		t.Fatalf("notebook.updated = %s", raw)
	}
	raw, ok = seen["notebook.deleted"]
	if !ok {
		t.Fatalf("no notebook.deleted event: %v", seen)
	}
	var deleted struct {
		NotebookID string `json:"notebookId"`
		Version    int64  `json:"version"`
		Bytes      int    `json:"bytes"`
	}
	decodeJSON(t, raw, &deleted)
	if deleted.NotebookID != doc.ID || deleted.Version != 2 || deleted.Bytes == 0 {
		t.Fatalf("notebook.deleted = %s", raw)
	}
}
