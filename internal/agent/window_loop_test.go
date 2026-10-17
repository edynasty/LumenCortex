package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// rejectingProvider fails its first request with an error of class, then answers from script.
type rejectingProvider struct {
	scriptedProvider
	class  protocol.ErrorClass
	failed bool
}

func (p *rejectingProvider) Complete(ctx context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	if !p.failed {
		p.failed = true
		p.mu.Lock()
		p.seen = append(p.seen, req)
		p.mu.Unlock()
		return protocol.ProviderResponse{}, &protocol.ProviderError{Class: p.class, Err: errors.New("rejected")}
	}
	return p.scriptedProvider.Complete(ctx, req)
}

// oldRows is n user rows of about 60 tokens each.
func oldRows(n int) []protocol.Message {
	rows := make([]protocol.Message, n)
	for i := range rows {
		rows[i] = protocol.Message{Role: "user", Content: fmt.Sprintf("old-%d %s", i, strings.Repeat("y", 200))}
	}
	return rows
}

func transcriptTokens(req protocol.ProviderRequest) int {
	tokens := 0
	for _, m := range req.Messages[2:] {
		tokens += window.Estimate(m)
	}
	return tokens
}

func replayAll(t *testing.T, store *memoryStore) []stepRecord {
	t.Helper()
	var out []stepRecord
	for step := int64(1); step <= int64(len(store.steps)); step++ {
		raw, err := json.Marshal(store.steps[step])
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyStep(raw); err != nil {
			t.Fatalf("step %d: %v\n%s", step, err, raw)
		}
		out = append(out, store.steps[step].(stepRecord))
	}
	return out
}

func hasRule(decisions []Decision, rule string) bool {
	return slices.ContainsFunc(decisions, func(d Decision) bool { return d.Rule == rule })
}

// A capacity rejection is retried once on the same model with at most half the transcript tokens,
// the limit holds for that model for the rest of the run, and the retry is a replayable decision.
func TestLoopCapacityRetryHalvesTranscript(t *testing.T) {
	store := &memoryStore{state: SessionState{ID: "cap", Goal: "cap", Status: "running"}, steps: map[int64]any{}, messages: oldRows(60)}
	provider := &rejectingProvider{class: protocol.ClassCapacity, scriptedProvider: scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("c1", "shell", `{"command":"ok"}`)}}, FinishReason: "tool_calls"},
		{Message: protocol.Message{Content: "done"}, FinishReason: "stop"},
	}}}
	store.seed(t, nil, nil)
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "cap", Options{Session: testRef, MaxSteps: 4})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(provider.seen) != 3 {
		t.Fatalf("requests=%d, want rejected + retry + next step", len(provider.seen))
	}
	rejected, retry, next := transcriptTokens(provider.seen[0]), transcriptTokens(provider.seen[1]), transcriptTokens(provider.seen[2])
	if retry > rejected/2+1 {
		t.Fatalf("retry holds %d transcript tokens, want at most half of %d", retry, rejected)
	}
	if next > retry+window.Estimate(provider.seen[2].Messages[len(provider.seen[2].Messages)-1])+window.Estimate(provider.seen[2].Messages[len(provider.seen[2].Messages)-2]) {
		t.Fatalf("next step holds %d transcript tokens, want the retry limit (%d) kept", next, retry)
	}
	steps := replayAll(t, store)
	if !hasRule(steps[0].Decisions, RuleWindowCapacityRetry) || !hasRule(steps[0].Decisions, RuleWindowDropped) {
		t.Fatalf("step 1 decisions %+v, want capacity retry and dropped", steps[0].Decisions)
	}
}

// Each failover candidate gets a window built with its own budget: the small model's request is cut,
// the large model's request holds the whole transcript.
func TestLoopWindowPerCandidate(t *testing.T) {
	const small, large = config.ModelRef("test/small"), config.ModelRef("test/large")
	ps := &rejectingProvider{class: protocol.ClassAvailability}
	pl := &scriptedProvider{responses: []protocol.ProviderResponse{{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}}}
	ms, ml := testModel(small, ps), testModel(large, pl)
	ms.Context, ms.Output = 4000, 500
	ml.Context, ml.Output = 64000, 4096
	store := &memoryStore{state: SessionState{ID: "cand", Goal: "cand", Status: "running"}, steps: map[int64]any{}, messages: oldRows(100)}
	store.seed(t, nil, nil)
	loop := Loop{Models: map[config.ModelRef]Model{small: ms, large: ml}, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "cand", Options{Session: small, Failover: []config.ModelRef{large}, MaxSteps: 2})
	if err != nil || result.Status != StatusCompleted {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(ps.seen) != 1 || len(pl.seen) != 1 {
		t.Fatalf("requests small=%d large=%d", len(ps.seen), len(pl.seen))
	}
	if got := len(ps.seen[0].Messages) - 2; got == 0 || got >= 100 {
		t.Fatalf("small model got %d transcript rows, want a cut window", got)
	}
	if got := len(pl.seen[0].Messages) - 2; got != 100 {
		t.Fatalf("large model got %d transcript rows, want all 100", got)
	}
	steps := replayAll(t, store)
	if steps[0].Window.Dropped != 0 || steps[0].Window.FirstSeq != 0 {
		t.Fatalf("answered window %+v, want the large model's full window", steps[0].Window)
	}
}

// A transcript that cannot be split into groups ends the run failed with transcript_unpaired before
// any request.
func TestLoopUnpairedTranscriptFails(t *testing.T) {
	store := &memoryStore{state: SessionState{ID: "orphan", Goal: "orphan", Status: "running"}, steps: map[int64]any{},
		messages: []protocol.Message{{Role: "user", Content: "go"}, {Role: "tool", ToolCallID: "x", Content: "orphan"}}}
	store.seed(t, nil, nil)
	provider := &scriptedProvider{}
	loop := Loop{Models: testModels(provider), Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "orphan", Options{Session: testRef, MaxSteps: 2})
	if err == nil || result.Status != StatusFailed || !errors.Is(err, window.ErrUnpaired) {
		t.Fatalf("result %+v err %v", result, err)
	}
	if store.cause["kind"] != KindTranscriptUnpaired {
		t.Fatalf("error_json %+v, want kind %s", store.cause, KindTranscriptUnpaired)
	}
	if len(provider.seen) != 0 {
		t.Fatalf("requests=%d, want none", len(provider.seen))
	}
}

// Trimming the groups no window can include never changes a window: for every budget up to the
// largest context, the window built from the trimmed transcript equals the one built from all of it.
func TestRunGroupsTrimKeepsEveryWindow(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 7))
	trimmedTrials := 0
	for trial := range 200 {
		var full []window.Entry
		for seq := int64(0); len(full) < 40; seq++ {
			if rng.IntN(2) == 0 {
				full = append(full, window.Entry{Seq: seq, Msg: protocol.Message{Role: "user", Content: strings.Repeat("u", rng.IntN(400))}})
				continue
			}
			id := fmt.Sprintf("t%d", seq)
			full = append(full, window.Entry{Seq: seq, Msg: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: "shell", Arguments: json.RawMessage(`{}`)}}}})
			seq++
			full = append(full, window.Entry{Seq: seq, Msg: protocol.Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("t", rng.IntN(800))}})
		}
		maxContext := 200 + rng.IntN(3000)
		r := &run{}
		r.transcript = slices.Clone(full)
		groups, err := r.groups(maxContext)
		if err != nil {
			t.Fatal(err)
		}
		allGroups, _ := window.Groups(full)
		if len(groups)+r.trimmed != len(allGroups) {
			t.Fatalf("trial %d: %d kept + %d trimmed != %d groups", trial, len(groups), r.trimmed, len(allGroups))
		}
		state := protocol.Message{Role: "user", Content: "state"}
		for context := 0; context <= maxContext; context += 37 {
			b := window.Budget{Context: context + window.Estimate(state)}
			want, wantErr := window.Build(window.Input{State: state, Transcript: full}, b)
			got, gotErr := window.Build(window.Input{State: state, Transcript: r.transcript}, b)
			if (wantErr == nil) != (gotErr == nil) {
				t.Fatalf("trial %d context %d: full err %v, trimmed err %v (trimmed %d)", trial, context, wantErr, gotErr, r.trimmed)
			}
			if wantErr == nil && (want.FirstSeq != got.FirstSeq || len(want.Messages) != len(got.Messages) || want.Dropped != got.Dropped+r.trimmed) {
				t.Fatalf("trial %d context %d: full %d/%d rows, trimmed %d/%d rows (trimmed %d)", trial, context, want.FirstSeq, len(want.Messages), got.FirstSeq, len(got.Messages), r.trimmed)
			}
		}
		if r.trimmed > 0 {
			trimmedTrials++
		}
	}
	if trimmedTrials < 50 {
		t.Fatalf("only %d of 200 trials trimmed anything", trimmedTrials)
	}
}
