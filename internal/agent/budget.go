package agent

import (
	"errors"
	"fmt"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// ErrBudgetExhausted is the error of a run its budget stopped (ADR-0019 §6).
var ErrBudgetExhausted = errors.New("run budget exhausted")

// Spend is what one run used against its budget (ADR-0019 §6). Tokens and dollars are split by how
// they are known: Known from the provider's usage; Estimated from the request's own token estimate
// when a response carried no usage or a request failed after it was sent (its billing is not
// reported); dollars of a request to a model with no configured price are not added anywhere and the
// request is counted in UnknownCostRequests, so an unknown cost is never shown as zero.
type Spend struct {
	ToolCalls           int     `json:"toolCalls"`
	ModelRequests       int     `json:"modelRequests"`
	KnownTokens         int64   `json:"knownTokens"`
	EstimatedTokens     int64   `json:"estimatedTokens"`
	KnownCostUSD        float64 `json:"knownCostUsd"`
	EstimatedCostUSD    float64 `json:"estimatedCostUsd"`
	UnknownCostRequests int     `json:"unknownCostRequests"`
	// warned: budget.warning was reported for this run.
	warned bool
}

func (s *Spend) tokens() int64 { return s.KnownTokens + s.EstimatedTokens }

func (s *Spend) cost() float64 { return s.KnownCostUSD + s.EstimatedCostUSD }

// toolRoom is the tool calls the budget still admits, or -1 for no limit.
func (s *Spend) toolRoom(b config.Budget) int {
	if b.MaxToolCalls <= 0 {
		return -1
	}
	return max(0, b.MaxToolCalls-s.ToolCalls)
}

// admitTools is the exit of a run whose budget admits no further tool call. It is checked before the
// next model request: the reply could only ask for tools the run may not execute.
func (s *Spend) admitTools(b config.Budget) *ExitCause {
	if s.toolRoom(b) != 0 {
		return nil
	}
	return budgetExit(KindBudgetToolCalls, "the run executed %d tool calls, its limit", s.ToolCalls)
}

// admitRequest is the exit of a run whose budget does not admit a request of input estimated tokens to
// m, or nil. The request's output is not known before it is sent, so a sent request may overshoot the
// token and cost limits by its reply.
func (s *Spend) admitRequest(b config.Budget, m Model, input int) *ExitCause {
	if b.MaxModelRequests > 0 && s.ModelRequests >= b.MaxModelRequests {
		return budgetExit(KindBudgetModelRequests, "the run sent %d model requests, its limit", s.ModelRequests)
	}
	if b.TokenBudget > 0 && s.tokens()+int64(input) > b.TokenBudget {
		return budgetExit(KindBudgetTokens, "the run used %d tokens (%d known, %d estimated) and the next request needs about %d more; the budget is %d",
			s.tokens(), s.KnownTokens, s.EstimatedTokens, input, b.TokenBudget)
	}
	if b.CostLimitUSD > 0 {
		if m.Price == nil {
			return budgetExit(KindBudgetCost, "model %s has no configured cost, so the run's cost limit of $%.4f cannot admit it; set the model's cost or remove the limit",
				m.Ref, b.CostLimitUSD)
		}
		next := float64(input) * m.Price.Input / 1e6
		if s.cost()+next > b.CostLimitUSD {
			return budgetExit(KindBudgetCost, "the run cost $%.4f ($%.4f known, $%.4f estimated) and the next request's input costs about $%.4f; the limit is $%.4f",
				s.cost(), s.KnownCostUSD, s.EstimatedCostUSD, next, b.CostLimitUSD)
		}
	}
	return nil
}

// record settles one sent request to m. usage is the provider's (zero for a failed request or a
// response that reported none); input and output are the request's and the reply's estimated tokens,
// used only when usage is missing.
func (s *Spend) record(m Model, usage protocol.Usage, input, output int) {
	s.ModelRequests++
	in, out := usage.PromptTokens, usage.CompletionTokens
	known := in > 0 || out > 0 || usage.TotalTokens > 0
	if known {
		total := usage.TotalTokens
		if total < in+out {
			total = in + out
		}
		s.KnownTokens += total
		if in+out == 0 {
			// Only a total was reported: price all of it at the higher of the two rates.
			in = total
			if m.Price != nil && m.Price.Output > m.Price.Input {
				in, out = 0, total
			}
		}
	} else {
		in, out = int64(input), int64(output)
		s.EstimatedTokens += in + out
	}
	if m.Price == nil {
		s.UnknownCostRequests++
		return
	}
	cost := float64(in)*m.Price.Input/1e6 + float64(out)*m.Price.Output/1e6
	if known {
		s.KnownCostUSD += cost
	} else {
		s.EstimatedCostUSD += cost
	}
}

// warn reports, once per run, whether the cost warning is due: known and estimated cost reached it, or
// a request went to a model whose cost is unknown.
func (s *Spend) warn(b config.Budget) bool {
	if b.CostWarningUSD <= 0 || s.warned {
		return false
	}
	if s.cost() >= b.CostWarningUSD || s.UnknownCostRequests > 0 {
		s.warned = true
		return true
	}
	return false
}

// budgetWarning reports budget.warning the first time the run's cost warning is due.
func (l *Loop) budgetWarning(sessionID string, step int64, b config.Budget, s *Spend) {
	if !s.warn(b) {
		return
	}
	data := s.view(b)
	data["step"] = step
	l.emit("budget.warning", sessionID, data)
}

// view is the spend and the limits it is measured against, for events and the diagnostics log.
func (s *Spend) view(b config.Budget) map[string]any {
	return map[string]any{
		"toolCalls": s.ToolCalls, "modelRequests": s.ModelRequests,
		"knownTokens": s.KnownTokens, "estimatedTokens": s.EstimatedTokens,
		"knownCostUsd": s.KnownCostUSD, "estimatedCostUsd": s.EstimatedCostUSD,
		"unknownCostRequests": s.UnknownCostRequests, "budget": b,
	}
}

func budgetExit(kind, format string, args ...any) *ExitCause {
	return &ExitCause{Status: StatusInterrupted, Kind: kind, Err: fmt.Errorf("%w: "+format, append([]any{ErrBudgetExhausted}, args...)...)}
}
