package verify

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

type testHook struct {
	id       string
	version  int
	events   []Event
	priority int
	timeout  time.Duration
	policy   FailPolicy
	mutable  bool
	safety   bool
	run      func(context.Context, Request) (Result, error)
}

func (h *testHook) ID() string      { return h.id }
func (h *testHook) Version() int    { return h.version }
func (h *testHook) Events() []Event { return h.events }
func (h *testHook) Priority() int   { return h.priority }
func (h *testHook) Timeout() time.Duration {
	return h.timeout
}
func (h *testHook) FailPolicy() FailPolicy { return h.policy }
func (h *testHook) MutableArgs() bool      { return h.mutable }
func (h *testHook) Safety() bool           { return h.safety }
func (h *testHook) Run(ctx context.Context, req Request) (Result, error) {
	return h.run(ctx, req)
}

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(Version)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRegistryRefusesInvalidHooks(t *testing.T) {
	r := testRegistry(t)
	if _, err := NewRegistry("2.0"); err == nil {
		t.Fatal("an unknown protocol version must be refused")
	}
	cases := []struct {
		name string
		hook Hook
	}{
		{"no id", &testHook{version: 1, events: []Event{EventStop}}},
		{"no version", &testHook{id: "a", events: []Event{EventStop}}},
		{"no events", &testHook{id: "a", version: 1}},
		{"unknown event", &testHook{id: "a", version: 1, events: []Event{"Nope"}}},
	}
	for _, c := range cases {
		if err := r.Register(c.hook); err == nil {
			t.Fatalf("%s: hook must be refused", c.name)
		}
	}
	good := &testHook{id: "a", version: 1, events: []Event{EventStop}, run: func(context.Context, Request) (Result, error) {
		return Result{Decision: DecisionAllow}, nil
	}}
	if err := r.Register(good); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(good); err == nil {
		t.Fatal("a duplicate id must be refused")
	}
	if len(r.Hooks()) != 1 || r.Hooks()[0].Version != 1 {
		t.Fatalf("hooks=%+v", r.Hooks())
	}
}

func TestHooksRunInDeterministicOrder(t *testing.T) {
	r := testRegistry(t)
	var order []string
	mk := func(id string, priority int) *testHook {
		return &testHook{id: id, version: 1, events: []Event{EventPreToolUse}, priority: priority,
			run: func(context.Context, Request) (Result, error) {
				order = append(order, id)
				return Result{Decision: DecisionContinue}, nil
			}}
	}
	// Registered out of order: the event order is priority, then id.
	for _, h := range []Hook{mk("b", 1), mk("a", 2), mk("c", 1)} {
		if err := r.Register(h); err != nil {
			t.Fatal(err)
		}
	}
	out := r.Decide(context.Background(), Request{Event: EventPreToolUse})
	if out.Decision != DecisionAllow {
		t.Fatalf("decision=%s", out.Decision)
	}
	if !slices.Equal(order, []string{"b", "c", "a"}) {
		t.Fatalf("order=%v", order)
	}
	if len(out.Audit) != 3 || out.Audit[0].Hook != "b" {
		t.Fatalf("audit=%+v", out.Audit)
	}
}

func TestDenyShortCircuits(t *testing.T) {
	r := testRegistry(t)
	ran := false
	deny := &testHook{id: "a", version: 1, events: []Event{EventPreToolUse}, priority: 0,
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionDeny, Reason: "no"}, nil
		}}
	after := &testHook{id: "b", version: 1, events: []Event{EventPreToolUse}, priority: 1,
		run: func(context.Context, Request) (Result, error) {
			ran = true
			return Result{Decision: DecisionAllow}, nil
		}}
	_ = r.Register(deny)
	_ = r.Register(after)
	out := r.Decide(context.Background(), Request{Event: EventPreToolUse})
	if !out.Denied() || ran {
		t.Fatalf("out=%+v ran=%v", out, ran)
	}
	if len(out.Audit) != 1 || out.Audit[0].Decision != DecisionDeny {
		t.Fatalf("audit=%+v", out.Audit)
	}
}

func TestHookTimeoutFailsClosed(t *testing.T) {
	r := testRegistry(t)
	slow := &testHook{id: "slow", version: 1, events: []Event{EventPreVerify}, timeout: time.Millisecond,
		run: func(ctx context.Context, _ Request) (Result, error) {
			<-ctx.Done()
			return Result{}, ctx.Err()
		}}
	if err := r.Register(slow); err != nil {
		t.Fatal(err)
	}
	out := r.Decide(context.Background(), Request{Event: EventPreVerify})
	if !out.Denied() {
		t.Fatalf("a timed-out hook must fail closed, out=%+v", out)
	}
	if out.Audit[0].FailPolicy != FailClosed {
		t.Fatalf("policy=%s", out.Audit[0].FailPolicy)
	}
}

func TestExplicitFailOpenContinues(t *testing.T) {
	r := testRegistry(t)
	broken := &testHook{id: "open", version: 1, events: []Event{EventPostToolUse}, policy: FailOpen,
		run: func(context.Context, Request) (Result, error) {
			return Result{}, errors.New("boom")
		}}
	if err := r.Register(broken); err != nil {
		t.Fatal(err)
	}
	out := r.Decide(context.Background(), Request{Event: EventPostToolUse})
	if out.Denied() {
		t.Fatalf("an explicitly fail-open hook must not block: %+v", out)
	}
	if out.Audit[0].Decision != DecisionContinue || out.Audit[0].FailPolicy != FailOpen {
		t.Fatalf("audit=%+v", out.Audit)
	}
}

func TestSafetyHookAlwaysFailsClosed(t *testing.T) {
	r := testRegistry(t)
	safety := &testHook{id: "safe", version: 1, events: []Event{EventPreToolUse}, policy: FailOpen, safety: true,
		run: func(context.Context, Request) (Result, error) {
			return Result{}, errors.New("guard failed")
		}}
	if err := r.Register(safety); err != nil {
		t.Fatal(err)
	}
	if out := r.Decide(context.Background(), Request{Event: EventPreToolUse}); !out.Denied() {
		t.Fatalf("a safety hook must fail closed: %+v", out)
	}
}

func TestMutableArgsOnlyFromMutableHook(t *testing.T) {
	r := testRegistry(t)
	fixed := &testHook{id: "fixed", version: 1, events: []Event{EventPreToolUse}, mutable: false,
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionAllow, Args: map[string]any{"path": "other"}}, nil
		}}
	mutable := &testHook{id: "mutable", version: 1, events: []Event{EventPreToolUse}, priority: 1, mutable: true,
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionAllow, Args: map[string]any{"limit": 5}}, nil
		}}
	_ = r.Register(fixed)
	_ = r.Register(mutable)
	out := r.Decide(context.Background(), Request{Event: EventPreToolUse, ToolArgs: map[string]any{"path": "a"}})
	if out.Args["path"] != nil {
		t.Fatalf("a non-mutable hook changed arguments: %v", out.Args)
	}
	if out.Args["limit"] != 5 {
		t.Fatalf("mutable args missing: %v", out.Args)
	}
	if !out.Audit[0].ArgsIgnored {
		t.Fatalf("audit=%+v", out.Audit)
	}
}

func TestFeedbackIsBounded(t *testing.T) {
	r := testRegistry(t)
	huge := &testHook{id: "huge", version: 1, events: []Event{EventStop},
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionDeny, Feedback: strings.Repeat("x", 10000)}, nil
		}}
	_ = r.Register(huge)
	out := r.Decide(context.Background(), Request{Event: EventStop})
	if len(out.FeedbackText()) > DefaultFeedbackBytes {
		t.Fatalf("feedback=%d bytes", len(out.FeedbackText()))
	}
	if !strings.HasSuffix(out.FeedbackText(), "…") {
		t.Fatalf("truncation marker missing")
	}
}

func TestStopDenialsAreFinite(t *testing.T) {
	r := testRegistry(t)
	r.MaxStopDenials = 2
	stop := &testHook{id: "hold", version: 1, events: []Event{EventStop},
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionDeny, Feedback: "more work"}, nil
		}}
	_ = r.Register(stop)
	req := Request{Event: EventStop, SessionID: "s"}
	if out := r.Decide(context.Background(), req); !out.Denied() {
		t.Fatalf("first denial must block: %+v", out)
	}
	if out := r.Decide(context.Background(), req); !out.Denied() {
		t.Fatalf("second denial must block: %+v", out)
	}
	out := r.Decide(context.Background(), req)
	if out.Denied() {
		t.Fatalf("a Stop hook must not hold a run open forever: %+v", out)
	}
	if !out.StopLimitReached {
		t.Fatalf("out=%+v", out)
	}
	// Another session has its own finite count.
	if out := r.Decide(context.Background(), Request{Event: EventStop, SessionID: "other"}); !out.Denied() {
		t.Fatalf("a new session must get its own denials: %+v", out)
	}
	// A non-Stop deny is not limited.
	block := &testHook{id: "block", version: 1, events: []Event{EventPreVerify},
		run: func(context.Context, Request) (Result, error) {
			return Result{Decision: DecisionDeny}, nil
		}}
	_ = r.Register(block)
	for range 5 {
		if out := r.Decide(context.Background(), Request{Event: EventPreVerify, SessionID: "s"}); !out.Denied() {
			t.Fatal("PreVerify deny must always block")
		}
	}
}

func TestHookSeesTypedToolResult(t *testing.T) {
	r := testRegistry(t)
	var seen *protocol.ToolResult
	h := &testHook{id: "observe", version: 1, events: []Event{EventPostEval},
		run: func(_ context.Context, req Request) (Result, error) {
			seen = req.ToolResult
			return Result{Decision: DecisionContinue}, nil
		}}
	_ = r.Register(h)
	result := protocol.ToolResult{OK: true, Content: "x"}
	r.Decide(context.Background(), Request{Event: EventPostEval, ToolResult: &result})
	if seen == nil || !seen.OK || seen.Content != "x" {
		t.Fatalf("seen=%+v", seen)
	}
}
