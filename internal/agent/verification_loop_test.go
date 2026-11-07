package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

// scriptedChecks is a Checks over a fixed verdict list. approved is the host approval state.
type scriptedChecks struct {
	verdicts []evaluator.Verdict
	decided  int
}

func (f *scriptedChecks) Configured() (all, required []string) {
	for _, v := range f.verdicts {
		all = append(all, v.Result.Check)
		if v.Result.Kind != "" {
			required = append(required, v.Result.Check)
		}
	}
	return all, required
}

func (f *scriptedChecks) Decide(context.Context, []string, string) ([]evaluator.Verdict, error) {
	f.decided++
	return f.verdicts, nil
}

func (f *scriptedChecks) Mutated() {}

func (f *scriptedChecks) SetApproved(context.Context) (bool, error) { return true, nil }

// scriptedVerifier answers each Verify call with the next scripted verdict list.
type scriptedVerifier struct {
	mu      sync.Mutex
	results [][]verify.Verdict
	calls   int
}

func (v *scriptedVerifier) Verify(context.Context, verify.VerifyRequest) ([]verify.Verdict, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	index := v.calls
	v.calls++
	if index >= len(v.results) {
		return nil, nil
	}
	return v.results[index], nil
}

// recordingTools records every executed call on top of fakeTools.
type recordingTools struct {
	fakeTools
	mu    sync.Mutex
	calls []string
}

func (r *recordingTools) Execute(ctx context.Context, name string, args map[string]any, out func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	r.mu.Lock()
	r.calls = append(r.calls, name)
	r.mu.Unlock()
	return r.fakeTools.Execute(ctx, name, args, out)
}

func newRunningStore(id string) *memoryStore {
	store := &memoryStore{state: SessionState{ID: id, Goal: "deliver the report", Status: "running"}, steps: map[int64]any{}}
	store.seedGoal()
	return store
}

func completionBlocked(store *memoryStore) []protocol.Message {
	var out []protocol.Message
	for _, message := range store.messages {
		if message.Origin == protocol.OriginCompletionBlocked {
			out = append(out, message)
		}
	}
	return out
}

// A model's own claim is not verification: the independent semantic level fails once, the bounded
// feedback reaches the model, and the repaired proposal passes. The failed attempt stays recorded.
func TestLoopRepairsAfterIndependentSemanticFailure(t *testing.T) {
	store := newRunningStore("repair")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done, with the unit fixed"}},
	}}
	verifier := &scriptedVerifier{results: [][]verify.Verdict{
		{{Level: verify.LevelSemantic, Source: verify.SourceIndependent, Check: "independent", Status: verify.StatusFailed,
			Detail: "the report is in feet, the contract asks for metres"}},
		{{Level: verify.LevelSemantic, Source: verify.SourceIndependent, Check: "independent", Status: verify.StatusPassed}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Verifier: verifier}
	result, err := loop.Run(context.Background(), "repair", Options{
		Session: testRef, MaxSteps: 6, Verification: VerificationOptions{RequireSemantic: true, MaxRepairs: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted || result.Final != "done, with the unit fixed" {
		t.Fatalf("result=%+v", result)
	}
	blocked := completionBlocked(store)
	if len(blocked) != 1 || !strings.Contains(blocked[0].Content, "metres") {
		t.Fatalf("corrections=%+v", blocked)
	}
	record := store.ts.Verification
	if record == nil || record.Status != string(verify.StatusPassed) {
		t.Fatalf("verification=%+v", record)
	}
	if len(record.Attempts) != 1 || record.Attempts[0].Status != verify.StatusFailed {
		t.Fatalf("the failed attempt must be preserved: %+v", record.Attempts)
	}
	if verifier.calls != 2 {
		t.Fatalf("verify calls=%d", verifier.calls)
	}
}

// A required semantic level with no independent verdict is UNKNOWN; after the repair budget the run
// stops as unverified instead of completing on the model's word.
func TestLoopStopsAtUnknownSemanticWithinBudget(t *testing.T) {
	store := newRunningStore("unknown")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	_, err := loop.Run(context.Background(), "unknown", Options{
		Session: testRef, MaxSteps: 8, Verification: VerificationOptions{RequireSemantic: true, MaxRepairs: 1},
	})
	if !errors.Is(err, ErrSemanticUnverified) {
		t.Fatalf("err=%v", err)
	}
	if store.state.Status != StatusInterrupted {
		t.Fatalf("status=%s", store.state.Status)
	}
	record := store.ts.Verification
	if record == nil || record.Status != string(verify.StatusUnknown) {
		t.Fatalf("verification=%+v", record)
	}
	if len(record.Attempts) < 2 {
		t.Fatalf("attempts=%+v", record.Attempts)
	}
}

// A Stop hook can ask for more work, but its denials are finite: the run ends.
func TestLoopStopDenialsAreFinite(t *testing.T) {
	registry, err := verify.NewRegistry(verify.Version)
	if err != nil {
		t.Fatal(err)
	}
	registry.MaxStopDenials = 1
	if err := registry.Register(&lifecycleTestHook{id: "hold", version: 1, events: []verify.Event{verify.EventStop},
		run: func(context.Context, verify.Request) (verify.Result, error) {
			return verify.Result{Decision: verify.DecisionDeny, Feedback: "record the evidence first"}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	store := newRunningStore("stop")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Hooks: registry}
	result, err := loop.Run(context.Background(), "stop", Options{Session: testRef, MaxSteps: 6})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("result=%+v", result)
	}
	blocked := completionBlocked(store)
	if len(blocked) != 1 || !strings.Contains(blocked[0].Content, "record the evidence first") {
		t.Fatalf("corrections=%+v", blocked)
	}
}

// A PreToolUse deny answers the call without running the tool, and the model sees why.
func TestLoopPreToolUseDenySkipsExecution(t *testing.T) {
	registry, err := verify.NewRegistry(verify.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&lifecycleTestHook{id: "no-bash", version: 1, events: []verify.Event{verify.EventPreToolUse},
		run: func(_ context.Context, req verify.Request) (verify.Result, error) {
			if req.ToolName == "bash" {
				return verify.Result{Decision: verify.DecisionDeny, Feedback: "bash is not allowed for this task"}, nil
			}
			return verify.Result{Decision: verify.DecisionAllow}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	store := newRunningStore("deny")
	tools := &recordingTools{}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "bash", `{"command":"true"}`)}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Hooks: registry}
	result, err := loop.Run(context.Background(), "deny", Options{Session: testRef, MaxSteps: 5})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Fatalf("result=%+v", result)
	}
	if len(tools.calls) != 0 {
		t.Fatalf("the denied tool ran: %v", tools.calls)
	}
	var toolRow protocol.Message
	for _, message := range store.messages {
		if message.Role == "tool" {
			toolRow = message
		}
	}
	if !strings.Contains(toolRow.Content, "denied by a lifecycle hook") || !strings.Contains(toolRow.Content, "not allowed") {
		t.Fatalf("tool row=%q", toolRow.Content)
	}
}

// The cancellation and timeout exits run their hooks; the audit reaches the event stream.
func TestLoopObservesCancellationHook(t *testing.T) {
	registry, err := verify.NewRegistry(verify.Version)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&lifecycleTestHook{id: "on-cancel", version: 1, events: []verify.Event{verify.EventOnCancel},
		run: func(context.Context, verify.Request) (verify.Result, error) {
			return verify.Result{Decision: verify.DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{state: SessionState{ID: "cancel-hook", Goal: "wait", Status: "running"}, steps: map[int64]any{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var mu sync.Mutex
	var events []string
	loop := Loop{Models: testModels(cancelProvider{}), Store: store, Tools: fakeTools{}, Hooks: registry,
		Emit: func(event Event) {
			mu.Lock()
			events = append(events, event.Type)
			mu.Unlock()
		}}
	_, _ = loop.Run(ctx, "cancel-hook", Options{Session: testRef, MaxSteps: 2})
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(events, "hook.OnCancel") {
		t.Fatalf("events=%v", events)
	}
}

// The claim ledger is written by the model tool and the completion gate reads it: a required claim
// that only the model proposed is not confirmed, so completion is blocked until it is settled.
func TestLoopClaimToolFeedsTheGate(t *testing.T) {
	store := newRunningStore("claims")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "claim_write", `{"kind":"assumption","statement":"the input is metres","required":true}`)}}},
		{Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	_, err := loop.Run(context.Background(), "claims", Options{
		Session: testRef, MaxSteps: 8, Verification: VerificationOptions{MaxRepairs: 1},
	})
	if !errors.Is(err, ErrSemanticUnverified) {
		t.Fatalf("err=%v", err)
	}
	if len(store.ts.Claims) != 1 || store.ts.Claims[0].Status != verify.ClaimProposed {
		t.Fatalf("claims=%+v", store.ts.Claims)
	}
	blocked := completionBlocked(store)
	if len(blocked) == 0 || !strings.Contains(blocked[0].Content, "c1") {
		t.Fatalf("corrections=%+v", blocked)
	}
}

// The verification record is durable: a later reader sees the levels and the attempts.
func TestVerificationRecordIsDurable(t *testing.T) {
	store := newRunningStore("durable")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done"}}}}
	verifier := &scriptedVerifier{results: [][]verify.Verdict{{
		{Level: verify.LevelSemantic, Source: verify.SourceIndependent, Check: "independent", Status: verify.StatusPassed},
	}}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Verifier: verifier}
	if _, err := loop.Run(context.Background(), "durable", Options{
		Session: testRef, MaxSteps: 2, Verification: VerificationOptions{RequireSemantic: true},
	}); err != nil {
		t.Fatal(err)
	}
	state, err := store.TaskState(context.Background(), "durable")
	if err != nil {
		t.Fatal(err)
	}
	if state.Verification == nil || state.Verification.Status != string(verify.StatusPassed) {
		t.Fatalf("verification=%+v", state.Verification)
	}
	if state.Verification.UpdatedAt.IsZero() || state.Verification.UpdatedAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("updatedAt=%v", state.Verification.UpdatedAt)
	}
}
