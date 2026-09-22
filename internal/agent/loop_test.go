package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

type memoryStore struct {
	mu       sync.Mutex
	state    SessionState
	messages []protocol.Message
	steps    map[int64]any
}

func (s *memoryStore) Get(context.Context, string) (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}
func (s *memoryStore) RecentMessages(_ context.Context, _ string, limit int) ([]protocol.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := len(s.messages) - limit
	if start < 0 {
		start = 0
	}
	return append([]protocol.Message(nil), s.messages[start:]...), nil
}
func (s *memoryStore) AppendMessage(_ context.Context, _ string, m protocol.Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messages = append(s.messages, m)
	return int64(len(s.messages) - 1), nil
}
func (s *memoryStore) AppendStep(_ context.Context, _ string, step int64, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.steps == nil {
		s.steps = map[int64]any{}
	}
	s.steps[step] = v
	return nil
}
func (s *memoryStore) NextStep(context.Context, string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int64(len(s.steps)) + 1, nil
}
func (s *memoryStore) Update(_ context.Context, _ string, p SessionPatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Status != nil {
		s.state.Status = *p.Status
	}
	if p.Metadata != nil {
		s.state.Metadata = p.Metadata
	}
	if p.ClearFinal {
		s.state.Final = ""
	} else if p.Final != nil {
		s.state.Final = *p.Final
	}
	if p.Usage != nil {
		s.state.Usage = *p.Usage
	}
	return nil
}

type scriptedProvider struct {
	mu        sync.Mutex
	responses []protocol.ProviderResponse
	seen      []protocol.ProviderRequest
}

func (p *scriptedProvider) Model() string { return "fake" }
func (p *scriptedProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, req)
	if len(p.responses) == 0 {
		return protocol.ProviderResponse{}, errors.New("no scripted response")
	}
	r := p.responses[0]
	p.responses = p.responses[1:]
	return r, nil
}

type fakeTools struct{}

func (fakeTools) Specs(allow []string) []protocol.ToolSpec {
	all := []protocol.ToolSpec{{Name: "shell"}, {Name: "apply_patch"}}
	if allow == nil {
		return all
	}
	set := map[string]bool{}
	for _, n := range allow {
		set[n] = true
	}
	out := []protocol.ToolSpec{}
	for _, s := range all {
		if set[s.Name] {
			out = append(out, s)
		}
	}
	return out
}
func (fakeTools) Execute(_ context.Context, name string, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	switch name {
	case "shell":
		code := 0
		if fmt.Sprint(args["command"]) == "test-fail" {
			code = 1
		}
		raw, _ := json.Marshal(map[string]any{"exitCode": code})
		return protocol.ToolResult{OK: code == 0, Content: string(raw)}, nil
	case "apply_patch":
		return protocol.ToolResult{OK: true, Content: `{"changed":true}`, MutatesWorkspace: true}, nil
	}
	return protocol.ToolResult{}, errors.New("unknown tool")
}

func call(id, name, args string) protocol.ToolCall {
	return protocol.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
}

func TestLoopWorkflowAndBoundedRecentMessages(t *testing.T) {
	store := &memoryStore{state: SessionState{ID: "s1", Goal: "fix it", Status: "running", Metadata: map[string]any{}}, steps: map[int64]any{}}
	for i := 0; i < 100; i++ {
		store.messages = append(store.messages, protocol.Message{Role: "user", Content: fmt.Sprintf("old-%d", i)})
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "shell", `{"command":"test-fail"}`)}}},
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("2", "apply_patch", `{}`)}}},
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("3", "shell", `{"command":"test-pass"}`)}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	workflow := []byte(`{"id":"wf","entry":"diagnose","facts":{"failed":false,"changed":false,"passed":false},"actions":{"diagnose":{"allowedTools":["shell"],"outcomes":[{"when":{"result":"exitCode","notEquals":0},"set":{"failed":true}}],"routes":[{"to":"repair","when":{"fact":"failed","equals":true}}]},"repair":{"allowedTools":["apply_patch"],"outcomes":[{"when":{"tool":"apply_patch","ok":true},"set":{"changed":true}}],"completeWhen":{"fact":"changed","equals":true},"routes":[{"to":"verify"}]},"verify":{"terminal":true,"allowedTools":["shell"],"outcomes":[{"when":{"result":"exitCode","equals":0},"set":{"passed":true}}],"completeWhen":{"fact":"passed","equals":true}}}}`)
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "s1", Options{RecentMessages: 6, MaxSteps: 8, WorkflowJSON: workflow})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Final != "done" {
		t.Fatalf("result=%#v", result)
	}
	for i, req := range provider.seen {
		if len(req.Messages) > 8 {
			t.Fatalf("request %d has %d messages", i, len(req.Messages))
		}
	}
	if len(provider.seen) < 4 {
		t.Fatalf("requests=%d", len(provider.seen))
	}
}

func TestLoopRejectsPrematureFinal(t *testing.T) {
	store := &memoryStore{state: SessionState{ID: "s2", Goal: "verify", Status: "running", Metadata: map[string]any{}}, steps: map[int64]any{}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done too early"}}, {Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "shell", `{"command":"test-pass"}`)}}}, {Message: protocol.Message{Content: "done"}}}}
	workflow := []byte(`{"id":"wf","facts":{"passed":false},"actions":{"verify":{"terminal":true,"allowedTools":["shell"],"outcomes":[{"when":{"result":"exitCode","equals":0},"set":{"passed":true}}],"completeWhen":{"fact":"passed","equals":true}}}}`)
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "s2", Options{RecentMessages: 6, MaxSteps: 5, WorkflowJSON: workflow})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result=%#v", result)
	}
	found := false
	for _, m := range store.messages {
		if m.Role == "user" && len(m.Content) > 0 && m.Content[:min(8, len(m.Content))] == "Workflow" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing workflow correction message")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type cancelProvider struct{}

func (cancelProvider) Model() string { return "cancel" }
func (cancelProvider) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	<-ctx.Done()
	return protocol.ProviderResponse{}, ctx.Err()
}

func TestLoopPersistsInterruptionOnCancellation(t *testing.T) {
	store := &memoryStore{state: SessionState{ID: "cancel", Goal: "wait", Status: "running", Metadata: map[string]any{}}, steps: map[int64]any{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop := Loop{Provider: cancelProvider{}, Store: store, Tools: fakeTools{}}
	_, err := loop.Run(ctx, "cancel", Options{MaxSteps: 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if store.state.Status != "interrupted" {
		t.Fatalf("status=%s", store.state.Status)
	}
}


func TestLoopClearsStaleFinalWhenRunStarts(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "continue", Goal: "continue", Status: "completed", Final: "old final", Metadata: map[string]any{}},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "new final"}}}}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "continue", Options{MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "new final" {
		t.Fatalf("result final=%q", result.Final)
	}
	if store.state.Final != "new final" {
		t.Fatalf("persisted final=%q", store.state.Final)
	}
}


func TestLoopAddsAdditionalSystemPrompt(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "skills", Goal: "build", Status: "created", Metadata: map[string]any{}},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done"}}}}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	_, err := loop.Run(context.Background(), "skills", Options{
		MaxSteps: 2,
		AdditionalSystemPrompt: "Skill instruction: use repository conventions.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.seen) != 1 {
		t.Fatalf("requests=%d", len(provider.seen))
	}
	messages := provider.seen[0].Messages
	if len(messages) < 3 {
		t.Fatalf("messages=%#v", messages)
	}
	if messages[0].Role != "system" || messages[1].Role != "system" {
		t.Fatalf("system messages=%#v", messages[:2])
	}
	if messages[1].Content != "Skill instruction: use repository conventions." {
		t.Fatalf("additional prompt=%q", messages[1].Content)
	}
}
