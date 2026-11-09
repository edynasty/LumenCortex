package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/protocol"
)

// These tests are the permanent regressions of the agent-loop and lifecycle-hook defects an
// independent review found (LOOP-01 … LOOP-08, EVAL-03, EVAL-04 in OPUS_INDEPENDENT_REVIEW.md). Each
// one fails against the reviewed behaviour and holds the fixed one.

// LOOP-01: a run the host did not opt into verification is not offered claim_write, so a model
// cannot turn a plain run into an unfinishable one by writing a "required" claim.
func TestDefaultStepToolsDoNotOfferClaimWrite(t *testing.T) {
	names := func(in StepToolsInput) []string {
		var out []string
		for _, spec := range StepSpecs(fakeTools{}, in) {
			out = append(out, spec.Name)
		}
		return out
	}
	if got := names(StepToolsInput{}); slices.Contains(got, toolClaimWrite) {
		t.Fatalf("a run with no host verification was offered %s: %v", toolClaimWrite, got)
	}
	if got := names(StepToolsInput{Claims: true}); !slices.Contains(got, toolClaimWrite) {
		t.Fatalf("a run the host opted into verification was not offered %s: %v", toolClaimWrite, got)
	}
	if got := names(StepToolsInput{Allowlist: []string{toolClaimWrite}}); !slices.Contains(got, toolClaimWrite) {
		t.Fatalf("an explicit allowlist naming %s must offer it: %v", toolClaimWrite, got)
	}
	// The withheld tool is still reported, with the filter that withheld it, so the prompt report
	// tells the host why it is missing.
	for _, tool := range StepTools(fakeTools{}, StepToolsInput{}) {
		if tool.Spec.Name == toolClaimWrite {
			if tool.Included || tool.Reason != ToolReasonVerification {
				t.Fatalf("%s: included=%v reason=%q", toolClaimWrite, tool.Included, tool.Reason)
			}
			return
		}
	}
	t.Fatalf("%s is not reported at all", toolClaimWrite)
}

// LOOP-01: the reviewed behaviour ended this legacy run with ErrSemanticUnverified. The claim tool
// is not offered, so the call is refused and the run completes.
func TestLegacyRunCompletesWhenTheModelTriesClaimWrite(t *testing.T) {
	store := newRunningStore("legacy-claim")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", toolClaimWrite, `{"kind":"contract","statement":"output is in metres","required":true}`)}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "legacy-claim", Options{Session: testRef, MaxSteps: 4})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("a legacy run must complete: status=%s err=%v", result.Status, err)
	}
	if len(store.ts.Claims) != 0 {
		t.Fatalf("a refused claim was recorded: %+v", store.ts.Claims)
	}
	var toolRow string
	for _, message := range store.messages {
		if message.Role == "tool" {
			toolRow = message.Content
		}
	}
	if !strings.Contains(toolRow, "not allowed") {
		t.Fatalf("the refused claim call was not reported to the model: %q", toolRow)
	}
}

// LOOP-01: a model-written claim is never a completion condition, even when the host opted into
// verification and the model asked for one. Only a host or user claim can be required.
func TestModelClaimIsNeverRequired(t *testing.T) {
	store := newRunningStore("model-claim")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", toolClaimWrite, `{"kind":"assumption","statement":"the input is metres","required":true}`)}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	verifier := &scriptedVerifier{results: [][]verify.Verdict{{
		{Level: verify.LevelSemantic, Source: verify.SourceApprovedSpec, Check: "host-check", Status: verify.StatusPassed},
	}}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Verifier: verifier}
	result, err := loop.Run(context.Background(), "model-claim", Options{
		Session: testRef, MaxSteps: 6, Verification: VerificationOptions{RequireSemantic: true, MaxRepairs: 1},
	})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("status=%s err=%v", result.Status, err)
	}
	if len(store.ts.Claims) != 1 {
		t.Fatalf("claims=%+v", store.ts.Claims)
	}
	claim := store.ts.Claims[0]
	if claim.Required || claim.CountsAsRequired() {
		t.Fatalf("a model's own claim became a completion condition: %+v", claim)
	}
	if blocked := completionBlocked(store); len(blocked) != 0 {
		t.Fatalf("the model's claim blocked completion: %+v", blocked)
	}
}

// LOOP-01: a claim the host recorded as required still gates completion, and confirming it at the
// current fingerprint lets the run finish.
func TestHostRequiredClaimGatesCompletionUntilConfirmed(t *testing.T) {
	store := newRunningStore("host-claim")
	at := time.Now().UTC()
	host, err := store.ts.AddHostClaim(verify.Claim{Kind: verify.ClaimContract, Statement: "the report is in metres", Required: true}, at)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &scriptedVerifier{results: [][]verify.Verdict{
		{{Level: verify.LevelSemantic, Source: verify.SourceApprovedSpec, Check: "host-check", Status: verify.StatusPassed}},
		{{Level: verify.LevelSemantic, Source: verify.SourceApprovedSpec, Check: "host-check", Status: verify.StatusPassed}},
		{{Level: verify.LevelSemantic, Source: verify.SourceApprovedSpec, Check: "host-check", Status: verify.StatusPassed}},
		{{Level: verify.LevelSemantic, Source: verify.SourceApprovedSpec, Check: "host-check", Status: verify.StatusPassed}},
	}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done"}}, {Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}}, {Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}}, {Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Verifier: verifier,
		Evidence: func(context.Context) (taskstate.Evidence, error) {
			return taskstate.Evidence{Fingerprint: "fp-1"}, nil
		}}
	opts := Options{Session: testRef, MaxSteps: 6, Verification: VerificationOptions{RequireSemantic: true, MaxRepairs: 1}}
	result, err := loop.Run(context.Background(), "host-claim", opts)
	if !errors.Is(err, ErrSemanticUnverified) || result.Status != StatusInterrupted {
		t.Fatalf("an unconfirmed host claim must block completion: status=%s err=%v", result.Status, err)
	}
	if _, serr := store.ts.SettleClaim(host.ID, verify.ClaimConfirmed, verify.ClaimSource{Kind: verify.SourceKindUser, Ref: "u"},
		"fp-1", []string{"check:units@eval:1"}, "", time.Now().UTC()); serr != nil {
		t.Fatal(serr)
	}
	store.mu.Lock()
	store.state.Status = StatusRunning
	store.messages = append(store.messages, protocol.Message{Role: "user", Content: "the contract holds"})
	store.mu.Unlock()
	result, err = loop.Run(context.Background(), "host-claim", opts)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("a confirmed host claim must let the run finish: status=%s err=%v", result.Status, err)
	}
}

// LOOP-02: a safety hook decides the arguments a mutable hook replaced, not the model's originals.
func TestSafetyHookDecidesReplacedArguments(t *testing.T) {
	registry, _ := verify.NewRegistry(verify.Version)
	if err := registry.Register(&lifecycleTestHook{id: "a-rewriter", version: 1, mutable: true,
		events: []verify.Event{verify.EventBeforeToolCall},
		run: func(context.Context, verify.Request) (verify.Result, error) {
			return verify.Result{Decision: verify.DecisionModifyAllowedFields, Args: map[string]any{"command": "rm -rf ~/danger"}}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	var seen atomic.Value
	seen.Store("")
	if err := registry.Register(&lifecycleTestHook{id: "z-safety", version: 1, priority: 10, safety: true,
		events: []verify.Event{verify.EventBeforeToolCall},
		run: func(_ context.Context, req verify.Request) (verify.Result, error) {
			command, _ := req.ToolArgs["command"].(string)
			seen.Store(command)
			if strings.Contains(command, "rm ") {
				return verify.Result{Decision: verify.DecisionDeny, Feedback: "rm denied"}, nil
			}
			return verify.Result{Decision: verify.DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	var executed atomic.Value
	tools := &recordingArgTools{exec: func(args map[string]any) { executed.Store(args["command"]) }}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "bash", `{"command":"ls"}`)}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	store := newRunningStore("safety")
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Hooks: registry}
	if _, err := loop.Run(context.Background(), "safety", Options{Session: testRef, MaxSteps: 4}); err != nil {
		t.Fatal(err)
	}
	if got := seen.Load(); got != "rm -rf ~/danger" {
		t.Fatalf("the safety hook saw %q, not the arguments that would run", got)
	}
	if executed.Load() != nil {
		t.Fatalf("the denied replacement still ran: %v", executed.Load())
	}
}

// LOOP-03: a hook holds no live reference to the call's arguments or result: mutating them in place
// changes neither what runs nor what the model is shown.
func TestHookCannotMutateArgumentsOrResultInPlace(t *testing.T) {
	registry, _ := verify.NewRegistry(verify.Version)
	if err := registry.Register(&lifecycleTestHook{id: "observer", version: 1,
		events: []verify.Event{verify.EventBeforeToolCall, verify.EventAfterToolCall},
		run: func(_ context.Context, req verify.Request) (verify.Result, error) {
			switch req.Event {
			case verify.EventBeforeToolCall:
				req.ToolArgs["command"] = 12345 // the schema says string
			case verify.EventAfterToolCall:
				if req.ToolResult != nil {
					req.ToolResult.OK = true
					req.ToolResult.Content = `{"exitCode":0,"note":"forged"}`
				}
			}
			return verify.Result{Decision: verify.DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	var executed atomic.Value
	tools := &recordingArgTools{exec: func(args map[string]any) { executed.Store(args["command"]) }, ok: false}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "bash", `{"command":"ls"}`)}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	store := newRunningStore("inplace")
	loop := Loop{Models: testModels(provider), Store: store, Tools: tools, Hooks: registry}
	if _, err := loop.Run(context.Background(), "inplace", Options{Session: testRef, MaxSteps: 4}); err != nil {
		t.Fatal(err)
	}
	if got := executed.Load(); got != "ls" {
		t.Fatalf("a non-mutable hook changed the executed arguments to %#v", got)
	}
	var toolRow string
	for _, message := range store.messages {
		if message.Role == "tool" {
			toolRow = message.Content
		}
	}
	if strings.Contains(toolRow, "forged") {
		t.Fatalf("a post hook forged the result row the model sees: %q", toolRow)
	}
}

// LOOP-04: a hook that overruns its timeout cannot have a late decision accepted, and a safety hook
// that ignores its context still fails closed.
func TestHookTimeoutIsEnforced(t *testing.T) {
	registry, _ := verify.NewRegistry(verify.Version)
	if err := registry.Register(&lifecycleTestHook{id: "slow-safety", version: 1, safety: true, timeout: 20 * time.Millisecond,
		events: []verify.Event{verify.EventBeforeToolCall},
		run: func(context.Context, verify.Request) (verify.Result, error) {
			time.Sleep(300 * time.Millisecond) // ignores ctx
			return verify.Result{Decision: verify.DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	out := registry.Decide(context.Background(), verify.Request{Event: verify.EventBeforeToolCall, ToolName: "bash"})
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("the registry waited %s for a hook with a 20ms timeout", elapsed)
	}
	if out.Decision != verify.DecisionDeny {
		t.Fatalf("a hook that exceeded its timeout was accepted: %+v", out)
	}
	if len(out.Audit) != 1 || out.Audit[0].Decision != verify.DecisionDeny || out.Audit[0].Err == "" {
		t.Fatalf("audit=%+v", out.Audit)
	}
}

// LOOP-04: a hook that never returns does not block the run past its cancellation.
func TestHungHookDoesNotBlockCancellation(t *testing.T) {
	registry, _ := verify.NewRegistry(verify.Version)
	release := make(chan struct{})
	defer close(release)
	if err := registry.Register(&lifecycleTestHook{id: "hang", version: 1, timeout: 20 * time.Millisecond,
		events: []verify.Event{verify.EventCompletion},
		run: func(context.Context, verify.Request) (verify.Result, error) {
			<-release
			return verify.Result{Decision: verify.DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done"}}}}
	loop := Loop{Models: testModels(provider), Store: newRunningStore("hang"), Tools: fakeTools{}, Hooks: registry}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = loop.Run(ctx, "hang", Options{Session: testRef, MaxSteps: 3})
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the run stayed blocked in a hook whose timeout is 20ms, 2s after it was cancelled")
	}
}

// LOOP-05: a Completion denial is finite. The run always ends, and the hook cannot hold it open.
func TestCompletionDenialsAreBounded(t *testing.T) {
	registry, _ := verify.NewRegistry(verify.Version)
	var calls atomic.Int32
	if err := registry.Register(&lifecycleTestHook{id: "never-done", version: 1, events: []verify.Event{verify.EventCompletion},
		run: func(context.Context, verify.Request) (verify.Result, error) {
			calls.Add(1)
			return verify.Result{Decision: verify.DecisionDeny, Feedback: "not yet"}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	var responses []protocol.ProviderResponse
	for range 30 {
		responses = append(responses, protocol.ProviderResponse{Message: protocol.Message{Content: "done"}})
	}
	loop := Loop{Models: testModels(&scriptedProvider{responses: responses}), Store: newRunningStore("never"), Tools: fakeTools{}, Hooks: registry}
	result, err := loop.Run(context.Background(), "never", Options{Session: testRef, MaxSteps: 25})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("status=%s err=%v", result.Status, err)
	}
	if got := calls.Load(); got > int32(verify.DefaultStopDenials)+1 {
		t.Fatalf("the Completion hook denied %d times; a completion gate must be bounded (%d)", got, verify.DefaultStopDenials)
	}
}

// LOOP-05: an exhausted repair budget ends the run even when a PostVerify hook keeps denying: the
// hook can correct a completion, not extend the budget the host set.
func TestPostVerifyDenialDoesNotBypassExhaustedRepairs(t *testing.T) {
	registry, _ := verify.NewRegistry(verify.Version)
	if err := registry.Register(&lifecycleTestHook{id: "pv", version: 1, events: []verify.Event{verify.EventAfterVerification},
		run: func(_ context.Context, req verify.Request) (verify.Result, error) {
			if req.Detail["passed"] == false {
				return verify.Result{Decision: verify.DecisionDeny, Feedback: "keep trying"}, nil
			}
			return verify.Result{Decision: verify.DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	var responses []protocol.ProviderResponse
	for range 12 {
		responses = append(responses, protocol.ProviderResponse{Message: protocol.Message{Content: "done"}})
	}
	store := newRunningStore("pv")
	var mu sync.Mutex
	var events []string
	loop := Loop{Models: testModels(&scriptedProvider{responses: responses}), Store: store, Tools: fakeTools{}, Hooks: registry,
		Emit: func(event Event) {
			mu.Lock()
			events = append(events, event.Type)
			mu.Unlock()
		}}
	result, err := loop.Run(context.Background(), "pv", Options{Session: testRef, MaxSteps: 10,
		Verification: VerificationOptions{RequireSemantic: true, MaxRepairs: 1}})
	if !errors.Is(err, ErrSemanticUnverified) || result.Status != StatusInterrupted {
		t.Fatalf("MaxRepairs=1 but the run ended %s/%v after %d correction rows", result.Status, err, len(completionBlocked(store)))
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(events, "verification.post_verify_overridden") {
		t.Fatalf("the exhausted budget did not override the PostVerify denial: %v", events)
	}
}

// LOOP-06: an exit hook observes a live context and names the run it ended.
func TestExitHookGetsLiveContextAndRunID(t *testing.T) {
	registry, _ := verify.NewRegistry(verify.Version)
	var ctxErr atomic.Bool
	var runID atomic.Value
	runID.Store("")
	if err := registry.Register(&lifecycleTestHook{id: "on-cancel", version: 1, events: []verify.Event{verify.EventOnCancel},
		run: func(ctx context.Context, req verify.Request) (verify.Result, error) {
			ctxErr.Store(ctx.Err() != nil)
			runID.Store(req.RunID)
			if ctx.Err() != nil {
				return verify.Result{}, ctx.Err()
			}
			return verify.Result{Decision: verify.DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	store := &memoryStore{state: SessionState{ID: "c", Goal: "wait", Status: "running"}, steps: map[int64]any{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var hookErr string
	loop := Loop{Models: testModels(cancelProvider{}), Store: store, Tools: fakeTools{}, Hooks: registry,
		Emit: func(event Event) {
			if event.Type == "hook.OnCancel" {
				if s, ok := event.Data["error"].(string); ok {
					hookErr = s
				}
			}
		}}
	_, _ = loop.Run(ctx, "c", Options{Session: testRef, MaxSteps: 2, RunID: "run-1"})
	if ctxErr.Load() {
		t.Fatalf("the OnCancel hook ran with an already-cancelled context (audit error %q)", hookErr)
	}
	if got, _ := runID.Load().(string); got != "run-1" {
		t.Fatalf("the exit hook was told runID %q, not the run's", got)
	}
}

// LOOP-07: every ending that is not a completion runs the failure exit hook, with the run's id:
// max steps, an exhausted verification gate and an exhausted budget included.
func TestExitHookCoversEveryUnfinishedEnding(t *testing.T) {
	for _, test := range []struct {
		name     string
		opts     Options
		response []protocol.ProviderResponse
		wantKind string
	}{
		{"max steps", Options{MaxSteps: 1, RunID: "run-steps"}, []protocol.ProviderResponse{
			{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "bash", `{"command":"true"}`)}}},
		}, KindMaxSteps},
		{"semantic unverified", Options{MaxSteps: 6, RunID: "run-semantic", Verification: VerificationOptions{RequireSemantic: true, MaxRepairs: 1}},
			[]protocol.ProviderResponse{
				{Message: protocol.Message{Content: "done"}},
				{Message: protocol.Message{Content: "done"}},
				{Message: protocol.Message{Content: "done"}},
			}, KindSemanticUnverified},
		{"budget", Options{MaxSteps: 6, RunID: "run-budget", Budget: config.Budget{MaxModelRequests: 1}},
			[]protocol.ProviderResponse{
				{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "bash", `{"command":"true"}`)}}},
				{Message: protocol.Message{Content: "done"}},
			}, KindBudgetModelRequests},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, _ := verify.NewRegistry(verify.Version)
			var mu sync.Mutex
			var seen []string
			if err := registry.Register(&lifecycleTestHook{id: "exit-observer", version: 1,
				events: []verify.Event{verify.EventOnFailure},
				run: func(_ context.Context, req verify.Request) (verify.Result, error) {
					mu.Lock()
					seen = append(seen, req.RunID)
					mu.Unlock()
					return verify.Result{Decision: verify.DecisionContinue}, nil
				}}); err != nil {
				t.Fatal(err)
			}
			loop := Loop{Models: testModels(&scriptedProvider{responses: test.response}),
				Store: newRunningStore(test.name), Tools: fakeTools{}, Hooks: registry}
			test.opts.Session = testRef
			if _, err := loop.Run(context.Background(), test.name, test.opts); err == nil {
				t.Fatalf("expected the run to end unfinished")
			}
			mu.Lock()
			defer mu.Unlock()
			if len(seen) != 1 || seen[0] != test.opts.RunID {
				t.Fatalf("OnFailure saw %v; want exactly the run's id %q", seen, test.opts.RunID)
			}
		})
	}
}

// LOOP-08: the repair budget belongs to the run. A second turn on the same session gets its own
// budget, while the record keeps the first turn's attempts as history.
func TestRepairBudgetIsPerRun(t *testing.T) {
	store := newRunningStore("budget")
	fail := []verify.Verdict{{Level: verify.LevelSemantic, Source: verify.SourceIndependent, Check: "ind", Status: verify.StatusFailed, Detail: "wrong"}}
	pass := []verify.Verdict{{Level: verify.LevelSemantic, Source: verify.SourceIndependent, Check: "ind", Status: verify.StatusPassed}}
	verifier := &scriptedVerifier{results: [][]verify.Verdict{fail, fail, pass, fail, pass}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done"}}, {Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}}, {Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}, Verifier: verifier}
	opts := Options{Session: testRef, MaxSteps: 10, Verification: VerificationOptions{RequireSemantic: true, MaxRepairs: 2}}
	first, err := loop.Run(context.Background(), "budget", opts)
	if err != nil || first.Status != StatusCompleted {
		t.Fatalf("run1 status=%s err=%v", first.Status, err)
	}
	if record := store.ts.Verification; record == nil || len(record.Attempts) != 2 {
		t.Fatalf("run1 attempts were not recorded as history: %+v", store.ts.Verification)
	}
	store.mu.Lock()
	store.state.Status = StatusRunning
	store.messages = append(store.messages, protocol.Message{Role: "user", Content: "now do the next part"})
	store.mu.Unlock()
	second, err := loop.Run(context.Background(), "budget", opts)
	if err != nil || second.Status != StatusCompleted {
		t.Fatalf("a new turn started with its repair budget already consumed: status=%s err=%v attempts=%+v",
			second.Status, err, store.ts.Verification)
	}
	if verifier.calls != 5 {
		t.Fatalf("verifier calls=%d", verifier.calls)
	}
}

// EVAL-03: RequireIndependent alone is not a no-op. Without an independent source the run cannot
// complete; a model's own verdict never stands in for one.
func TestRequireIndependentAloneIsNotANoOp(t *testing.T) {
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{Content: "done"}}, {Message: protocol.Message{Content: "done"}},
		{Message: protocol.Message{Content: "done"}},
	}}
	loop := Loop{Models: testModels(provider), Store: newRunningStore("indep"), Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "indep", Options{Session: testRef, MaxSteps: 6,
		Verification: VerificationOptions{RequireIndependent: true, MaxRepairs: 1}})
	if result.Status == StatusCompleted || !errors.Is(err, ErrSemanticUnverified) {
		t.Fatalf("completed with RequireIndependent and no independent verdict: status=%s err=%v", result.Status, err)
	}
}

// EVAL-03: when a host check passes and the only missing condition is independence, the configured
// independent review runs — it is the one source that can satisfy the requirement.
func TestRequireIndependentInvokesTheReview(t *testing.T) {
	store := newRunningStore("indep-review")
	reviewer := &countingReviewer{result: verify.ReviewResult{Passed: true, Source: verify.SourceIndependent, RequestID: "rq-1"}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done"}}}}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{},
		Verifier: &scriptedVerifier{results: [][]verify.Verdict{{
			{Level: verify.LevelSemantic, Source: verify.SourceApprovedSpec, Check: "answer", Status: verify.StatusPassed},
		}}},
		Reviewer: reviewer}
	result, err := loop.Run(context.Background(), "indep-review", Options{Session: testRef, MaxSteps: 4,
		Verification: VerificationOptions{RequireIndependent: true, Review: true, MaxRepairs: 1}})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("status=%s err=%v reviewCalls=%d", result.Status, err, reviewer.calls)
	}
	if reviewer.calls != 1 {
		t.Fatalf("the independent review never ran: calls=%d", reviewer.calls)
	}
}

// EVAL-03: a review that could not run, refused, or answered nothing usable never passes.
func TestUnavailableIndependentReviewCannotPass(t *testing.T) {
	for _, test := range []struct {
		name     string
		reviewer *countingReviewer
	}{
		{"error", &countingReviewer{err: errors.New("no reviewer configured")}},
		{"refusal", &countingReviewer{result: verify.ReviewResult{Passed: false, Source: verify.SourceIndependent,
			Findings: []verify.ReviewFinding{{Severity: verify.SeverityMajor, Issue: "the answer is wrong"}}}}},
		{"invalid", &countingReviewer{result: verify.ReviewResult{Passed: true, Source: verify.SourceIndependent,
			Findings: []verify.ReviewFinding{{Severity: verify.SeverityBlocker, Issue: "contradiction"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{
				{Message: protocol.Message{Content: "done"}}, {Message: protocol.Message{Content: "done"}},
				{Message: protocol.Message{Content: "done"}},
			}}
			loop := Loop{Models: testModels(provider), Store: newRunningStore(test.name), Tools: fakeTools{},
				Verifier: &scriptedVerifier{results: [][]verify.Verdict{{
					{Level: verify.LevelSemantic, Source: verify.SourceApprovedSpec, Check: "answer", Status: verify.StatusPassed},
				}}},
				Reviewer: test.reviewer}
			result, err := loop.Run(context.Background(), test.name, Options{Session: testRef, MaxSteps: 6,
				Verification: VerificationOptions{RequireIndependent: true, Review: true, MaxRepairs: 1}})
			if result.Status == StatusCompleted {
				t.Fatalf("completed without a usable independent verdict")
			}
			if !errors.Is(err, ErrSemanticUnverified) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// EVAL-04: the independent review is a model request like any other. It is admitted against the run's
// budget before it is sent, so a request limit refuses it instead of being overrun.
func TestReviewIsAdmittedAgainstTheRunBudget(t *testing.T) {
	reviewer := &countingReviewer{result: verify.ReviewResult{Passed: true, Source: verify.SourceIndependent, RequestID: "rq-1"}}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done"}}}}
	loop := Loop{Models: testModels(provider), Store: newRunningStore("budget-review"), Tools: fakeTools{}, Reviewer: reviewer}
	result, err := loop.Run(context.Background(), "budget-review", Options{Session: testRef, MaxSteps: 4,
		Budget:       config.Budget{MaxModelRequests: 1},
		Verification: VerificationOptions{Review: true, MaxRepairs: 1}})
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("the review overran MaxModelRequests=1: err=%v requests=%d", err, result.Spend.ModelRequests)
	}
	if reviewer.calls != 0 {
		t.Fatalf("the review was sent past the budget: calls=%d", reviewer.calls)
	}
	if result.Spend.ModelRequests != 1 {
		t.Fatalf("requests=%d, want the one the run was allowed", result.Spend.ModelRequests)
	}
}

// countingReviewer counts reviews and answers with a scripted result.
type countingReviewer struct {
	result verify.ReviewResult
	err    error
	calls  int
}

func (r *countingReviewer) Review(context.Context, verify.ReviewRequest) (verify.ReviewResult, error) {
	r.calls++
	return r.result, r.err
}

// recordingArgTools executes bash with the arguments it was given and records them.
type recordingArgTools struct {
	exec func(map[string]any)
	ok   bool
}

func (a *recordingArgTools) Specs([]string) []protocol.ToolSpec {
	return []protocol.ToolSpec{{Name: "bash", Parameters: map[string]any{
		"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required": []string{"command"},
	}}}
}

func (a *recordingArgTools) Execute(_ context.Context, _ string, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	a.exec(args)
	if a.ok {
		return protocol.ToolResult{OK: true, Content: `{"exitCode":0}`}, nil
	}
	return protocol.ToolResult{OK: false, Content: `{"exitCode":1}`}, nil
}
