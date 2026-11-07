package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

// taskProvider drives the parent: it starts an execution task, then (on the next Start, after the
// task result row) collects the child's result.
type taskProvider struct {
	mu        sync.Mutex
	requests  []protocol.ProviderRequest
	waitChild func(context.Context, string) error
}

func (p *taskProvider) Model() string { return "task-parent" }

func (p *taskProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
	last := req.Messages[len(req.Messages)-1]
	if last.Role == "tool" {
		var started struct {
			Child ExecutionTask `json:"child"`
		}
		if err := json.Unmarshal([]byte(last.Content), &started); err != nil || started.Child.SessionID == "" {
			return protocol.ProviderResponse{}, fmt.Errorf("task start failed: %s", last.Content)
		}
		if err := p.waitChild(ctx, started.Child.SessionID); err != nil {
			return protocol.ProviderResponse{}, err
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "worker finished"}, FinishReason: "stop"}, nil
	}
	if len(p.requests) == 1 {
		args, _ := json.Marshal(map[string]any{"objective": "compute 2+2 in the shared kernel", "agent": "worker"})
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant",
			ToolCalls: []protocol.ToolCall{{ID: "task", Name: "start_task", Arguments: args}}}, FinishReason: "tool_calls"}, nil
	}
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "collected: child computed 10 after appending 4 to the shared values"}, FinishReason: "stop"}, nil
}

// evalChildProvider is the execution child: it discovers the granted kernel, executes one cell in it
// that uses the parent's retained variable, parses the real eval result, and only then reports. A
// denied or failed eval must never look like a computed result.
type evalChildProvider struct {
	mu        sync.Mutex
	step      int
	kernel    string
	evals     int
	evaluated bool
	failure   string
}

func (p *evalChildProvider) Model() string { return "eval-child" }

func (p *evalChildProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	last := req.Messages[len(req.Messages)-1]
	switch p.step {
	case 3:
		if last.Role != "tool" {
			return protocol.ProviderResponse{}, fmt.Errorf("child completion rejected: %s", last.Content)
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child computed 10 after appending 4 to the shared values"}, FinishReason: "stop"}, nil
	case 0:
		p.step = 1
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant",
			ToolCalls: []protocol.ToolCall{{ID: "sessions", Name: "eval_sessions", Arguments: json.RawMessage(`{}`)}}}, FinishReason: "tool_calls"}, nil
	case 1:
		p.step = 2
		var sessions []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(last.Content), &sessions); err != nil || len(sessions) == 0 {
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "no shared kernel was granted"}, FinishReason: "stop"}, nil
		}
		p.kernel = sessions[0].ID
		// The cell reads and mutates the variable the parent seeded in the shared kernel, so an
		// isolated `print(2+2)` (or a denied eval) cannot produce this output.
		code := "shared_values.append(4)\nprint(sum(shared_values))"
		args, _ := json.Marshal(map[string]any{"session_id": p.kernel, "cell_id": "c1", "code": code})
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant",
			ToolCalls: []protocol.ToolCall{{ID: "eval", Name: "eval", Arguments: args}}}, FinishReason: "tool_calls"}, nil
	default:
		var result evalapi.Result
		if err := json.Unmarshal([]byte(last.Content), &result); err != nil {
			p.failure = "the eval result was not a JSON evalapi.Result: " + err.Error()
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child failed: " + p.failure}, FinishReason: "stop"}, nil
		}
		p.evals++
		if result.Status != "completed" {
			p.failure = "the eval did not complete: status=" + result.Status + " error=" + result.Error
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child failed: " + p.failure}, FinishReason: "stop"}, nil
		}
		if !strings.Contains(result.Stdout, "10") {
			p.failure = "the retained shared value was not used: stdout=" + result.Stdout
			return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "child failed: " + p.failure}, FinishReason: "stop"}, nil
		}
		p.evaluated = true
		p.step = 3
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "finish-work", Name: "work_unit_update", Arguments: json.RawMessage(`{"id":"task","status":"completed","summary":"child computed 10 after appending 4 to the shared values"}`)}}}, FinishReason: "tool_calls"}, nil
	}
}

// reviewProvider answers the independent review with a passed JSON verdict and reports usage.
type reviewProvider struct {
	mu         sync.Mutex
	calls      int
	sawOutcome bool
}

func (p *reviewProvider) Model() string { return "reviewer" }
func (p *reviewProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.calls++
	// The reviewer's answer may be predetermined, but the input it sees must carry the evaluated
	// outcome: the run's final answer, not the model's claim alone.
	for _, message := range req.Messages {
		if strings.Contains(message.Content, "child computed 10") && strings.Contains(message.Content, "shared values") {
			p.sawOutcome = true
		}
	}
	p.mu.Unlock()
	return protocol.ProviderResponse{
		Message: protocol.Message{Role: "assistant", Content: `{"passed":true,"findings":[]}`},
		Usage:   protocol.Usage{PromptTokens: 40, CompletionTokens: 10, TotalTokens: 50},
	}, nil
}

func openEvalEngine(t *testing.T) *Engine {
	t.Helper()
	python := os.Getenv("LCX_TEST_PYTHON")
	if strings.TrimSpace(python) == "" {
		t.Skip("LCX_TEST_PYTHON is not set: the shared-kernel scenario needs a Python runtime")
	}
	engine, err := Open(Options{Workspace: t.TempDir(), Eval: &EvalOptions{Python: python}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

func pythonRuntime(t *testing.T, engine *Engine) evalapi.Manifest {
	t.Helper()
	runtimes, err := engine.EvalRuntimes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, manifest := range runtimes {
		if manifest.Language == "python" || strings.Contains(strings.ToLower(manifest.ID), "python") {
			return manifest
		}
	}
	t.Fatalf("no python runtime: %+v", runtimes)
	return evalapi.Manifest{}
}

// A host claim carries the status, evidence locators and fingerprint it supplies: a confirmation is
// recorded only with its proof, and it is never silently downgraded to a proposal.
func TestAddSessionClaimRecordsSuppliedProofAndRefusesConfirmationWithoutIt(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "host claims"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.AddSessionClaim(ctx, handle.ID, ClaimView{Kind: "contract", Statement: "report is in USD",
		Required: true, Status: "confirmed"}); err == nil {
		t.Fatal("a host confirmation without evidence or fingerprint was recorded")
	}
	if claims, err := engine.SessionClaims(ctx, handle.ID); err != nil || len(claims) != 0 {
		t.Fatalf("a refused claim was persisted: %+v %v", claims, err)
	}
	added, err := engine.AddSessionClaim(ctx, handle.ID, ClaimView{Kind: "contract", Statement: "report is in USD",
		Required: true, Status: "confirmed", Evidence: []string{"check:units@eval:1"}, Fingerprint: "head-a@local"})
	if err != nil {
		t.Fatal(err)
	}
	if added.Status != "confirmed" || added.Fingerprint != "head-a@local" || len(added.Evidence) != 1 {
		t.Fatalf("the host status, evidence or fingerprint was dropped: %+v", added)
	}
	state, err := engine.loadTaskState(ctx, handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.ClaimCoverage("head-a@local"); got.Confirmed != 1 || got.Stale != 0 {
		t.Fatalf("coverage at the recorded fingerprint=%+v", got)
	}
	if got := state.ClaimCoverage("head-b@local"); got.Stale != 1 || got.Confirmed != 0 {
		t.Fatalf("coverage after the workspace moved=%+v", got)
	}
	if got := state.ClaimCoverage(""); got.Stale != 1 || got.Confirmed != 0 {
		t.Fatalf("coverage with an unknown fingerprint=%+v", got)
	}
}

// Scenario C: a parent starts an execution task with an explicit host grant of its own kernel. The
// child's own run discovers the granted kernel, executes in it through the real eval tool, and reports;
// the parent collects the task result on its next Start; the run's single independent review decides
// once and is metered.
func TestScenarioCExecutionChildSharedKernelAndReview(t *testing.T) {
	useGlobalConfig(t, catalogWithAgents(`{"worker": {"models": [{"model": "test/b63"}]}}`))
	engine := openEvalEngine(t)
	initGitRepo(t, engine.workspace)
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()

	parentProvider := &taskProvider{waitChild: func(ctx context.Context, id string) error {
		deadline := time.NewTimer(10 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for supervisor.Active(id) {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-deadline.C:
				return fmt.Errorf("execution child did not finish")
			case <-tick.C:
			}
		}
		return nil
	}}
	parent := newParentSession(t, engine, parentProvider)
	childProvider := &evalChildProvider{}
	bindRef(engine, "b63", childProvider)
	reviewer := &reviewProvider{}
	reviewRef := bind(engine, reviewer)

	ctx := context.Background()
	manifest := pythonRuntime(t, engine)
	kernel, err := engine.CreateEvalSession(ctx, parent, manifest.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The parent seeds the retained variable in its own kernel before the child is granted access.
	seeded, err := engine.ExecuteEval(ctx, parent, evalapi.Request{SessionID: kernel.ID, CellID: "seed",
		Code: "shared_values = [1, 2, 3]\nprint(len(shared_values))"})
	if err != nil || seeded.Status != "completed" {
		t.Fatalf("seeding the shared kernel: %+v %v", seeded, err)
	}
	yes := true
	if _, err := supervisor.Start(ctx, parent, AgentOptions{
		Input: "delegate the computation", MaxSteps: 8,
		Delegation:     DelegationIsolated,
		Retry:          &config.Retry{Disabled: true},
		ChildExecution: &ChildExecutionOptions{EvalEnabled: &yes, SharedKernel: kernel.ID, SharedKernelLeaseSeconds: 300},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, parent, 30*time.Second)

	relations, err := engine.store.ChildRelations(ctx, parent, 10)
	if err != nil {
		t.Fatal(err)
	}
	var childID string
	for _, relation := range relations {
		if relation.Kind == RelationExecution {
			childID = relation.ChildSessionID
		}
	}
	if childID == "" {
		_, state, _ := engine.Session(ctx, parent)
		messages, _ := engine.store.RecentMessages(ctx, parent, 10)
		t.Fatalf("no execution child: relations=%+v parent=%+v messages=%+v", relations, state, messages)
	}
	waitUntilIdle(t, supervisor, childID, 30*time.Second)

	_, child, err := engine.Session(ctx, childID)
	if err != nil {
		t.Fatal(err)
	}
	final := ""
	if child.Final != nil {
		final = *child.Final
	}
	if child.Status != "completed" || !strings.Contains(final, "child computed 10") {
		t.Fatalf("child=%+v", child)
	}
	childProvider.mu.Lock()
	evals, evaluated, failure := childProvider.evals, childProvider.evaluated, childProvider.failure
	childProvider.mu.Unlock()
	if evals != 1 || !evaluated {
		t.Fatalf("the child did not verify its eval result: evals=%d evaluated=%v failure=%q", evals, evaluated, failure)
	}
	if failure != "" {
		t.Fatalf("child eval failed: %s", failure)
	}
	// The parent observes the child's mutation in the same kernel it owns.
	observed, err := engine.ExecuteEval(ctx, parent, evalapi.Request{SessionID: kernel.ID, CellID: "observe",
		Code: "print(sum(shared_values))"})
	if err != nil || observed.Status != "completed" || !strings.Contains(observed.Stdout, "10") {
		t.Fatalf("the parent did not observe the child's mutation: %+v %v", observed, err)
	}
	// The child's transcript holds the real eval result, not a denial.
	rows, err := engine.store.RecentMessages(ctx, childID, 100)
	if err != nil {
		t.Fatal(err)
	}
	foundEval := false
	for _, row := range rows {
		var message protocol.Message
		if json.Unmarshal(row.JSON, &message) != nil {
			continue
		}
		if message.Role == "tool" && message.Name == "eval" && strings.Contains(message.Content, `"status":"completed"`) {
			foundEval = true
		}
	}
	if !foundEval {
		t.Fatal("the child's eval call did not complete in the shared kernel")
	}

	// The parent collects the child's evaluated result on its next Start; the review runs on that
	// run, where the evaluated outcome is in its input context.
	if _, err := supervisor.Start(ctx, parent, AgentOptions{
		Input: "collect the task result", MaxSteps: 4,
		Review: &ReviewOptions{Enabled: true, Model: reviewRef},
	}); err != nil {
		t.Fatal(err)
	}
	waitUntilIdle(t, supervisor, parent, 30*time.Second)
	rows, err = engine.store.RecentMessages(ctx, parent, 100)
	if err != nil {
		t.Fatal(err)
	}
	delivered := false
	for _, row := range rows {
		var message protocol.Message
		if json.Unmarshal(row.JSON, &message) == nil && message.Origin == protocol.OriginTaskResult &&
			strings.Contains(message.Content, "child computed 10") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("the parent never received the child's evaluated result")
	}

	// Exactly one review for the run that enabled it, recorded durably with its usage.
	reviewer.mu.Lock()
	calls, sawOutcome := reviewer.calls, reviewer.sawOutcome
	reviewer.mu.Unlock()
	if calls != 1 {
		t.Fatalf("review calls=%d", calls)
	}
	if !sawOutcome {
		t.Fatal("the reviewer's input did not carry the evaluated outcome")
	}
	verification, err := engine.SessionVerification(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if verification.Review == nil || !verification.Review.Passed || verification.Review.Model != string(reviewRef) {
		t.Fatalf("verification=%+v", verification)
	}
}

// The reviewer is opt-in: a run without ReviewOptions makes no review request.
func TestIndependentReviewIsOffByDefault(t *testing.T) {
	engine := openTestEngine(t)
	reviewer := &reviewProvider{}
	reviewRef := bind(engine, reviewer)
	p := completingProvider("done")
	parent := newParentSession(t, engine, p)
	ctx := context.Background()
	if _, err := engine.runAgentWith(ctx, parent, p, AgentOptions{
		Input: "answer", MaxSteps: 2, Review: &ReviewOptions{Enabled: false, Model: reviewRef},
	}); err != nil {
		t.Fatal(err)
	}
	reviewer.mu.Lock()
	defer reviewer.mu.Unlock()
	if reviewer.calls != 0 {
		t.Fatalf("a disabled review made %d requests", reviewer.calls)
	}
}

// A child's eval grant is host-only and off by default: the parent's own EvalEnabled never grants it,
// and the parent's ChildExecution override does.
func TestChildEvalGrantIsHostOnly(t *testing.T) {
	parent := AgentOptions{EvalEnabled: true}
	if enabled, kernel, _ := childEvalGrant(parent); enabled || kernel != "" {
		t.Fatal("a child must not inherit the parent's EvalEnabled")
	}
	no := false
	if enabled, _, _ := childEvalGrant(AgentOptions{ChildExecution: &ChildExecutionOptions{EvalEnabled: &no}}); enabled {
		t.Fatal("an explicit false grant must stay off")
	}
	yes := true
	enabled, kernel, lease := childEvalGrant(AgentOptions{ChildExecution: &ChildExecutionOptions{
		EvalEnabled: &yes, SharedKernel: "eval-1", SharedKernelLeaseSeconds: 7200}})
	if !enabled || kernel != "eval-1" || lease != time.Hour {
		t.Fatalf("grant=%v %q %v", enabled, kernel, lease)
	}
}
