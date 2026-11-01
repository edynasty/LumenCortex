package agent

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

func okTools() *scriptTools {
	return &scriptTools{fn: func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true, Content: "ok"}, nil
	}}
}

// toolCallsWith answers every request with one bash call that reports usage.
func toolCallsWith(usage protocol.Usage) *funcProvider {
	return &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		r := shellCall(fmt.Sprintf("t%d", n))
		r.Usage = usage
		return r, nil
	}}
}

func assertBudgetExit(t *testing.T, store *memoryStore, result Result, err error, kind string) {
	t.Helper()
	assertExit(t, store, result, StatusInterrupted, kind)
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
}

// A reply asking for more calls than the budget has left runs only the calls that fit, stores only
// those (the transcript stays paired), and the run ends before its next request.
func TestBudgetToolCallsEndsTheRunBeforeTheNextRequest(t *testing.T) {
	store := newState("bt")
	tools := okTools()
	provider := &funcProvider{fn: func(_ context.Context, n int, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			call(fmt.Sprintf("s%d-a", n), "bash", `{"command":"x"}`),
			call(fmt.Sprintf("s%d-b", n), "bash", `{"command":"y"}`),
		}}}, nil
	}}
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: tools}).Run(context.Background(), "bt",
		Options{Session: testRef, MaxSteps: 10, Budget: config.Budget{MaxToolCalls: 3}})
	assertBudgetExit(t, store, result, err, KindBudgetToolCalls)
	if provider.n != 2 || tools.executed != 3 || result.Spend.ToolCalls != 3 || result.Spend.ModelRequests != 2 {
		t.Fatalf("requests=%d executed=%d spend=%+v, want 2 requests and 3 tool calls", provider.n, tools.executed, result.Spend)
	}
	stored, answered := map[string]bool{}, map[string]bool{}
	for _, m := range store.messages {
		for _, c := range m.ToolCalls {
			stored[c.ID] = true
		}
		if m.Role == "tool" {
			answered[m.ToolCallID] = true
		}
	}
	if stored["s2-b"] || answered["s2-b"] {
		t.Fatal("the call past the budget was stored or answered")
	}
	for id := range stored {
		if !answered[id] {
			t.Fatalf("call %s is stored without a result: the transcript is unpaired", id)
		}
	}
}

// Every attempt is a request: a failover is refused once the request limit is reached, and the failed
// attempt's input is spent as an estimate (a sent request may be billed).
func TestBudgetModelRequestsCountsFailoverAttempts(t *testing.T) {
	const a, b = config.ModelRef("test/a"), config.ModelRef("test/b")
	pa := &sequenceProvider{errs: []error{&protocol.ProviderError{Class: protocol.ClassAvailability, Err: errors.New("overloaded")}}}
	pb := &scriptedProvider{responses: []protocol.ProviderResponse{answerOf("done")}}
	store := newState("bm")
	loop := Loop{Models: map[config.ModelRef]Model{a: testModel(a, pa), b: testModel(b, pb)}, Store: store, Tools: fakeTools{}}
	result, err := loop.Run(context.Background(), "bm", Options{Session: a, Failover: []config.ModelRef{b}, MaxSteps: 3,
		Budget: config.Budget{MaxModelRequests: 1}})
	assertBudgetExit(t, store, result, err, KindBudgetModelRequests)
	if pa.calls != 1 || len(pb.seen) != 0 {
		t.Fatalf("calls a=%d b=%d, want the failover refused", pa.calls, len(pb.seen))
	}
	s := result.Spend
	if s.ModelRequests != 1 || s.KnownTokens != 0 || s.EstimatedTokens <= 0 || s.UnknownCostRequests != 1 {
		t.Fatalf("spend = %+v, want one request with estimated tokens and an unknown cost", s)
	}
}

// Provider usage counts as known tokens; a request is refused when its estimated input would pass
// the budget, so the request that crossed it was the last one sent.
func TestBudgetTokensRefusesTheRequestPastTheBudget(t *testing.T) {
	store := newState("bk")
	provider := toolCallsWith(protocol.Usage{PromptTokens: 80_000, CompletionTokens: 20_000, TotalTokens: 100_000})
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: okTools()}).Run(context.Background(), "bk",
		Options{Session: testRef, MaxSteps: 10, Budget: config.Budget{TokenBudget: 150_000}})
	assertBudgetExit(t, store, result, err, KindBudgetTokens)
	if provider.n != 2 || result.Spend.KnownTokens != 200_000 || result.Spend.EstimatedTokens != 0 {
		t.Fatalf("requests=%d spend=%+v, want 2 requests and 200000 known tokens", provider.n, result.Spend)
	}
}

// A cost limit sends nothing to a model whose cost is unknown: unknown is never zero.
func TestBudgetCostLimitRefusesAModelWithoutAPrice(t *testing.T) {
	store := newState("bu")
	provider := toolCallsWith(protocol.Usage{PromptTokens: 10, CompletionTokens: 10, TotalTokens: 20})
	result, err := (&Loop{Models: testModels(provider), Store: store, Tools: okTools()}).Run(context.Background(), "bu",
		Options{Session: testRef, MaxSteps: 5, Budget: config.Budget{CostLimitUSD: 5}})
	assertBudgetExit(t, store, result, err, KindBudgetCost)
	if provider.n != 0 || result.Spend.ModelRequests != 0 || !strings.Contains(err.Error(), "no configured cost") {
		t.Fatalf("requests=%d spend=%+v err=%v, want nothing sent", provider.n, result.Spend, err)
	}
}

// Known cost comes from usage at the model's price. The warning is reported once; the limit refuses
// the request after the one that crossed it, and that one's overshoot is reported, not hidden.
func TestBudgetCostWarningAndLimit(t *testing.T) {
	store := newState("bc")
	// $1 per million input and $2 per million output: 400k in + 100k out is $0.60 a request.
	provider := toolCallsWith(protocol.Usage{PromptTokens: 400_000, CompletionTokens: 100_000, TotalTokens: 500_000})
	model := testModel(testRef, provider)
	model.Price = &config.Cost{Input: 1, Output: 2}
	var warnings, spends int
	emit := func(e Event) {
		switch e.Type {
		case "budget.warning":
			warnings++
		case "budget.spend":
			spends++
		}
	}
	result, err := (&Loop{Models: map[config.ModelRef]Model{testRef: model}, Store: store, Tools: okTools(), Emit: emit}).Run(context.Background(), "bc",
		Options{Session: testRef, MaxSteps: 10, Budget: config.Budget{CostWarningUSD: 0.5, CostLimitUSD: 1}})
	assertBudgetExit(t, store, result, err, KindBudgetCost)
	if provider.n != 2 || math.Abs(result.Spend.KnownCostUSD-1.2) > 1e-9 || result.Spend.EstimatedCostUSD != 0 {
		t.Fatalf("requests=%d spend=%+v, want 2 requests and $1.20 known", provider.n, result.Spend)
	}
	if warnings != 1 || spends != 1 {
		t.Fatalf("budget.warning=%d budget.spend=%d, want 1 each", warnings, spends)
	}
}

// A response without usage is spent as an estimate at the model's price, never as zero; a model with
// no price is counted as an unknown-cost request with no dollars.
func TestBudgetMissingUsageIsEstimatedAndMissingPriceIsUnknown(t *testing.T) {
	for _, priced := range []bool{true, false} {
		store := newState("be")
		provider := toolCallsWith(protocol.Usage{})
		model := testModel(testRef, provider)
		if priced {
			model.Price = &config.Cost{Input: 3, Output: 15}
		}
		result, err := (&Loop{Models: map[config.ModelRef]Model{testRef: model}, Store: store, Tools: okTools()}).Run(context.Background(), "be",
			Options{Session: testRef, MaxSteps: 5, Budget: config.Budget{MaxModelRequests: 1}})
		assertBudgetExit(t, store, result, err, KindBudgetModelRequests)
		s := result.Spend
		if s.KnownTokens != 0 || s.EstimatedTokens <= 0 || s.KnownCostUSD != 0 {
			t.Fatalf("priced=%v spend=%+v, want estimated tokens only", priced, s)
		}
		if priced && (s.EstimatedCostUSD <= 0 || s.UnknownCostRequests != 0) {
			t.Fatalf("priced spend = %+v, want an estimated cost", s)
		}
		if !priced && (s.EstimatedCostUSD != 0 || s.UnknownCostRequests != 1) {
			t.Fatalf("unpriced spend = %+v, want one unknown-cost request and no dollars", s)
		}
	}
}
