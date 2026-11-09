package verify

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestSafetyDenialNeverExpires(t *testing.T) {
	for _, event := range []Event{EventStop, EventCompletion, EventBeforeVerification, EventAfterVerification} {
		t.Run(string(event), func(t *testing.T) {
			r := testRegistry(t)
			if err := r.Register(&testHook{id: "security", version: 1, events: []Event{event}, safety: true, run: func(context.Context, Request) (Result, error) { return Result{Decision: DecisionDeny}, nil }}); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < DefaultStopDenials+3; i++ {
				if out := r.Decide(context.Background(), Request{SessionID: "s", Event: event}); !out.Denied() || out.DenialLimitReached || out.StopLimitReached {
					t.Fatalf("security bypass at attempt %d: %+v", i, out)
				}
			}
		})
	}
}

func TestObservationFailureDoesNotHideLaterObserver(t *testing.T) {
	for _, failure := range []bool{false, true} {
		r := testRegistry(t)
		seen := false
		_ = r.Register(&testHook{id: "a", version: 1, events: []Event{EventAfterToolCall}, run: func(context.Context, Request) (Result, error) {
			if failure {
				return Result{}, errors.New("observer failed")
			}
			return Result{Decision: DecisionDeny}, nil
		}})
		_ = r.Register(&testHook{id: "b", version: 1, events: []Event{EventAfterToolCall}, run: func(context.Context, Request) (Result, error) {
			seen = true
			return Result{Decision: DecisionContinue}, nil
		}})
		out := r.Decide(context.Background(), Request{Event: EventAfterToolCall})
		if out.Denied() || !seen || len(out.Audit) != 2 {
			t.Fatalf("observation changed execution: %+v seen=%v", out, seen)
		}
	}
}

func TestHookRecursionFailsClosed(t *testing.T) {
	r := testRegistry(t)
	calls := 0
	_ = r.Register(&testHook{id: "recursive", version: 1, events: []Event{EventBeforeToolCall}, run: func(ctx context.Context, req Request) (Result, error) {
		calls++
		out := r.Decide(ctx, req)
		return Result{Decision: out.Decision}, nil
	}})
	out := r.Decide(context.Background(), Request{Event: EventBeforeToolCall})
	if !out.Denied() || calls != maxHookDepth {
		t.Fatalf("unbounded recursion: calls=%d out=%+v", calls, out)
	}
}

func TestHookModificationRequiresExplicitDecisionAndSchema(t *testing.T) {
	for _, tc := range []struct {
		event          Event
		decision       Decision
		schema, safety bool
		want           bool
	}{
		{EventBeforeToolCall, DecisionModifyAllowedFields, true, false, true},
		{EventBeforeToolCall, DecisionContinue, true, false, false},
		{EventBeforeToolCall, DecisionModifyAllowedFields, false, false, false},
		{EventBeforeEval, DecisionModifyAllowedFields, true, false, false},
		{EventBeforeToolCall, DecisionModifyAllowedFields, true, true, false},
	} {
		r := testRegistry(t)
		_ = r.Register(&testHook{id: "rewrite", version: 1, events: []Event{tc.event}, mutable: true, safety: tc.safety, run: func(context.Context, Request) (Result, error) {
			return Result{Decision: tc.decision, Args: map[string]any{"path": "changed"}}, nil
		}})
		req := Request{Event: tc.event, ToolArgs: map[string]any{"path": "original"}}
		if tc.schema {
			req.ValidateArgs = func(args map[string]any) error {
				if _, ok := args["path"].(string); !ok {
					return errors.New("path type")
				}
				return nil
			}
		}
		out := r.Decide(context.Background(), req)
		if (out.Args != nil) != tc.want || req.ToolArgs["path"] != "original" {
			t.Fatalf("unauthorized mutation for %+v: %+v", tc, out)
		}
	}
}

func TestAuditLimitCannotSkipSecurity(t *testing.T) {
	r := testRegistry(t)
	for i := 0; i < maxAuditEntries; i++ {
		_ = r.Register(&testHook{id: fmt.Sprint(i), version: 1, events: []Event{EventBeforeToolCall}, run: func(context.Context, Request) (Result, error) { return Result{Decision: DecisionContinue}, nil }})
	}
	_ = r.Register(&testHook{id: "security", version: 1, events: []Event{EventBeforeToolCall}, safety: true, run: func(context.Context, Request) (Result, error) { return Result{Decision: DecisionDeny}, nil }})
	if out := r.Decide(context.Background(), Request{Event: EventBeforeToolCall}); !out.Denied() {
		t.Fatalf("audit truncation bypassed guard: %+v", out)
	}
}
