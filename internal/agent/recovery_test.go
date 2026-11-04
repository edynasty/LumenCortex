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

// A recovery request goes, in failover order after the candidate that failed, to the first candidate
// ready within the backoff, after the backoff; when none is, to the one ready soonest, once its own
// Retry-After has passed. Candidates never retried (-1) are skipped.
func TestRecoveryPickRotatesAndHonoursEachRetryAfter(t *testing.T) {
	const s = time.Second
	cases := []struct {
		name    string
		readyIn []time.Duration
		failed  int
		pick    int
		wait    time.Duration
	}{
		{"one candidate", []time.Duration{0}, 0, 0, s},
		{"rotates past the one that failed", []time.Duration{0, 0, 0}, 0, 1, s},
		{"wraps around", []time.Duration{0, 0, 0}, 2, 0, s},
		{"skips a cooling candidate", []time.Duration{0, 60 * s}, 0, 0, s},
		{"waits for its own retry-after", []time.Duration{20 * s}, 0, 0, 20 * s},
		{"the soonest of two hints", []time.Duration{60 * s, 10 * s}, 1, 1, 10 * s},
		{"not one hint shorter than another candidate's", []time.Duration{60 * s, 2 * s}, 1, 1, 2 * s},
		{"skips a candidate never retried", []time.Duration{-1, 0}, 1, 1, s},
		{"nothing to retry", []time.Duration{-1, -1}, 0, -1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if pick, wait := recoveryPick(c.readyIn, c.failed, s); pick != c.pick || wait != c.wait {
				t.Fatalf("pick = %d %s, want %d %s", pick, wait, c.pick, c.wait)
			}
		})
	}
}

// A candidate's Retry-After counts from its own failure, so it ages while other candidates are tried;
// a candidate capacity rejected for good is not retried.
func TestOutageReadinessAgesAndSkipsExhausted(t *testing.T) {
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	var o outage
	o.fail(0, t0, t0, 10*time.Second, errors.New("a"))
	o.fail(1, t0.Add(8*time.Second), t0.Add(8*time.Second), 20*time.Second, errors.New("b"))
	got := o.readyIn(3, nil, t0.Add(8*time.Second))
	if want := []time.Duration{2 * time.Second, 20 * time.Second, -1}; !reflect.DeepEqual(got, want) {
		t.Fatalf("readyIn = %v, want %v", got, want)
	}
	if got := o.readyIn(3, map[int]bool{0: true}, t0.Add(30*time.Second)); !reflect.DeepEqual(got, []time.Duration{-1, 0, -1}) {
		t.Fatalf("readyIn with 0 exhausted = %v", got)
	}
	if o.start != t0 || o.failures != 2 || o.last.Error() != "b" {
		t.Fatalf("outage = %+v", o)
	}
}

// A wait that passes the outage window or reaches the deadline is refused with that reason, before
// anything sleeps.
func TestRecoveryStopsAtWindowDeadlineAndDisabled(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	p := Recovery{BaseDelay: time.Second, MaxDelay: 4 * time.Second, MaxOutage: time.Minute}
	cases := []struct {
		name     string
		pick     int
		wait     time.Duration
		start    time.Time
		deadline time.Time
		stop     string
	}{
		{"wait", 0, time.Second, now, time.Time{}, ""},
		{"within the window", 0, 59 * time.Second, now.Add(-time.Second), now.Add(time.Hour), ""},
		{"outage window", 0, time.Second, now.Add(-time.Minute), time.Time{}, RecoveryStopOutage},
		{"retry-after past the outage window", 0, 2 * time.Minute, now, time.Time{}, RecoveryStopOutage},
		{"deadline", 0, 20 * time.Second, now, now.Add(10 * time.Second), RecoveryStopDeadline},
		{"exactly at the deadline", 0, time.Second, now, now.Add(time.Second), RecoveryStopDeadline},
		{"no candidate", -1, 0, now, time.Time{}, RecoveryStopNoCandidate},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if stop := p.stopFor(c.pick, c.wait, c.start, now, c.deadline, 0); stop != c.stop {
				t.Fatalf("stopFor = %q, want %q", stop, c.stop)
			}
		})
	}
	if stop := (Recovery{}).stopFor(0, 0, now, now, time.Time{}, 0); stop != RecoveryStopDisabled {
		t.Fatalf("the zero Recovery retried: %q", stop)
	}
}

// A recovery decision replays from its inputs, and a changed output is caught (I7).
func TestRecoveryDecisionReplays(t *testing.T) {
	cands := []Candidate{{Ref: "t/a", Context: 8000}, {Ref: "t/b", Context: 8000}}
	next, wait, d := RecoveryDecision(cands, 1, protocol.ClassAvailability, []time.Duration{2 * time.Second, 1500 * time.Millisecond}, time.Second)
	if next != 1 || wait != 1500*time.Millisecond || d.Rule != RuleFailoverRecovery {
		t.Fatalf("decision = %d %s %s", next, wait, d.Output)
	}
	if err := ReplayDecision(d); err != nil {
		t.Fatalf("replay: %v", err)
	}
	d.Output = []byte(`{"next":0,"waitMs":1500}`)
	if err := ReplayDecision(d); err == nil {
		t.Fatal("a changed recovery output replayed")
	}
	// A sub-millisecond backoff is recorded as 1 ms, never 0: the wait between retries is kept.
	if _, wait, _ := RecoveryDecision(cands[:1], 0, protocol.ClassAvailability, []time.Duration{0}, 750*time.Microsecond); wait != time.Millisecond {
		t.Fatalf("a 750µs backoff waited %s", wait)
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
		if r["failure"] != wantReasons[i] || r["retry"] != i+1 || r["failedRequests"] != i+1 {
			t.Errorf("retry event %d = %v", i, r)
		}
	}
	// delayMs is what is left of the Retry-After after the failure was handled, so it depends on
	// scheduling; the request gap above proves the whole Retry-After passed.
	if r := retries[1]; r["status"] != 429 || r["retryAfterMs"] != int64(120) || r["delayMs"].(int64) <= 0 {
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
		Recovery: Recovery{BaseDelay: 20 * time.Millisecond, MaxDelay: 20 * time.Millisecond, MaxRetries: 100, MaxOutage: 100 * time.Millisecond}}
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
	if result.Status != StatusInterrupted || !errors.As(err, &rec) || rec.Stop != RecoveryStopDeadline || rec.Wait < 59*time.Minute {
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

// Failover comes first: the chain tries every candidate before the step waits, then the retries rotate
// over the unavailable candidates, and every step's decision log leads to the model that answered.
func TestRetrySweepsFromTheFirstCandidate(t *testing.T) {
	const a, b = config.ModelRef("test/a"), config.ModelRef("test/b")
	pa := &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0), answerOf("done")}}
	pb := &flakyProvider{script: []any{unavailable(protocol.ReasonServerError, 502, 0)}}
	health := NewHealth(nil)
	store := envStore("sw")
	loop := Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}, Health: health}
	opts := Options{Session: a, Failover: []config.ModelRef{b}, MaxSteps: 2, RunID: "r",
		Recovery: Recovery{BaseDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxOutage: time.Minute}}
	result, err := loop.Run(context.Background(), "sw", opts)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(pa.seen) != 2 || len(pb.seen) != 1 {
		t.Fatalf("requests a=%d b=%d, want a, b, then a again", len(pa.seen), len(pb.seen))
	}
	if steps := replayAll(t, store); !hasRule(steps[0].Decisions, RuleFailoverRecovery) {
		t.Fatalf("no recovery decision in %+v", steps[0].Decisions)
	}
}

// A chain that ends in capacity is still waited out when an earlier candidate failed with availability:
// that candidate may answer once it recovers (review F1).
func TestCapacityAtTheEndOfAnOutageIsWaitedOut(t *testing.T) {
	const a, b = config.ModelRef("test/a"), config.ModelRef("test/b")
	tooLarge := func() *protocol.ProviderError {
		return &protocol.ProviderError{Class: protocol.ClassCapacity, Status: 400, Err: errors.New("too large")}
	}
	pa := &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0), answerOf("done")}}
	pb := &flakyProvider{script: []any{tooLarge(), tooLarge()}}
	mb := testModel(b, pb)
	mb.Context *= 2
	store := withOldRows(envStore("cap"))
	loop := Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: mb}, Store: store, Tools: fakeTools{}}
	opts := Options{Session: a, Failover: []config.ModelRef{b}, MaxSteps: 2, RunID: "r",
		Recovery: Recovery{BaseDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxOutage: time.Minute}}
	result, err := loop.Run(context.Background(), "cap", opts)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(pa.seen) != 2 || len(pb.seen) != 2 {
		t.Fatalf("requests a=%d b=%d, want a, b, b halved, then a again", len(pa.seen), len(pb.seen))
	}
	replayAll(t, store)
}

// A candidate that capacity rejected for good is not sent again while the step waits out another's
// outage, even though it failed with availability before (review F2): a and its halved retry are
// rejected once, then only b is retried until it answers.
func TestCapacityRejectedCandidateIsNotRetried(t *testing.T) {
	const a, b = config.ModelRef("test/a"), config.ModelRef("test/b")
	tooLarge := &protocol.ProviderError{Class: protocol.ClassCapacity, Status: 400, Err: errors.New("too large")}
	pa := &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0), tooLarge, tooLarge, answerOf("from a")}}
	pb := &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0), unavailable(protocol.ReasonOverloaded, 503, 0), answerOf("done")}}
	store := withOldRows(envStore("ex"))
	loop := Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}}
	opts := Options{Session: a, Failover: []config.ModelRef{b}, MaxSteps: 2, RunID: "r",
		Recovery: Recovery{BaseDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxOutage: time.Minute}}
	result, err := loop.Run(context.Background(), "ex", opts)
	if err != nil || result.Status != StatusCompleted || result.Final != "done" {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(pa.seen) != 3 || len(pb.seen) != 3 {
		t.Fatalf("requests a=%d b=%d, want a, b, a, a halved, b, b", len(pa.seen), len(pb.seen))
	}
	replayAll(t, store)
}

// Each candidate is retried no earlier than its own Retry-After: a short hint on one candidate does not
// release another that asked for longer (review F3).
func TestEachCandidateKeepsItsOwnRetryAfter(t *testing.T) {
	const a, b = config.ModelRef("test/a"), config.ModelRef("test/b")
	pa := &flakyProvider{script: []any{unavailable(protocol.ReasonRateLimited, 429, 150*time.Millisecond), answerOf("done")}}
	pb := &flakyProvider{script: []any{unavailable(protocol.ReasonRateLimited, 429, time.Hour)}}
	store := envStore("ra")
	loop := Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}}
	opts := Options{Session: a, Failover: []config.ModelRef{b}, MaxSteps: 2, RunID: "r",
		Recovery: Recovery{BaseDelay: 10 * time.Millisecond, MaxDelay: 10 * time.Millisecond, MaxOutage: time.Minute}}
	result, err := loop.Run(context.Background(), "ra", opts)
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(pa.seen) != 2 || len(pb.seen) != 1 {
		t.Fatalf("requests a=%d b=%d, want a, b, then a once its Retry-After passed", len(pa.seen), len(pb.seen))
	}
	if gap := pa.at[1].Sub(pa.at[0]); gap < 150*time.Millisecond {
		t.Fatalf("a was retried %s after it asked for 150ms", gap)
	}
	replayAll(t, store)
}

// withOldRows adds older transcript rows, so a capacity retry with half the transcript still fits and
// is sent.
func withOldRows(store *memoryStore) *memoryStore {
	for i := range 20 {
		store.messages = append(store.messages, protocol.Message{Role: "user", Content: fmt.Sprintf("old-%d %s", i, strings.Repeat("x", 200))})
	}
	return store
}
