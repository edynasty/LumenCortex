// Package window implements the request-window builder from the M3.2 design: the model request is
//
//	System ++ [Task State] ++ <contiguous suffix of whole tool-call groups>
//
// chosen newest→oldest under a token budget. Groups are taken atomically (user alone; assistant plus
// its tool results), transcript selection starts at TaskState.windowStartSeq, and no group is skipped.
package window

import (
	"errors"
	"fmt"

	"github.com/edynasty/LumenCortex/protocol"
)

// Entry is one persisted transcript row. EchoTokens is the adapter-reported size of the run-memory echo
// attached to the row (0 when none); the echo bytes themselves never reach the builder.
type Entry struct {
	Seq        int64
	Msg        protocol.Message
	EchoTokens int
}

// Budget is expressed in tokens.
type Budget struct{ Context, Output, Margin int }

// Input is everything one model request is built from.
type Input struct {
	System     []protocol.Message // fixed prefix (system prompt)
	State      protocol.Message   // Task State block, role "user"; always included (I3)
	Transcript []Entry            // reconciled transcript from TaskState.windowStartSeq, ascending seq
	ToolTokens int                // estimate for tool specs
}

// Result is the built request.
type Result struct {
	Messages []protocol.Message
	FirstSeq int64 // seq of the oldest included transcript entry; -1 if none
	Dropped  int   // whole groups left out (oldest first)
	Tokens   int   // Σ System + State + ToolTokens + included groups
	// Available is the transcript budget and Costs the token cost of each group Select examined,
	// newest first: the included groups and, when one did not fit, that group. With Dropped they are
	// the inputs of the window.dropped decision.
	Available int
	Costs     []int
}

var (
	// ErrUnpaired: the transcript violates I2 (tool call / result pairing).
	ErrUnpaired = errors.New("window: unpaired tool call or tool result")
	// ErrOverflow: the fixed prefix alone exceeds the budget, or not even the
	// newest group fits.
	ErrOverflow = errors.New("window: context budget overflow")
)

// Groups splits the transcript into atomic groups. A group is one user
// message, or one assistant message plus the tool messages that immediately
// follow it and answer its calls. A tool message that answers no call of the
// immediately preceding assistant message, a duplicate answer, or an
// unanswered call is ErrUnpaired.
func Groups(t []Entry) ([][]Entry, error) {
	var groups [][]Entry
	for i := 0; i < len(t); {
		e := t[i]
		switch e.Msg.Role {
		case "user":
			groups = append(groups, t[i:i+1])
			i++
		case "assistant":
			pending := make(map[string]bool, len(e.Msg.ToolCalls))
			for _, tc := range e.Msg.ToolCalls {
				if tc.ID == "" || pending[tc.ID] {
					return nil, fmt.Errorf("%w: seq %d: empty or repeated tool call id %q", ErrUnpaired, e.Seq, tc.ID)
				}
				pending[tc.ID] = true
			}
			j := i + 1
			for ; j < len(t) && t[j].Msg.Role == "tool"; j++ {
				id := t[j].Msg.ToolCallID
				open, known := pending[id]
				if !known {
					return nil, fmt.Errorf("%w: seq %d: tool result %q answers no call of assistant seq %d", ErrUnpaired, t[j].Seq, id, e.Seq)
				}
				if !open {
					return nil, fmt.Errorf("%w: seq %d: duplicate tool result %q", ErrUnpaired, t[j].Seq, id)
				}
				pending[id] = false
			}
			for _, tc := range e.Msg.ToolCalls {
				if pending[tc.ID] {
					return nil, fmt.Errorf("%w: seq %d: tool call %q has no result", ErrUnpaired, e.Seq, tc.ID)
				}
			}
			groups = append(groups, t[i:j])
			i = j
		case "tool":
			return nil, fmt.Errorf("%w: seq %d: tool result %q does not follow an assistant message", ErrUnpaired, e.Seq, e.Msg.ToolCallID)
		default:
			return nil, fmt.Errorf("window: seq %d: unexpected transcript role %q", e.Seq, e.Msg.Role)
		}
	}
	return groups, nil
}

// Fixed is the part of every request outside the transcript budget: System, State and tool specs.
func Fixed(in Input) int {
	fixed := in.ToolTokens + Estimate(in.State)
	for _, m := range in.System {
		fixed += Estimate(m)
	}
	return fixed
}

// GroupTokens is the token cost of one group: its rows and their run-memory echo.
func GroupTokens(g []Entry) int {
	cost := 0
	for _, e := range g {
		cost += Estimate(e.Msg) + e.EchoTokens
	}
	return cost
}

// Select returns how many groups, taken newest first from costs (newest first), fit in available:
// selection stops at the first group that does not fit, so the result is a contiguous suffix.
func Select(costs []int, available int) int {
	used := 0
	for k, cost := range costs {
		if used+cost > available {
			return k
		}
		used += cost
	}
	return len(costs)
}

// CapacityRetry is the budget for the one retry on the same model after the provider rejected, as too
// long, the request that Build made from in and b. Estimates are approximate and the window is
// deterministic, so without a retry the same window would be rejected on every Start. The retry keeps
// at most half of the transcript tokens the rejected request held; Build reports ErrOverflow when the
// newest group alone exceeds that.
func CapacityRetry(in Input, b Budget, rejected Result) Budget {
	b.Margin = RetryMargin(b, Fixed(in), rejected.Tokens)
	return b
}

// RetryMargin is the margin that leaves at most half of the rejected request's transcript tokens.
func RetryMargin(b Budget, fixed, rejectedTokens int) int {
	return b.Context - b.Output - fixed - (rejectedTokens-fixed)/2
}

// Build assembles one request. available = Context − Output − Margin −
// Σ Estimate(System) − Estimate(State) − ToolTokens; groups are taken
// newest→oldest while the running total ≤ available and selection stops at
// the first group that does not fit (contiguous suffix, no skipping).
func Build(in Input, b Budget) (Result, error) {
	groups, err := Groups(in.Transcript)
	if err != nil {
		return Result{}, err
	}
	fixed := Fixed(in)
	available := b.Context - b.Output - b.Margin - fixed
	if available < 0 {
		return Result{}, fmt.Errorf("%w: fixed prefix %d tokens exceeds budget by %d", ErrOverflow, fixed, -available)
	}
	costs := make([]int, 0, len(groups))
	used := 0
	for k := len(groups) - 1; k >= 0; k-- {
		cost := GroupTokens(groups[k])
		costs = append(costs, cost)
		if used+cost > available {
			break
		}
		used += cost
	}
	included := Select(costs, available)
	if len(groups) > 0 && included == 0 {
		return Result{}, fmt.Errorf("%w: newest group does not fit in %d available tokens", ErrOverflow, available)
	}
	first := len(groups) - included
	res := Result{FirstSeq: -1, Dropped: first, Tokens: fixed + used, Available: available, Costs: costs}
	res.Messages = make([]protocol.Message, 0, len(in.System)+1+len(in.Transcript))
	res.Messages = append(res.Messages, in.System...)
	res.Messages = append(res.Messages, in.State)
	for k := first; k < len(groups); k++ {
		if res.FirstSeq < 0 {
			res.FirstSeq = groups[k][0].Seq
		}
		for _, e := range groups[k] {
			res.Messages = append(res.Messages, e.Msg)
		}
	}
	return res, nil
}
