package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestBackoffDoublesToTheCapWithJitter(t *testing.T) {
	p := Recovery{BaseDelay: time.Second, MaxDelay: 8 * time.Second}
	for n, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 4: 8 * time.Second, 9: 8 * time.Second, 1000: 8 * time.Second} {
		if got := p.backoff(n, 1); got != want {
			t.Errorf("backoff(%d, jitter 1) = %s, want %s", n, got, want)
		}
		if got := p.backoff(n, 0); got != want*3/4 {
			t.Errorf("backoff(%d, jitter 0) = %s, want %s (75%%)", n, got, want*3/4)
		}
	}
}

// The wait is the longer of the backoff and the shortest Retry-After of the sweep; a wait that passes
// the outage window or reaches the deadline is refused with that reason, before anything sleeps.
func TestRecoveryNextHonoursRetryAfterOutageAndDeadline(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	p := Recovery{BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxOutage: time.Minute}
	cases := []struct {
		name     string
		hints    []time.Duration
		start    time.Time
		deadline time.Time
		wait     time.Duration
		stop     string
	}{
		{"backoff", []time.Duration{0}, now, time.Time{}, time.Second, ""},
		{"retry-after longer than backoff", []time.Duration{20 * time.Second}, now, time.Time{}, 20 * time.Second, ""},
		{"shortest hint of a sweep", []time.Duration{30 * time.Second, 10 * time.Second}, now, time.Time{}, 10 * time.Second, ""},
		{"a candidate without a hint", []time.Duration{30 * time.Second, 0}, now, time.Time{}, time.Second, ""},
		{"outage window", []time.Duration{0}, now.Add(-time.Minute), time.Time{}, time.Second, RecoveryStopOutage},
		{"retry-after past the outage window", []time.Duration{2 * time.Minute}, now, time.Time{}, 2 * time.Minute, RecoveryStopOutage},
		{"deadline", []time.Duration{20 * time.Second}, now, now.Add(10 * time.Second), 20 * time.Second, RecoveryStopDeadline},
		{"exactly at the deadline", []time.Duration{0}, now, now.Add(time.Second), time.Second, RecoveryStopDeadline},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var o outage
			for _, h := range c.hints {
				o.fail(c.start, h)
			}
			wait, stop := p.next(&o, now, c.deadline, 1)
			if wait != c.wait || stop != c.stop {
				t.Fatalf("next = %s %q, want %s %q", wait, stop, c.wait, c.stop)
			}
		})
	}
	if _, stop := (Recovery{}).next(&outage{}, now, time.Time{}, 1); stop != RecoveryStopDisabled {
		t.Fatalf("the zero Recovery retried: %q", stop)
	}
}

func TestRecoveryFromConfig(t *testing.T) {
	got := RecoveryFrom(config.Retry{}, true)
	if want := (Recovery{BaseDelay: time.Second, MaxDelay: 30 * time.Second}); got != want {
		t.Errorf("defaults with a deadline = %+v, want %+v (the deadline bounds the outage)", got, want)
	}
	if got := RecoveryFrom(config.Retry{}, false); got.MaxOutage != 30*time.Minute {
		t.Errorf("defaults without a deadline: outage %s, want 30m", got.MaxOutage)
	}
	if got := RecoveryFrom(config.Retry{BaseDelayMs: 50, MaxDelayMs: 10, MaxOutageSeconds: 5}, true); got != (Recovery{BaseDelay: 50 * time.Millisecond, MaxDelay: 50 * time.Millisecond, MaxOutage: 5 * time.Second}) {
		t.Errorf("configured = %+v", got)
	}
	if got := RecoveryFrom(config.Retry{Disabled: true, BaseDelayMs: 50}, true); got.enabled() {
		t.Errorf("disabled = %+v, want no retries", got)
	}
}

// flakyProvider plays its script in order (an error or a response per request), then answers every
// further request with a 503, and records every request.
type flakyProvider struct {
	mu     sync.Mutex
	script []any
	seen   []protocol.ProviderRequest
	at     []time.Time
}

func (p *flakyProvider) Model() string { return "flaky" }
func (p *flakyProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, req)
	p.at = append(p.at, time.Now())
	if len(p.script) == 0 {
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: protocol.ClassAvailability, Reason: protocol.ReasonOverloaded, Status: 503, Err: errors.New("503 down")}
	}
	next := p.script[0]
	p.script = p.script[1:]
	if err, ok := next.(error); ok {
		return protocol.ProviderResponse{}, err
	}
	return next.(protocol.ProviderResponse), nil
}

type countingTools struct {
	fakeTools
	calls atomic.Int32
}

func (c *countingTools) Execute(ctx context.Context, name string, args map[string]any, out func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	c.calls.Add(1)
	return c.fakeTools.Execute(ctx, name, args, out)
}

type eventLog struct {
	mu     sync.Mutex
	events []Event
}

func (l *eventLog) emit(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) of(kind string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, e := range l.events {
		if e.Type == kind {
			out = append(out, e.Data)
		}
	}
	return out
}

func unavailable(reason protocol.Reason, status int, retryAfter time.Duration) error {
	return &protocol.ProviderError{Class: protocol.ClassAvailability, Reason: reason, Status: status, RetryAfter: retryAfter,
		Err: fmt.Errorf("HTTP %d", status)}
}

// A 502, a 429 with Retry-After and a cut stream in a row, between the tool call and the final
// answer: the run waits them out on one session, every retry sends the same transcript as a new
// admitted request, the tool runs once, and the run completes.
func TestLoopWaitsOutConsecutiveUpstreamFailures(t *testing.T) {
	p := &flakyProvider{script: []any{
		protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("1", "apply_patch", `{}`)}}},
		unavailable(protocol.ReasonServerError, 502, 0),
		unavailable(protocol.ReasonRateLimited, 429, 120*time.Millisecond),
		unavailable(protocol.ReasonStreamIncomplete, 0, 0),
		answerOf("done"),
	}}
	tools := &countingTools{}
	store := envStore("rec")
	events := &eventLog{}
	loop := Loop{Models: testModels(p), Store: store, Tools: tools, Emit: events.emit}
	opts := Options{Session: testRef, MaxSteps: 4, RunID: "r",
		Recovery: Recovery{BaseDelay: 10 * time.Millisecond, MaxDelay: 40 * time.Millisecond, MaxOutage: time.Minute}}
	result, err := loop.Run(context.Background(), "rec", opts)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	if got := tools.calls.Load(); got != 1 {
		t.Fatalf("tool executions = %d, want 1: a failed request never runs a tool", got)
	}
	if len(p.seen) != 5 {
		t.Fatalf("requests = %d, want 5 (answer, 3 failures, answer)", len(p.seen))
	}
	for i := 2; i < 5; i++ {
		if !reflect.DeepEqual(p.seen[i].Messages, p.seen[1].Messages) {
			t.Fatalf("retry %d sent a different transcript", i-1)
		}
	}
	// The 429's Retry-After (120 ms) outlasts its backoff (at most 20 ms).
	if gap := p.at[3].Sub(p.at[2]); gap < 120*time.Millisecond {
		t.Fatalf("the retry after the 429 came %s later, want at least its 120 ms Retry-After", gap)
	}
	retries := events.of("provider.retry")
	if len(retries) != 3 {
		t.Fatalf("provider.retry events = %d, want 3", len(retries))
	}
	wantReasons := []protocol.Reason{protocol.ReasonServerError, protocol.ReasonRateLimited, protocol.ReasonStreamIncomplete}
	for i, r := range retries {
		if r["reason"] != wantReasons[i] || r["retry"] != i+1 || r["failedRequests"] != i+1 {
			t.Errorf("retry event %d = %v", i, r)
		}
	}
	if r := retries[1]; r["status"] != 429 || r["retryAfterMs"] != int64(120) || r["delayMs"].(int64) < 120 {
		t.Errorf("the 429 retry event = %v", r)
	}
	recovered := events.of("provider.recovered")
	if len(recovered) != 1 || recovered[0]["failedRequests"] != 3 || recovered[0]["retries"] != 3 {
		t.Fatalf("provider.recovered = %v", recovered)
	}
	envs := store.envelopes()
	if len(envs) != 5 || store.admissions != 5 {
		t.Fatalf("envelopes=%d admissions=%d, want one per request", len(envs), store.admissions)
	}
	if got := states(envs); fmt.Sprint(got) != "[answered unknown:provider_error unknown:provider_error unknown:provider_error answered]" {
		t.Fatalf("envelope states = %v", got)
	}
}

// An outage longer than the window ends the run interrupted, with the count, the stop reason and the
// last provider error in the persisted cause.
func TestLoopStopsAtTheOutageWindowWithTheReason(t *testing.T) {
	p := &flakyProvider{}
	store := envStore("out")
	events := &eventLog{}
	loop := Loop{Models: testModels(p), Store: store, Tools: fakeTools{}, Emit: events.emit}
	opts := Options{Session: testRef, MaxSteps: 2, RunID: "r",
		Recovery: Recovery{BaseDelay: 20 * time.Millisecond, MaxDelay: 20 * time.Millisecond, MaxOutage: 100 * time.Millisecond}}
	start := time.Now()
	result, err := loop.Run(context.Background(), "out", opts)
	if result.Status != StatusInterrupted || store.cause["kind"] != KindProviderAvailability {
		t.Fatalf("result %+v cause %v", result, store.cause)
	}
	var rec *RecoveryError
	if !errors.As(err, &rec) || rec.Stop != RecoveryStopOutage || rec.Failures != len(p.seen) || rec.Retries != rec.Failures-1 {
		t.Fatalf("err %v (%+v), requests %d", err, rec, len(p.seen))
	}
	var pe *protocol.ProviderError
	if !errors.As(err, &pe) || pe.Status != 503 {
		t.Fatalf("the last provider error is not reachable: %v", err)
	}
	msg, _ := store.cause["message"].(string)
	for _, want := range []string{"provider unavailable for", "failed requests", "outage window", "503 down"} {
		if !strings.Contains(msg, want) {
			t.Errorf("cause message %q lacks %q", msg, want)
		}
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stopped after %s", elapsed)
	}
	if stopped := events.of("provider.retry_stopped"); len(stopped) != 1 || stopped[0]["stop"] != RecoveryStopOutage {
		t.Fatalf("provider.retry_stopped = %v", stopped)
	}
}

// A Retry-After past the run's deadline is not slept through: the run ends at once with the reason.
func TestLoopDoesNotWaitPastTheDeadline(t *testing.T) {
	p := &flakyProvider{script: []any{unavailable(protocol.ReasonRateLimited, 429, time.Hour)}}
	store := envStore("dl")
	loop := Loop{Models: testModels(p), Store: store, Tools: fakeTools{}}
	opts := Options{Session: testRef, MaxSteps: 2, RunID: "r", Deadline: time.Now().Add(10 * time.Minute),
		Recovery: Recovery{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}}
	start := time.Now()
	result, err := loop.Run(context.Background(), "dl", opts)
	var rec *RecoveryError
	if result.Status != StatusInterrupted || !errors.As(err, &rec) || rec.Stop != RecoveryStopDeadline || rec.Wait != time.Hour {
		t.Fatalf("result %+v err %v", result, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second || len(p.seen) != 1 {
		t.Fatalf("elapsed %s requests %d", elapsed, len(p.seen))
	}
}

// Cancelling the run during a backoff ends it cancelled at once, without another request.
func TestCancelDuringBackoffEndsTheRun(t *testing.T) {
	p := &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0)}}
	store := envStore("cx")
	events := &eventLog{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop := Loop{Models: testModels(p), Store: store, Tools: fakeTools{}, Emit: func(e Event) {
		events.emit(e)
		if e.Type == "provider.retry" {
			cancel()
		}
	}}
	opts := Options{Session: testRef, MaxSteps: 2, RunID: "r", Recovery: Recovery{BaseDelay: time.Hour, MaxDelay: time.Hour}}
	start := time.Now()
	result, _ := loop.Run(ctx, "cx", opts)
	if result.Status != StatusInterrupted || store.cause["kind"] != KindCancelled {
		t.Fatalf("result %+v cause %v", result, store.cause)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second || len(p.seen) != 1 {
		t.Fatalf("elapsed %s requests %d", elapsed, len(p.seen))
	}
}

// Non-availability errors never wait: auth and bad requests end the run on the first attempt.
func TestOnlyAvailabilityIsRetried(t *testing.T) {
	for _, class := range []protocol.ErrorClass{protocol.ClassAuth, protocol.ClassBadRequest} {
		p := &flakyProvider{script: []any{&protocol.ProviderError{Class: class, Status: 400, Err: errors.New("no")}}}
		store := envStore("na")
		loop := Loop{Models: testModels(p), Store: store, Tools: fakeTools{}}
		opts := Options{Session: testRef, MaxSteps: 2, RunID: "r", Recovery: Recovery{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}}
		result, _ := loop.Run(context.Background(), "na", opts)
		if result.Status != StatusFailed || len(p.seen) != 1 {
			t.Fatalf("%s: result %+v requests %d", class, result, len(p.seen))
		}
	}
}

// Failover comes first: a sweep tries every candidate before the step waits, and the retry starts
// again from the session model.
func TestRetrySweepsFromTheFirstCandidate(t *testing.T) {
	const a, b = config.ModelRef("test/a"), config.ModelRef("test/b")
	pa := &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0), answerOf("done")}}
	pb := &flakyProvider{script: []any{unavailable(protocol.ReasonServerError, 502, 0)}}
	health := NewHealth(nil)
	loop := Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: envStore("sw"), Tools: fakeTools{}, Health: health}
	opts := Options{Session: a, Failover: []config.ModelRef{b}, MaxSteps: 2, RunID: "r",
		Recovery: Recovery{BaseDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxOutage: time.Minute}}
	result, err := loop.Run(context.Background(), "sw", opts)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(pa.seen) != 2 || len(pb.seen) != 1 {
		t.Fatalf("requests a=%d b=%d, want a, b, then a again", len(pa.seen), len(pb.seen))
	}
}
