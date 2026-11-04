package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

func deadlineRows(messages []protocol.Message) []protocol.Message {
	var out []protocol.Message
	for _, m := range messages {
		if m.Origin == protocol.OriginDeadline {
			out = append(out, m)
		}
	}
	return out
}

func toolRows(messages []protocol.Message) []protocol.Message {
	var out []protocol.Message
	for _, m := range messages {
		if m.Role == "tool" {
			out = append(out, m)
		}
	}
	return out
}

// A run that reaches its deadline while a tool works ends interrupted/deadline, not cancelled; the
// tool's stopped output is kept as the call's one result (the command ran into the deadline reserve)
// and its execution is settled; the run's first row after the input states the budget.
func TestRunDeadlineEndsTheRunWithKindDeadline(t *testing.T) {
	store := newState("d1")
	tools := &scriptTools{fn: func(ctx context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
		<-ctx.Done()
		return protocol.ToolResult{OK: false, Interrupted: true, Content: `{"exitCode":-1,"timedOut":true}`, MutatesWorkspace: true}, nil
	}}
	provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return shellCall("t1"), nil
	}}
	started := time.Now()
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: tools}).Run(context.Background(), "d1",
		Options{Session: testRef, MaxSteps: 4, Deadline: time.Now().Add(400 * time.Millisecond)})
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the run ended %s after start, long past its deadline", elapsed)
	}
	if !errors.Is(err, ErrRunDeadline) {
		t.Fatalf("err = %v, want ErrRunDeadline", err)
	}
	assertExit(t, store, result, StatusInterrupted, KindDeadline)
	rows := toolRows(store.messages)
	if len(rows) != 1 || rows[0].ToolCallID != "t1" || rows[0].Origin != protocol.OriginToolInterrupted || !strings.Contains(rows[0].Content, `"timedOut":true`) {
		t.Fatalf("the stopped call must keep one terminal row: %+v", rows)
	}
	notices := deadlineRows(store.messages)
	if len(notices) == 0 || store.messages[1].Origin != protocol.OriginDeadline || !strings.Contains(notices[0].Content, "deadline") {
		t.Fatalf("the row after the input must be the deadline notice, got %+v", store.messages)
	}
}

// A deadline in the past ends the run at once, before any request.
func TestRunPastDeadlineEndsBeforeAnyRequest(t *testing.T) {
	store := newState("d2")
	provider := &funcProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
	}}
	result, _ := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(context.Background(), "d2",
		Options{Session: testRef, MaxSteps: 2, Deadline: time.Now().Add(-time.Second)})
	assertExit(t, store, result, StatusInterrupted, KindDeadline)
	if provider.n != 0 {
		t.Fatalf("%d requests were sent after the deadline", provider.n)
	}
}

// The host's cancellation of a run that has a deadline is still kind cancelled.
func TestRunWithDeadlineCancelledByTheHostIsCancelled(t *testing.T) {
	store := newState("d3")
	ctx, cancel := context.WithCancel(context.Background())
	provider := &funcProvider{fn: func(ctx context.Context, _ int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		cancel()
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}}
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}).Run(ctx, "d3",
		Options{Session: testRef, MaxSteps: 2, Deadline: time.Now().Add(time.Hour)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	assertExit(t, store, result, StatusInterrupted, KindCancelled)
}

// Every tool call sees its call id and a tool deadline before the run's deadline (the answer margin);
// every result reports the time left and where the time went, inside a JSON object or after plain text.
// Without a deadline neither the context nor the content changes.
func TestToolCallsSeeTheirDeadlineAndResultsReportTheTimeLeft(t *testing.T) {
	for _, withDeadline := range []bool{true, false} {
		store := newState("d4")
		deadline := time.Now().Add(time.Hour)
		var seen []time.Time
		var ids []string
		tools := &scriptTools{fn: func(ctx context.Context, _ string, args map[string]any) (protocol.ToolResult, error) {
			id := protocol.ToolCallID(ctx)
			ids = append(ids, id)
			if until, ok := protocol.ToolDeadline(ctx); ok {
				seen = append(seen, until)
			}
			if args["command"] == "plain" {
				return protocol.ToolResult{OK: true, Content: "plain text"}, nil
			}
			return protocol.ToolResult{OK: true, Content: `{"exitCode":0}`}, nil
		}}
		provider := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			switch n {
			case 1:
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("a", "bash", `{"command":"json"}`)}}}, nil
			case 2:
				return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("b", "bash", `{"command":"plain"}`)}}}, nil
			}
			return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
		}}
		opts := Options{Session: testRef, MaxSteps: 5}
		if withDeadline {
			opts.Deadline = deadline
		}
		result, err := (&Loop{Models: testModels(provider), Store: store, Tools: tools}).Run(context.Background(), "d4", opts)
		if err != nil || result.Status != StatusCompleted {
			t.Fatalf("withDeadline=%v: status=%s err=%v", withDeadline, result.Status, err)
		}
		if strings.Join(ids, ",") != "a,b" {
			t.Fatalf("tool call ids seen = %v, want a,b", ids)
		}
		rows := toolRows(store.messages)
		if len(rows) != 2 {
			t.Fatalf("tool rows = %+v", rows)
		}
		if !withDeadline {
			if len(seen) != 0 || rows[0].Content != `{"exitCode":0}` || rows[1].Content != "plain text" || len(deadlineRows(store.messages)) != 0 {
				t.Fatalf("a run without a deadline changed: deadlines=%v rows=%+v", seen, rows)
			}
			continue
		}
		if len(seen) != 2 {
			t.Fatalf("tool deadlines seen = %v", seen)
		}
		for _, until := range seen {
			if !until.Before(deadline) || deadline.Sub(until) < minAnswerMargin {
				t.Fatalf("tool deadline %s must leave at least %s before the run deadline %s", until, minAnswerMargin, deadline)
			}
		}
		var object struct {
			ExitCode int            `json:"exitCode"`
			Deadline deadlineReport `json:"deadline"`
		}
		if err := json.Unmarshal([]byte(rows[0].Content), &object); err != nil {
			t.Fatalf("an annotated JSON result must stay one valid object: %v (%s)", err, rows[0].Content)
		}
		if object.Deadline.RemainingSec <= 3500 || object.Deadline.RemainingSec > 3600 || object.Deadline.ReserveSec != 120 {
			t.Fatalf("deadline report = %+v, want about an hour left and the 120 s reserve", object.Deadline)
		}
		if !strings.HasPrefix(rows[1].Content, "plain text\n[runtime] deadline: {\"remainingSec\":") {
			t.Fatalf("a plain result must keep its text and gain a runtime line: %q", rows[1].Content)
		}
	}
}

// The reserve row is appended once, the first time the time left falls within the reserve; the run is not
// stopped by it.
func TestReserveNoticeIsAppendedOnceAndDoesNotStopTheRun(t *testing.T) {
	store := newState("d5")
	tools := &scriptTools{fn: func(ctx context.Context, _ string, _ map[string]any) (protocol.ToolResult, error) {
		time.Sleep(800 * time.Millisecond) // three calls bring the time left within the 2 s reserve
		return protocol.ToolResult{OK: true, Content: `{"exitCode":0}`}, nil
	}}
	provider := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if n <= 3 {
			return shellCall("t" + string(rune('0'+n))), nil
		}
		return protocol.ProviderResponse{Message: protocol.Message{Content: "delivered"}}, nil
	}}
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: tools}).Run(context.Background(), "d5",
		Options{Session: testRef, MaxSteps: 8, Deadline: time.Now().Add(4 * time.Second), DeadlineReserve: 3800 * time.Millisecond})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("status=%s err=%v; the reserve must not stop the run", result.Status, err)
	}
	notices := deadlineRows(store.messages)
	if len(notices) != 2 || !strings.Contains(notices[1].Content, "deliver") {
		t.Fatalf("want the start notice and exactly one reserve notice, got %+v", notices)
	}
}

// A tool that stopped its operation partway (Interrupted) leaves its execution outcome_unknown; a plain
// failure is failed.
func TestInterruptedToolResultEndsTheExecutionOutcomeUnknown(t *testing.T) {
	for _, tc := range []struct {
		interrupted bool
		want        session.ExecutionOutcome
	}{{true, session.OutcomeUnknown}, {false, session.OutcomeFailed}} {
		store := newState("d7")
		tools := &scriptTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
			return protocol.ToolResult{OK: false, Interrupted: tc.interrupted, Content: `{"exitCode":-1,"timedOut":true}`}, nil
		}}
		provider := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			if n == 1 {
				return shellCall("x1"), nil
			}
			return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
		}}
		if _, err := (&Loop{Models: testModels(provider), Store: store, Tools: tools}).Run(context.Background(), "d7",
			Options{Session: testRef, MaxSteps: 3, Interactive: true}); err != nil {
			t.Fatal(err)
		}
		if len(store.executions) != 1 || store.executions[0].post == nil {
			t.Fatalf("executions = %+v", store.executions)
		}
		if got := store.executions[0].post.Outcome; got != tc.want {
			t.Fatalf("interrupted=%v: outcome = %s, want %s", tc.interrupted, got, tc.want)
		}
	}
}

func TestRunClockReserveAndMargin(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		budget, reserve, want time.Duration
	}{
		{900 * time.Second, 0, 90 * time.Second},
		{100 * time.Second, 0, 30 * time.Second},
		{3600 * time.Second, 0, 120 * time.Second},
		{40 * time.Second, 0, 20 * time.Second}, // never more than half the budget
		{900 * time.Second, 45 * time.Second, 45 * time.Second},
	} {
		c := newRunClock(now.Add(tc.budget), tc.reserve, now)
		if c.reserve != tc.want {
			t.Errorf("budget %s reserve %s: got %s, want %s", tc.budget, tc.reserve, c.reserve, tc.want)
		}
	}
	c := newRunClock(now.Add(900*time.Second), 0, now)
	if got := c.answerMargin(); got != minAnswerMargin {
		t.Fatalf("margin before any request = %s, want %s", got, minAnswerMargin)
	}
	c.observeModel(40*time.Second, 40*time.Second)
	c.observeModel(5*time.Second, 5*time.Second)
	if got := c.answerMargin(); got != 60*time.Second {
		t.Fatalf("margin after a 40 s request = %s, want 60 s (1.5x the longest)", got)
	}
	c.observeModel(200*time.Second, 200*time.Second)
	if got := c.answerMargin(); got != c.reserve {
		t.Fatalf("margin = %s, want the reserve %s as its bound", got, c.reserve)
	}
	small := newRunClock(now.Add(8*time.Second), 0, now)
	if got := small.answerMargin(); got != small.reserve || got != 4*time.Second {
		t.Fatalf("a reserve below the minimum margin bounds it: margin %s reserve %s", got, small.reserve)
	}
	c.observeTool(jobWaitTool, 7*time.Second)
	c.observeTool("bash", 3*time.Second)
	if c.wait != 7*time.Second || c.tools != 3*time.Second {
		t.Fatalf("wait=%s tools=%s", c.wait, c.tools)
	}
	if got := (&runClock{}).annotate(`{"a":1}`, now); got != `{"a":1}` {
		t.Fatalf("no deadline must leave content unchanged: %s", got)
	}
	for _, content := range []string{`{}`, ` {"a":1} `, `{x}`, `[1]`} {
		got := c.annotate(content, now)
		if strings.HasPrefix(strings.TrimSpace(content), "{") && json.Valid([]byte(strings.TrimSpace(content))) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(got), &m); err != nil || m["deadline"] == nil {
				t.Fatalf("annotate(%q) = %q: not an object with a deadline member (%v)", content, got, err)
			}
			continue
		}
		if !strings.HasPrefix(got, content+"\n[runtime] deadline: ") {
			t.Fatalf("annotate(%q) = %q", content, got)
		}
	}
}
