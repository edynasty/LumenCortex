package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// A short outage has a finite retry allowance even without an outage window or deadline.
func TestRecoveryFiniteWithoutOutageWindow(t *testing.T) {
	p := &flakyProvider{}
	store := envStore("finite")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := (&Loop{Models: testModels(p), Store: store, Tools: fakeTools{}}).Run(ctx, "finite", Options{
		Session: testRef, RunID: "r", Recovery: RecoveryFrom(config.Retry{BaseDelayMs: 1, MaxDelayMs: 1}),
	})
	var rec *RecoveryError
	if result.Status != StatusInterrupted || !errors.As(err, &rec) || rec.Stop != "max_retries" || len(p.seen) != 7 {
		t.Fatalf("finite recovery: status=%s requests=%d error=%v", result.Status, len(p.seen), err)
	}
	if rec.Retries != 6 || store.admissions != 7 || result.Usage.Requests != 7 {
		t.Fatalf("attempt accounting: retries=%d admissions=%d usage=%+v", rec.Retries, store.admissions, result.Usage)
	}
}

// A useful task can require more than 24 response/tool cycles without any failure.
func TestDefaultStepsAllowLongerTask(t *testing.T) {
	p := &scriptedProvider{}
	for i := range 30 {
		p.responses = append(p.responses, protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: fmt.Sprint(i), Name: "bash", Arguments: []byte(`{"command":"true"}`)}}}})
	}
	p.responses = append(p.responses, answer("review complete"))
	tools := &countingTools{}
	result, err := (&Loop{Models: testModels(p), Store: newState("long"), Tools: tools}).Run(context.Background(), "long", Options{Session: testRef})
	if err != nil || result.Status != StatusCompleted || result.Final != "review complete" || tools.calls.Load() != 30 {
		t.Fatalf("long task: result=%+v calls=%d err=%v", result, tools.calls.Load(), err)
	}
}

// Delivery evidence, not tool activity, determines whether another correction is useful.
func TestDeliveryProgressAllowsFurtherCorrection(t *testing.T) {
	store := newState("delivery-progress")
	store.ts.Delivery = &taskstate.DeliveryContract{}
	p := &scriptedProvider{responses: []protocol.ProviderResponse{answer("first"), answer("second"), answer("third"), answer("fourth"), answer("delivered")}}
	missing := [][]string{{"a", "b", "c", "d"}, {"b", "c", "d"}, {"c", "d"}, {"d"}, nil}
	calls := 0
	loop := &Loop{Models: testModels(p), Store: store, Tools: fakeTools{}, Delivery: func(context.Context, *taskstate.DeliveryContract) ([]string, error) {
		i := calls
		calls++
		if i >= len(missing) {
			return nil, nil
		}
		return missing[i], nil
	}}
	result, err := loop.Run(context.Background(), "delivery-progress", Options{Session: testRef, MaxSteps: 8})
	if err != nil || result.Status != StatusCompleted || result.Final != "delivered" {
		t.Fatalf("progressive delivery: %+v %v", result, err)
	}
}

// An explicit hint survives a response step/run boundary even if every candidate is unhealthy.
func TestRetryAfterSurvivesNewStep(t *testing.T) {
	p := &flakyProvider{script: []any{answer("back")}}
	health := NewHealth(nil)
	health.Fail(testRef, 50*time.Millisecond)
	started := time.Now()
	result, err := (&Loop{Models: testModels(p), Store: newState("cooldown"), Tools: fakeTools{}, Health: health}).Run(context.Background(), "cooldown", Options{Session: testRef})
	if err != nil || result.Final != "back" || len(p.at) != 1 {
		t.Fatalf("cooldown result=%+v err=%v requests=%d", result, err, len(p.at))
	}
	if elapsed := p.at[0].Sub(started); elapsed < 45*time.Millisecond {
		t.Fatalf("explicit Retry-After bypassed across step boundary: %s", elapsed)
	}
}

func TestCapacityAfterAvailabilityStopsAsCapacity(t *testing.T) {
	capacity := &protocol.ProviderError{Class: protocol.ClassCapacity, Err: errors.New("context full")}
	p := &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0), capacity, capacity}}
	store := withOldRows(envStore("capacity-last"))
	result, err := (&Loop{Models: testModels(p), Store: store, Tools: fakeTools{}}).Run(context.Background(), "capacity-last", Options{
		Session: testRef, RunID: "r", Recovery: Recovery{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxRetries: 1},
	})
	if result.Status != StatusFailed || store.cause["kind"] != KindProviderCapacity || len(p.seen) != 3 {
		t.Fatalf("capacity terminal: result=%+v cause=%v requests=%d err=%v", result, store.cause, len(p.seen), err)
	}
}

func TestRecoveryLimitIsSharedAcrossCandidates(t *testing.T) {
	const a, b = config.ModelRef("t/a"), config.ModelRef("t/b")
	pa, pb := &flakyProvider{}, &flakyProvider{}
	store := envStore("shared")
	_, err := (&Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}}).Run(context.Background(), "shared", Options{
		Session: a, Failover: []config.ModelRef{b}, RunID: "r", Recovery: Recovery{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxRetries: 3},
	})
	var rec *RecoveryError
	if !errors.As(err, &rec) || rec.Stop != RecoveryStopMaxRetries || rec.Retries != 3 || len(pa.seen)+len(pb.seen) != 5 || store.admissions != 5 {
		t.Fatalf("shared retry allowance: a=%d b=%d admissions=%d error=%v", len(pa.seen), len(pb.seen), store.admissions, err)
	}
}

func TestSuccessfulStepRenewsRecoveryAllowance(t *testing.T) {
	failure := unavailable(protocol.ReasonOverloaded, 503, 0)
	p := &flakyProvider{script: []any{failure, protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "once", Name: "bash", Arguments: []byte(`{"command":"true"}`)}}}}, failure, answer("complete")}}
	tools := &countingTools{}
	store := envStore("renew")
	result, err := (&Loop{Models: testModels(p), Store: store, Tools: tools}).Run(context.Background(), "renew", Options{
		Session: testRef, RunID: "r", MaxSteps: 2, Recovery: Recovery{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxRetries: 1},
	})
	if err != nil || result.Status != StatusCompleted || len(p.seen) != 4 || tools.calls.Load() != 1 {
		t.Fatalf("renewed allowance: %+v requests=%d tools=%d err=%v", result, len(p.seen), tools.calls.Load(), err)
	}
	replayAll(t, store)
}

func TestCooldownCannotPassDeadlineOrCancellation(t *testing.T) {
	for _, cancelWait := range []bool{false, true} {
		p := &flakyProvider{}
		h := NewHealth(nil)
		h.Fail(testRef, time.Hour)
		ctx, cancel := context.WithCancel(context.Background())
		opts := Options{Session: testRef}
		if !cancelWait {
			opts.Deadline = time.Now().Add(time.Second)
		}
		store := envStore("cooldown-stop")
		loop := &Loop{Models: testModels(p), Health: h, Store: store, Tools: fakeTools{}, Emit: func(e Event) {
			if e.Type == "provider.cooldown" {
				cancel()
			}
		}}
		result, err := loop.Run(ctx, "cooldown-stop", opts)
		cancel()
		if result.Status != StatusInterrupted || err == nil || len(p.seen) != 0 || store.admissions != 0 {
			t.Fatalf("cooldown stop: %+v err=%v requests=%d", result, err, len(p.seen))
		}
		if cancelWait && !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel lost: %v", err)
		}
	}
}

func TestCooldownSelectsReadyFallback(t *testing.T) {
	for _, fallbackDelay := range []time.Duration{0, 20 * time.Millisecond} {
		const a, b = config.ModelRef("t/a"), config.ModelRef("t/b")
		pa, pb := &flakyProvider{}, &flakyProvider{script: []any{unavailable(protocol.ReasonOverloaded, 503, 0), answer("fallback answered")}}
		h := NewHealth(nil)
		h.Fail(a, time.Hour)
		h.Fail(b, fallbackDelay)
		store := envStore("ready-fallback")
		result, err := (&Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}, Health: h}).Run(context.Background(), "ready-fallback", Options{Session: a, Failover: []config.ModelRef{b}, RunID: "r", Deadline: time.Now().Add(time.Second), Recovery: Recovery{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxRetries: 1}})
		if err != nil || result.Final != "fallback answered" || len(pa.seen) != 0 || len(pb.seen) != 2 || store.admissions != 2 {
			t.Fatalf("viable fallback delay=%s: result=%+v primary=%d fallback=%d err=%v", fallbackDelay, result, len(pa.seen), len(pb.seen), err)
		}
		replayAll(t, store)
	}
}

func TestCooldownPreservesViableFirstAttempt(t *testing.T) {
	for _, backoff := range []time.Duration{time.Millisecond, time.Second} {
		const a, b = config.ModelRef("t/a"), config.ModelRef("t/b")
		pa := &flakyProvider{script: []any{answer("primary answered")}}
		pb := &flakyProvider{}
		h := NewHealth(nil)
		h.Fail(a, 50*time.Millisecond)
		h.Fail(b, 0)
		store := envStore("viable-first")
		result, err := (&Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}, Health: h}).Run(context.Background(), "viable-first", Options{Session: a, Failover: []config.ModelRef{b}, RunID: "r", Deadline: time.Now().Add(500 * time.Millisecond), Recovery: Recovery{BaseDelay: backoff, MaxDelay: backoff, MaxRetries: 1}})
		if err != nil || result.Final != "primary answered" || len(pa.seen) != 1 || len(pb.seen) != 1 || store.admissions != 2 {
			t.Fatalf("first attempt lost backoff=%s result=%+v a=%d b=%d err=%v", backoff, result, len(pa.seen), len(pb.seen), err)
		}
		replayAll(t, store)
	}
}
