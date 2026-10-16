package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

type memoryStore struct {
	mu       sync.Mutex
	state    SessionState
	messages []protocol.Message
	steps    map[int64]any
	cause    map[string]any // sessions.error_json as last written

	// ts is the newest Task State version (Version 0: none); reasons lists the reason of every
	// version written, in order. inbox holds queued host edits. putErr fails every Task State write.
	ts      taskstate.State
	reasons []string
	inbox   []taskstate.QueuedEdit
	putErr  error
}

func (s *memoryStore) Get(context.Context, string) (SessionState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}
func (s *memoryStore) RecentMessages(_ context.Context, _ string, fromSeq int64, limit int) ([]protocol.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	start := max(len(s.messages)-limit, int(fromSeq), 0)
	if start > len(s.messages) {
		start = len(s.messages)
	}
	return append([]protocol.Message(nil), s.messages[start:]...), nil
}
func (s *memoryStore) AppendMessage(_ context.Context, _ string, m protocol.Message, change *TaskStateChange) (int64, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var version int64
	if change != nil {
		v, err := s.putLocked(*change)
		if err != nil {
			return 0, 0, err
		}
		version = v
	}
	s.messages = append(s.messages, m)
	return int64(len(s.messages) - 1), version, nil
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
func (s *memoryStore) TaskState(context.Context, string) (taskstate.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ts.Clone(), nil
}
func (s *memoryStore) InboxEdits(context.Context, string) ([]taskstate.QueuedEdit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]taskstate.QueuedEdit(nil), s.inbox...), nil
}
func (s *memoryStore) PutTaskState(_ context.Context, _ string, change TaskStateChange) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putLocked(change)
}

// putLocked applies the store's rules: base must be the newest version, the write stores a copy and
// consumes the applied edits.
func (s *memoryStore) putLocked(change TaskStateChange) (int64, error) {
	if s.putErr != nil {
		return 0, s.putErr
	}
	if change.Next.Version != s.ts.Version {
		return 0, fmt.Errorf("task state conflict: base %d, newest %d", change.Next.Version, s.ts.Version)
	}
	next := change.Next.Clone()
	next.Version = s.ts.Version + 1
	s.ts = next
	s.reasons = append(s.reasons, change.Reason)
	if change.InboxThrough > 0 {
		kept := s.inbox[:0]
		for _, e := range s.inbox {
			if e.Seq > change.InboxThrough {
				kept = append(kept, e)
			}
		}
		s.inbox = kept
	}
	return next.Version, nil
}

// queue adds a host edit as the runtime's EditTaskState would.
func (s *memoryStore) queue(e taskstate.Edit) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seq := int64(len(s.inbox)) + 1
	if n := len(s.inbox); n > 0 {
		seq = s.inbox[n-1].Seq + 1
	}
	s.inbox = append(s.inbox, taskstate.QueuedEdit{Seq: seq, At: time.Now().UTC(), Edit: e})
}

// seed stores the workflow and work-unit seeds as version 1, as a Start would.
func (s *memoryStore) seed(t *testing.T, workflowJSON, workUnitsJSON []byte) {
	t.Helper()
	next := s.ts.Clone()
	if _, err := next.Seed(workflowJSON, workUnitsJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutTaskState(context.Background(), s.state.ID, TaskStateChange{Reason: taskstate.ReasonStart, Next: next}); err != nil {
		t.Fatal(err)
	}
}
func (s *memoryStore) Update(_ context.Context, _ string, p SessionPatch) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.Status != nil {
		s.state.Status = *p.Status
	}
	if p.ClearFinal {
		s.state.Final = ""
	} else if p.Final != nil {
		s.state.Final = *p.Final
	}
	if p.ClearError {
		s.cause = nil
	} else if p.Error != nil {
		s.cause = p.Error
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
	store := &memoryStore{state: SessionState{ID: "s1", Goal: "fix it", Status: "running"}, steps: map[int64]any{}}
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
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	store.seed(t, workflow, nil)
	result, err := loop.Run(context.Background(), "s1", Options{Session: testRef, RecentMessages: 6, MaxSteps: 8})
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
	store := &memoryStore{state: SessionState{ID: "s2", Goal: "verify", Status: "running"}, steps: map[int64]any{}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done too early"}}, {Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "shell", `{"command":"test-pass"}`)}}}, {Message: protocol.Message{Content: "done"}}}}
	workflow := []byte(`{"id":"wf","facts":{"passed":false},"actions":{"verify":{"terminal":true,"allowedTools":["shell"],"outcomes":[{"when":{"result":"exitCode","equals":0},"set":{"passed":true}}],"completeWhen":{"fact":"passed","equals":true}}}}`)
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	store.seed(t, workflow, nil)
	result, err := loop.Run(context.Background(), "s2", Options{Session: testRef, RecentMessages: 6, MaxSteps: 5})
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
			if m.Origin != protocol.OriginCompletionBlocked {
				t.Fatalf("workflow correction origin=%q, want completion_blocked", m.Origin)
			}
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
	store := &memoryStore{state: SessionState{ID: "cancel", Goal: "wait", Status: "running"}, steps: map[int64]any{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	loop := Loop{Models: testModels(cancelProvider{}), Store: store, Tools: fakeTools{}}
	_, err := loop.Run(ctx, "cancel", Options{Session: testRef, MaxSteps: 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if store.state.Status != "interrupted" {
		t.Fatalf("status=%s", store.state.Status)
	}
}

func TestLoopAddsAdditionalSystemPrompt(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "skills", Goal: "build", Status: "running"},
		steps: map[int64]any{},
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done"}}}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	_, err := loop.Run(context.Background(), "skills", Options{
		Session:                testRef,
		MaxSteps:               2,
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
		state: SessionState{ID: "deny", Goal: "test deny", Status: "running"},
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
			Message:      protocol.Message{Role: "assistant", Content: "done"},
			FinishReason: "stop",
		},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "deny", Options{
		Session:      testRef,
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
		state: SessionState{ID: "deny", Goal: "do not shell", Status: "running"},
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
			Message:      protocol.Message{Content: "done"},
			FinishReason: "stop",
		},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "deny", Options{
		Session:      testRef,
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

func TestLoopWorkUnitBlocksPrematureFinalUntilEvidenceAndVerificationPass(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "work-units", Goal: "repair the transaction bug", Status: "running"},
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
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	workUnits := []byte(`{"workUnits":[{
		"id":"repair",
		"goal":"repair the transaction bug",
		"requiredEvidence":["production path"],
		"verification":["focused test"]
	}]}`)
	store.seed(t, nil, workUnits)
	result, err := loop.Run(context.Background(), "work-units", Options{Session: testRef, MaxSteps: 5})
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
			if message.Origin != protocol.OriginCompletionBlocked {
				t.Fatalf("work unit correction origin=%q, want completion_blocked", message.Origin)
			}
			break
		}
	}
	if !foundCorrection {
		t.Fatalf("missing Work Unit completion correction: %#v", store.messages)
	}
	if store.ts.WorkUnits == nil || store.ts.WorkUnits.Items["repair"].Status != taskstate.WorkCompleted {
		t.Fatalf("stored work units=%#v", store.ts.WorkUnits)
	}
	if !slices.Contains(store.reasons, taskstate.ReasonToolPrefix+"work_unit_update") {
		t.Fatalf("the completion was not committed with its tool row: reasons=%v", store.reasons)
	}
}

func TestLoopCanCreateWorkUnitsWithoutASeed(t *testing.T) {
	store := &memoryStore{
		state: SessionState{ID: "dynamic-work-unit", Goal: "implement a bounded change", Status: "running"},
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
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "dynamic-work-unit", Options{Session: testRef, MaxSteps: 5})
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

// outageProvider fails its first request with an availability error carrying retryAfter, then
// answers from script.
type outageProvider struct {
	scriptedProvider
	retryAfter time.Duration
	failed     bool
}

func (p *outageProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if !p.failed {
		p.failed = true
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, RetryAfter: p.retryAfter, Err: errors.New("overloaded")}
	}
	return p.scriptedProvider.Complete(ctx, req)
}

// I14 across steps: after an availability failover the next step's candidate list leaves the failed
// model out and says why, every step record replays and traces to the model that answered, the
// provider's Retry-After sets how long the model stays out, and a later success clears the mark.
func TestLoopFailoverTraceAcrossStepsAndHealth(t *testing.T) {
	const a, b = config.ModelRef("test/a"), config.ModelRef("test/b")
	now := time.Unix(1_000_000, 0)
	health := NewHealth(func() time.Time { return now })
	pa := &outageProvider{retryAfter: time.Hour}
	pb := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("c1", "shell", `{"command":"ok"}`)}}, FinishReason: "tool_calls"},
		{Message: protocol.Message{Content: "done"}, FinishReason: "stop"},
	}}
	store := &memoryStore{state: SessionState{ID: "trace", Goal: "trace", Status: "running"}, steps: map[int64]any{}}
	loop := Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Health: health, Store: store, Tools: fakeTools{}}
	_, _, startDecision := ModelDecision("", a, a, "sha256:cfg")
	result, err := loop.Run(context.Background(), "trace", Options{Session: a, Failover: []config.ModelRef{b}, MaxSteps: 4, Decisions: []Decision{startDecision}})
	if err != nil || result.Status != StatusCompleted || result.Final != "done" {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(store.steps) != 2 {
		t.Fatalf("steps=%d, want 2", len(store.steps))
	}
	for step := int64(1); step <= 2; step++ {
		raw, err := json.Marshal(store.steps[step])
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyStep(raw); err != nil {
			t.Fatalf("step %d: %v\n%s", step, err, raw)
		}
		rec := store.steps[step].(stepRecord)
		if rec.ModelRef != b {
			t.Fatalf("step %d answered by %s, want %s", step, rec.ModelRef, b)
		}
	}
	second := store.steps[2].(stepRecord)
	var in candidatesInputs
	var out candidatesOutput
	if err := decodeDecision(second.Decisions[0], &in, &out); err != nil || second.Decisions[0].Rule != RuleModelCandidates {
		t.Fatalf("step 2 starts with %s (%v), want %s", second.Decisions[0].Rule, err, RuleModelCandidates)
	}
	if in.Session != a || !slices.Equal(in.Unavailable, []config.ModelRef{a}) || !slices.Equal(out.Candidates, []config.ModelRef{b}) {
		t.Fatalf("step 2 candidates: session %s unavailable %v -> %v", in.Session, in.Unavailable, out.Candidates)
	}

	// The model stays out for the hour the provider asked for, not the 30 s default.
	now = now.Add(31 * time.Minute)
	if health.Available(a) {
		t.Fatal("the provider's Retry-After was not recorded: the model came back after the default window")
	}
	now = now.Add(30 * time.Minute)
	if !health.Available(a) {
		t.Fatal("the model is still out after its Retry-After")
	}

	// A success clears a mark that has not expired. With every candidate marked, the registry is
	// advisory and the run still uses a; its answer must clear a.
	health.Fail(a, time.Hour)
	pa.scriptedProvider.responses = []protocol.ProviderResponse{{Message: protocol.Message{Content: "a is back"}, FinishReason: "stop"}}
	store2 := &memoryStore{state: SessionState{ID: "back", Goal: "back", Status: "running"}, steps: map[int64]any{}}
	loop.Store = store2
	if r, err := loop.Run(context.Background(), "back", Options{Session: a, MaxSteps: 2}); err != nil || r.Final != "a is back" {
		t.Fatalf("result %+v err %v", r, err)
	}
	if !health.Available(a) {
		t.Fatal("a success did not clear the model's mark")
	}
}
