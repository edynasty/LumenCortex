package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

func TestUnlimitedStepsDoNotInferProgress(t *testing.T) {
	for _, limit := range []int{0, -1, 7} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			store := newState("unlimited")
			store.ts.Progress.SameFailureCount = 9
			store.ts.Progress.NoProgressSteps = 9
			p := &funcProvider{fn: func(ctx context.Context, n int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
				if _, ok := ctx.Deadline(); ok {
					t.Fatal("implicit global deadline")
				}
				if req.ReasoningEffort != "low" {
					t.Fatalf("historical progress changed effort to %s", req.ReasoningEffort)
				}
				if n == 141 {
					return answer("finished"), nil
				}
				return shellCall(fmt.Sprint(n)), nil
			}}
			tools := &scriptTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
				return protocol.ToolResult{OK: false, Content: `{"error":"same failure"}`}, nil
			}}
			result, err := (&Loop{Models: testModels(p), Store: store, Tools: tools}).Run(context.Background(), "unlimited", Options{Session: testRef, MaxSteps: limit, EffortDefault: config.EffortLow})
			if limit > 0 {
				if err == nil || p.n != limit || store.cause["kind"] != KindMaxSteps {
					t.Fatalf("explicit: %+v n=%d err=%v", result, p.n, err)
				}
			} else if err != nil || result.Final != "finished" || p.n != 141 {
				t.Fatalf("unlimited: %+v n=%d err=%v", result, p.n, err)
			}
			if store.ts.Progress.SameFailureCount != 9 || store.ts.Progress.NoProgressSteps != 9 {
				t.Fatal("runtime inferred progress from repeated tools")
			}
		})
	}
}

func TestToolBatchOnlyHonorsExplicitLimit(t *testing.T) {
	for _, limit := range []int{0, -1, 3} {
		calls := make([]protocol.ToolCall, 12)
		for i := range calls {
			calls[i] = call(fmt.Sprint(i), "bash", `{"command":"true"}`)
		}
		p := &scriptedProvider{responses: []protocol.ProviderResponse{toolStep(calls...), answer("done")}}
		tools := &countingTools{}
		r, err := (&Loop{Models: testModels(p), Store: newState("batch"), Tools: tools}).Run(context.Background(), "batch", Options{Session: testRef, MaxToolCallsPerStep: limit})
		want := int32(12)
		if limit > 0 {
			want = int32(limit)
		}
		if err != nil || r.Final != "done" || tools.calls.Load() != want {
			t.Fatalf("limit=%d tools=%d result=%+v err=%v", limit, tools.calls.Load(), r, err)
		}
	}
}

func TestCooledCandidateRejoinsAfterHealthyFailure(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		const a, b = config.ModelRef("t/a"), config.ModelRef("t/b")
		pa := &flakyProvider{script: []any{answer("a recovered")}}
		pb := &flakyProvider{}
		h := NewHealth(nil)
		h.Fail(a, 40*time.Millisecond)
		start := time.Now()
		store := envStore("rejoin")
		events := &eventLog{}
		opts := Options{Session: a, Failover: []config.ModelRef{b}, RunID: "r", Deadline: time.Now().Add(time.Second)}
		if recovering {
			opts.Recovery = Recovery{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond, MaxRetries: 1}
		}
		r, err := (&Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}, Health: h, Emit: events.emit}).Run(context.Background(), "rejoin", opts)
		if err != nil || r.Final != "a recovered" || len(pa.seen) != 1 || len(pb.seen) != 1 || store.admissions != 2 {
			t.Fatalf("recovery=%v result=%+v A=%d B=%d admissions=%d err=%v", recovering, r, len(pa.seen), len(pb.seen), store.admissions, err)
		}
		if pa.at[0].Sub(start) < 35*time.Millisecond {
			t.Fatal("A requested before explicit cooldown")
		}
		if len(events.of("provider.retry")) != 0 {
			t.Fatal("deferred first attempt counted as retry")
		}
		replayAll(t, store)
		for _, v := range store.steps {
			record := v.(stepRecord)
			for i, d := range record.Decisions {
				if d.Rule == RuleModelCandidatesRetained {
					record.Decisions[i].Output = json.RawMessage(`{"candidates":["t/a","t/b"]}`)
					raw, _ := json.Marshal(record)
					if VerifyStep(raw) == nil {
						t.Fatal("tampered candidate order accepted")
					}
					break
				}
			}
		}
	}
}

func TestCancellationAfterAdmissionSendsNoRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &flakyProvider{script: []any{answer("must not run")}}
	store := envStore("admitted-cancel")
	r, err := (&Loop{Models: testModels(p), Store: store, Tools: fakeTools{}, Emit: func(e Event) {
		if e.Type == "llm.request" {
			cancel()
		}
	}}).Run(ctx, "admitted-cancel", Options{Session: testRef, RunID: "r"})
	if err == nil || r.Status != StatusInterrupted || len(p.seen) != 0 || r.Usage.Requests != 0 {
		t.Fatalf("cancel result=%+v requests=%d err=%v", r, len(p.seen), err)
	}
	if got := states(store.envelopes()); fmt.Sprint(got) != "[unknown:cancelled]" {
		t.Fatalf("envelope=%v", got)
	}
}
