package agent

import (
	"strings"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
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


func TestLoopToolDenylistHidesAndRejectsDeniedTool(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "deny", Goal: "test deny", Status: "created", Metadata: map[string]any{}},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{
			Message: protocol.Message{
				Role: "assistant",
				ToolCalls: []protocol.ToolCall{
					call("blocked-shell", "shell", `{"command":"echo should-not-run"}`),
				},
			},
			FinishReason: "tool_calls",
		},
		{
			Message: protocol.Message{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		},
	}}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "deny", Options{
		MaxSteps:     2,
		ToolDenylist: []string{"shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result=%#v", result)
	}
	if len(provider.seen) == 0 {
		t.Fatal("provider did not receive request")
	}
	for _, spec := range provider.seen[0].Tools {
		if spec.Name == "shell" {
			t.Fatalf("denied shell leaked into provider tools: %#v", provider.seen[0].Tools)
		}
	}
	foundDeniedResult := false
	for _, message := range store.messages {
		if message.Role == "tool" && message.ToolCallID == "blocked-shell" {
			foundDeniedResult = strings.Contains(message.Content, "not allowed")
		}
	}
	if !foundDeniedResult {
		t.Fatalf("denied tool result missing from messages: %#v", store.messages)
	}
}


func TestLoopToolDenylistRemovesSpecAndRejectsDirectCall(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "deny", Goal: "do not shell", Status: "created", Metadata: map[string]any{}},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{
			Message: protocol.Message{
				ToolCalls: []protocol.ToolCall{call("deny-1", "shell", `{"command":"echo should-not-run"}`)},
			},
			FinishReason: "tool_calls",
		},
		{
			Message: protocol.Message{Content: "done"},
			FinishReason: "stop",
		},
	}}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "deny", Options{
		MaxSteps:     3,
		ToolDenylist: []string{"shell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result=%#v", result)
	}
	if len(provider.seen) == 0 {
		t.Fatal("provider request missing")
	}
	for _, spec := range provider.seen[0].Tools {
		if spec.Name == "shell" {
			t.Fatalf("denied tool leaked into provider request: %#v", provider.seen[0].Tools)
		}
	}
	foundDenied := false
	for _, message := range store.messages {
		if message.Role == "tool" && message.ToolCallID == "deny-1" {
			foundDenied = strings.Contains(message.Content, "not allowed")
		}
	}
	if !foundDenied {
		t.Fatalf("denied tool result missing: %#v", store.messages)
	}
}


func TestLoopCognitiveRoutingPersistsPlanAndPassesReasoningEffort(t *testing.T) {
	store := &memoryStore{
		state: SessionState{
			ID: "cognition",
			Goal: "Debug a production database migration deadlock and verify the root cause",
			Status: "created",
			Metadata: map[string]any{},
		},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done"}, FinishReason: "stop"},
	}}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "cognition", Options{
		MaxSteps: 2,
		CognitionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result=%#v", result)
	}
	if len(provider.seen) != 1 {
		t.Fatalf("requests=%d", len(provider.seen))
	}
	req := provider.seen[0]
	if req.ReasoningEffort == "" || req.ReasoningEffort == "none" {
		t.Fatalf("expected active reasoning effort, got %q", req.ReasoningEffort)
	}
	foundPolicy := false
	for _, message := range req.Messages {
		if message.Role == "system" && strings.Contains(message.Content, "Cognitive policy:") {
			foundPolicy = true
		}
	}
	if !foundPolicy {
		t.Fatalf("missing cognitive policy prompt: %#v", req.Messages)
	}
	cognitive, ok := store.state.Metadata["cognition"].(map[string]any)
	if !ok {
		t.Fatalf("missing persisted cognition metadata: %#v", store.state.Metadata)
	}
	history, ok := cognitive["history"].([]any)
	if !ok || len(history) != 1 {
		t.Fatalf("cognition history=%#v", cognitive["history"])
	}
}


func TestLoopWorkUnitBlocksPrematureFinalUntilEvidenceAndVerificationPass(t *testing.T) {
	store := &memoryStore{
		state: SessionState{
			ID: "work-units",
			Goal: "repair the transaction bug",
			Status: "created",
			Metadata: map[string]any{},
		},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done too early"}, FinishReason: "stop"},
		{
			Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				call("wu-update", "work_unit_update", `{
					"id":"repair",
					"status":"completed",
					"summary":"verified",
					"evidence":[{"requirement":"production path","ref":"service.go:42"}],
					"verification_results":[{"check":"focused test","status":"passed","detail":"ok"}]
				}`),
			}},
			FinishReason: "tool_calls",
		},
		{Message: protocol.Message{Content: "all work units complete"}, FinishReason: "stop"},
	}}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	workUnits := []byte(`{"workUnits":[{
		"id":"repair",
		"goal":"repair the transaction bug",
		"requiredEvidence":["production path"],
		"verification":["focused test"]
	}]}`)
	result, err := loop.Run(context.Background(), "work-units", Options{
		MaxSteps: 5,
		WorkUnitsJSON: workUnits,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Final != "all work units complete" {
		t.Fatalf("result=%#v", result)
	}
	if len(provider.seen) != 3 {
		t.Fatalf("requests=%d", len(provider.seen))
	}
	foundCorrection := false
	for _, message := range store.messages {
		if message.Role == "user" && strings.Contains(message.Content, "Work Unit completion gate rejected") {
			foundCorrection = true
			break
		}
	}
	if !foundCorrection {
		t.Fatalf("missing Work Unit completion correction: %#v", store.messages)
	}
	raw, err := json.Marshal(store.state.Metadata["workUnits"])
	if err != nil {
		t.Fatal(err)
	}
	var persisted map[string]any
	if err := json.Unmarshal(raw, &persisted); err != nil {
		t.Fatal(err)
	}
	items, ok := persisted["items"].(map[string]any)
	if !ok {
		t.Fatalf("persisted work units=%#v", persisted)
	}
	repair, ok := items["repair"].(map[string]any)
	if !ok || repair["status"] != "completed" {
		t.Fatalf("repair unit=%#v", items["repair"])
	}
}

func TestLoopCognitiveSessionCanCreateWorkUnitsDynamically(t *testing.T) {
	store := &memoryStore{
		state: SessionState{
			ID: "dynamic-work-unit",
			Goal: "implement a bounded change",
			Status: "created",
			Metadata: map[string]any{},
		},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{
			Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				call("wu-create", "work_unit_create", `{"id":"dynamic","goal":"implement the change","risk":"low"}`),
			}},
			FinishReason: "tool_calls",
		},
		{
			Message: protocol.Message{ToolCalls: []protocol.ToolCall{
				call("wu-complete", "work_unit_update", `{"id":"dynamic","status":"completed","summary":"done"}`),
			}},
			FinishReason: "tool_calls",
		},
		{Message: protocol.Message{Content: "done"}, FinishReason: "stop"},
	}}
	loop := Loop{Provider: provider, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "dynamic-work-unit", Options{
		MaxSteps: 5,
		CognitionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result=%#v", result)
	}
	if len(provider.seen) < 1 {
		t.Fatal("provider saw no requests")
	}
	names := toolNames(provider.seen[0].Tools)
	if !containsString(names, "work_unit_create") || !containsString(names, "work_unit_update") {
		t.Fatalf("missing dynamic Work Unit tools: %#v", names)
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}


type chainProvider struct {
	model       string
	fail        bool
	calls       int
	lastRequest protocol.ProviderRequest
}

func (p *chainProvider) Model() string { return p.model }

func (p *chainProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.calls++
	p.lastRequest = req
	if p.fail {
		return protocol.ProviderResponse{}, errors.New("provider unavailable")
	}
	return protocol.ProviderResponse{
		Message: protocol.Message{Content: "done via " + p.model},
		FinishReason: "stop",
	}, nil
}

func TestLoopCategoryProviderChainFailoverAndCircuitSkip(t *testing.T) {
	health := cognition.NewHealthRegistry(cognition.HealthConfig{
		FailureThreshold: 1,
		Cooldown: time.Hour,
	})
	bad := &chainProvider{model: "bad-model", fail: true}
	good := &chainProvider{model: "good-model"}
	fallback := &chainProvider{model: "fallback-model"}

	events := []Event{}
	store := &memoryStore{
		state: SessionState{
			ID: "chain-1",
			Goal: "Debug a production database migration deadlock",
			Status: "created",
			Metadata: map[string]any{},
		},
		steps: map[int64]any{},
	}
	loop := Loop{
		Provider: fallback,
		ProviderName: "fallback",
		ProviderChains: map[string][]ProviderBinding{
			"deep": {
				{Name: "primary", Provider: bad},
				{Name: "secondary", Provider: good},
			},
		},
		ProviderHealth: health,
		Store: store,
		Tools: fakeTools{},
		Emit: func(event Event) { events = append(events, event) },
	}

	result, err := loop.Run(context.Background(), "chain-1", Options{
		MaxSteps: 2,
		CognitionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "done via good-model" {
		t.Fatalf("result=%#v", result)
	}
	if bad.calls != 1 || good.calls != 1 || fallback.calls != 0 {
		t.Fatalf("calls bad=%d good=%d fallback=%d", bad.calls, good.calls, fallback.calls)
	}
	if health.Available("model:primary:bad-model") {
		t.Fatal("expected failed primary provider circuit to be open")
	}
	failovers := 0
	for _, event := range events {
		if event.Type == "provider.failover" {
			failovers++
		}
	}
	if failovers != 1 {
		t.Fatalf("provider failovers=%d events=%#v", failovers, events)
	}
	step, ok := store.steps[1].(stepRecord)
	if !ok {
		t.Fatalf("step=%#v", store.steps[1])
	}
	if step.Provider != "secondary" || step.Model != "good-model" {
		t.Fatalf("provider trace=%#v", step)
	}

	store2 := &memoryStore{
		state: SessionState{
			ID: "chain-2",
			Goal: "Debug a production database migration deadlock",
			Status: "created",
			Metadata: map[string]any{},
		},
		steps: map[int64]any{},
	}
	loop.Store = store2
	result, err = loop.Run(context.Background(), "chain-2", Options{
		MaxSteps: 2,
		CognitionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "done via good-model" {
		t.Fatalf("result2=%#v", result)
	}
	if bad.calls != 1 {
		t.Fatalf("circuit-open primary should have been skipped, calls=%d", bad.calls)
	}
	if good.calls != 2 {
		t.Fatalf("secondary calls=%d", good.calls)
	}
}


type agentDecisionStub struct{}

func (agentDecisionStub) Name() string  { return "decision-stub" }
func (agentDecisionStub) Model() string { return "system-one-test" }

func (agentDecisionStub) Decide(context.Context, cognition.DecisionRequest) (cognition.DecisionResult, error) {
	return cognition.DecisionResult{
		Source: "decision-stub",
		Model: "system-one-test",
		Signals: cognition.Signals{
			Category: "writing",
			CategoryConfidence: 0.95,
			NeedThink: 0.9,
			HasNeedThink: true,
			Retrieval: "historical",
		},
	}, nil
}

func TestLoopDecisionLayerControlsCategoryChainAndThinkEffort(t *testing.T) {
	health := cognition.NewHealthRegistry(cognition.HealthConfig{
		FailureThreshold: 2,
		Cooldown: time.Minute,
	})
	writer := &chainProvider{model: "writer-model"}
	fallback := &chainProvider{model: "fallback-model"}
	store := &memoryStore{
		state: SessionState{
			ID: "decision-route",
			Goal: "update the release notes",
			Status: "created",
			Metadata: map[string]any{},
		},
		steps: map[int64]any{},
	}
	loop := Loop{
		Provider: fallback,
		ProviderName: "fallback",
		ProviderChains: map[string][]ProviderBinding{
			"writing": {
				{Name: "writer", Provider: writer},
			},
		},
		ProviderHealth: health,
		DecisionLayer: &cognition.DecisionLayer{
			Providers: []cognition.DecisionProvider{agentDecisionStub{}},
			Health: health,
		},
		Store: store,
		Tools: fakeTools{},
	}
	result, err := loop.Run(context.Background(), "decision-route", Options{
		MaxSteps: 2,
		CognitionEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Final != "done via writer-model" {
		t.Fatalf("result=%#v", result)
	}
	if writer.calls != 1 || fallback.calls != 0 {
		t.Fatalf("calls writer=%d fallback=%d", writer.calls, fallback.calls)
	}
	if writer.lastRequest.ReasoningEffort == "" || writer.lastRequest.ReasoningEffort == "none" {
		t.Fatalf("reasoning effort=%q", writer.lastRequest.ReasoningEffort)
	}
	step, ok := store.steps[1].(stepRecord)
	if !ok || step.Provider != "writer" || step.Model != "writer-model" {
		t.Fatalf("step=%#v", store.steps[1])
	}
	cognitionMeta, ok := store.state.Metadata["cognition"].(map[string]any)
	if !ok {
		t.Fatalf("metadata=%#v", store.state.Metadata)
	}
	if _, ok := cognitionMeta["lastDecision"]; !ok {
		t.Fatalf("lastDecision missing: %#v", cognitionMeta)
	}
}


func TestAdjustedMaxTokensMatchesNodeThinkEffortPolicy(t *testing.T) {
	cases := []struct {
		effort cognition.Effort
		want   int
	}{
		{cognition.EffortLow, 1000},
		{cognition.EffortMedium, 1150},
		{cognition.EffortHigh, 1500},
		{cognition.EffortMax, 2000},
	}
	for _, tc := range cases {
		if got := adjustedMaxTokens(1000, tc.effort); got != tc.want {
			t.Fatalf("effort=%s got=%d want=%d", tc.effort, got, tc.want)
		}
	}
	if got := adjustedMaxTokens(0, cognition.EffortMax); got != 0 {
		t.Fatalf("unbounded max tokens changed: %d", got)
	}
}
