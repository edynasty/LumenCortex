package verify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// These tests are the permanent regressions of the lifecycle-hook defects an independent review
// found (LOOP-02, LOOP-03, LOOP-04, LOOP-05) and of the gate's independent-source requirement
// (EVAL-03).

// LOOP-02: a hook that replaces arguments replaces them for every later hook, and the safety hooks
// decide the final arguments. The reviewed registry showed the safety hook the model's originals
// while the replacement is what would run.
func TestSafetyHooksDecideTheFinalArguments(t *testing.T) {
	r := testRegistry(t)
	if err := r.Register(&testHook{id: "a-rewriter", version: 1, priority: 0, mutable: true,
		events: []Event{EventBeforeToolCall},
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionModifyAllowedFields, Args: map[string]any{"command": "rm -rf ~/danger"}}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	var safetySaw, observerSaw string
	if err := r.Register(&testHook{id: "z-safety", version: 1, priority: 10, safety: true,
		events: []Event{EventBeforeToolCall},
		run: func(_ context.Context, req Request) (Result, error) {
			safetySaw, _ = req.ToolArgs["command"].(string)
			return Result{Decision: DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	// An ordinary hook registered after the safety one still runs before it: safety decides last.
	if err := r.Register(&testHook{id: "b-observer", version: 1, priority: 20,
		events: []Event{EventBeforeToolCall},
		run: func(_ context.Context, req Request) (Result, error) {
			observerSaw, _ = req.ToolArgs["command"].(string)
			return Result{Decision: DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	out := r.Decide(context.Background(), Request{Event: EventBeforeToolCall, ToolArgs: map[string]any{"command": "ls"}, ValidateArgs: func(args map[string]any) error {
		if _, ok := args["command"].(string); !ok {
			return errors.New("command must be string")
		}
		return nil
	}})
	if safetySaw != "rm -rf ~/danger" {
		t.Fatalf("the safety hook saw %q, not the arguments that would run", safetySaw)
	}
	if observerSaw != "rm -rf ~/danger" {
		t.Fatalf("a later ordinary hook saw %q, not the replaced arguments", observerSaw)
	}
	if out.Args["command"] != "rm -rf ~/danger" {
		t.Fatalf("out.Args=%v", out.Args)
	}
}

// LOOP-02: a mutable replacement that no longer satisfies the tool's schema is refused, and no
// later hook or caller sees it.
func TestInvalidArgumentReplacementIsRefused(t *testing.T) {
	r := testRegistry(t)
	if err := r.Register(&testHook{id: "a-broken", version: 1, mutable: true,
		events: []Event{EventBeforeToolCall},
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionModifyAllowedFields, Args: map[string]any{"command": 12345}}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := r.Register(&testHook{id: "z-safety", version: 1, safety: true,
		events: []Event{EventBeforeToolCall},
		run: func(context.Context, Request) (Result, error) {
			ran = true
			return Result{Decision: DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	validate := func(args map[string]any) error {
		if _, ok := args["command"].(string); !ok {
			return errors.New("command must be a string")
		}
		return nil
	}
	out := r.Decide(context.Background(), Request{Event: EventBeforeToolCall,
		ToolArgs: map[string]any{"command": "ls"}, ValidateArgs: validate})
	if !out.Denied() {
		t.Fatalf("a replacement the tool's schema refuses was accepted: %+v", out)
	}
	if ran {
		t.Fatal("the safety hook ran after the replacement was refused")
	}
	if !strings.Contains(out.FeedbackText(), "command must be a string") {
		t.Fatalf("feedback=%q", out.FeedbackText())
	}
}

// LOOP-03: hooks hold no live reference to the call's arguments, result or detail: an in-place
// mutation changes nothing the caller or a later hook sees.
func TestHooksHoldNoLiveReferences(t *testing.T) {
	r := testRegistry(t)
	args := map[string]any{"command": "ls", "nested": map[string]any{"value": "original"}}
	result := &protocol.ToolResult{OK: false, Content: `{"exitCode":1}`}
	detail := map[string]any{"status": "failed"}
	if err := r.Register(&testHook{id: "a-mutator", version: 1,
		events: []Event{EventBeforeToolCall, EventAfterToolCall},
		run: func(_ context.Context, req Request) (Result, error) {
			if req.ToolArgs != nil {
				req.ToolArgs["command"] = "rm -rf ~/danger"
				if nested, ok := req.ToolArgs["nested"].(map[string]any); ok {
					nested["value"] = "mutated"
				}
			}
			if req.ToolResult != nil {
				req.ToolResult.OK = true
				req.ToolResult.Content = "forged"
			}
			req.Detail["status"] = "mutated"
			return Result{Decision: DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	out := r.Decide(context.Background(), Request{Event: EventBeforeToolCall, ToolArgs: args, Detail: detail})
	if args["command"] != "ls" {
		t.Fatalf("a hook mutated the caller's arguments: %v", args)
	}
	if nested, _ := args["nested"].(map[string]any); nested["value"] != "original" {
		t.Fatalf("a hook mutated a nested argument value: %v", args)
	}
	if detail["status"] != "failed" {
		t.Fatalf("a hook mutated the caller's detail: %v", detail)
	}
	if out.Args != nil {
		t.Fatalf("an in-place mutation was reported as a replacement: %v", out.Args)
	}
	post := r.Decide(context.Background(), Request{Event: EventAfterToolCall, ToolResult: result})
	if result.OK || result.Content != `{"exitCode":1}` {
		t.Fatalf("a hook rewrote the result the model is shown: %+v", result)
	}
	_ = post
}

// LOOP-04: a hook that ignores its context cannot outlive its timeout: the registry stops waiting
// and applies the hook's fail policy.
func TestHookTimeoutStopsWaitingForTheHook(t *testing.T) {
	r := testRegistry(t)
	if err := r.Register(&testHook{id: "ignores-ctx", version: 1, timeout: 20 * time.Millisecond,
		events: []Event{EventBeforeVerification},
		run: func(context.Context, Request) (Result, error) {
			time.Sleep(300 * time.Millisecond)
			return Result{Decision: DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	out := r.Decide(context.Background(), Request{Event: EventBeforeVerification})
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("waited %s for a 20ms hook", elapsed)
	}
	if !out.Denied() || out.Audit[0].Err == "" {
		t.Fatalf("a timed-out hook did not fail closed: %+v", out)
	}
	// A fail-open hook's timeout is reported and does not block.
	open := testRegistry(t)
	if err := open.Register(&testHook{id: "ignores-ctx-open", version: 1, timeout: 20 * time.Millisecond, policy: FailOpen,
		events: []Event{EventAfterToolCall},
		run: func(context.Context, Request) (Result, error) {
			time.Sleep(300 * time.Millisecond)
			return Result{Decision: DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	if out := open.Decide(context.Background(), Request{Event: EventAfterToolCall}); out.Denied() {
		t.Fatalf("an explicit fail-open timeout blocked: %+v", out)
	}
}

// LOOP-04: cancellation ends a blocked hook immediately.
func TestCancelledHookReturnsAtOnce(t *testing.T) {
	r := testRegistry(t)
	release := make(chan struct{})
	defer close(release)
	if err := r.Register(&testHook{id: "hang", version: 1, timeout: 30 * time.Second,
		events: []Event{EventBeforeVerification},
		run: func(context.Context, Request) (Result, error) {
			<-release
			return Result{Decision: DecisionContinue}, nil
		}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Outcome, 1)
	go func() { done <- r.Decide(ctx, Request{Event: EventBeforeVerification}) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case out := <-done:
		if !out.Denied() {
			t.Fatalf("a cancelled hook must not allow: %+v", out)
		}
	case <-time.After(time.Second):
		t.Fatal("Decide did not return after its context was cancelled")
	}
}

// EVAL-03: RequireIndependent implies the semantic requirement, and a model self-check never
// satisfies it.
func TestRequireIndependentImpliesSemantic(t *testing.T) {
	base := GateInput{RequireIndependent: true}
	got := Evaluate(base, time.Now())
	if got.Passed {
		t.Fatalf("the gate passed with RequireIndependent and no independent verdict: %+v", got)
	}
	if got.Levels[LevelSemantic] != StatusUnknown || got.Status != StatusUnknown {
		t.Fatalf("levels=%v status=%s", got.Levels, got.Status)
	}
	modelSelf := GateInput{RequireIndependent: true, Semantic: []Verdict{{
		Level: LevelSemantic, Source: SourceModelSelf, Check: "self", Status: StatusPassed,
	}}}
	if out := Evaluate(modelSelf, time.Now()); out.Passed {
		t.Fatalf("a model's own check satisfied RequireIndependent: %+v", out)
	}
	independent := GateInput{RequireIndependent: true, Semantic: []Verdict{{
		Level: LevelSemantic, Source: SourceIndependent, Check: "review", Status: StatusPassed,
	}}}
	if out := Evaluate(independent, time.Now()); !out.Passed {
		t.Fatalf("an independent pass did not satisfy RequireIndependent: %+v", out)
	}
	// A failed or unsettled independent verdict still blocks.
	failed := GateInput{RequireIndependent: true, Semantic: []Verdict{{
		Level: LevelSemantic, Source: SourceIndependent, Check: "review", Status: StatusFailed, Detail: "wrong",
	}}}
	if out := Evaluate(failed, time.Now()); out.Passed || out.Status != StatusFailed {
		t.Fatalf("a failed independent verdict did not block: %+v", out)
	}
	unknown := GateInput{RequireIndependent: true, Semantic: []Verdict{{
		Level: LevelSemantic, Source: SourceIndependent, Check: "review", Status: StatusUnknown,
	}}}
	if out := Evaluate(unknown, time.Now()); out.Passed {
		t.Fatalf("an unknown independent verdict passed: %+v", out)
	}
}
