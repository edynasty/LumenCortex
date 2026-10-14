// Package window holds the token estimate the model-selection policy uses to decide whether a
// request fits a candidate's context. The full window builder (grouping, budgets, dropping) arrives
// with M3.2; until then this is the only thing the package offers (design D6).
package window

import (
	"encoding/json"

	"github.com/edynasty/LumenCortex/protocol"
)

// Estimate is a deterministic token estimate of v's JSON encoding: (len(json)+2)/3 + 4. It
// undercounts for some tokenizers and content; the policy keeps a 5% margin for that.
//
// json.Marshal of a protocol.Message only fails when a ToolCall.Arguments value is not valid JSON.
// Messages and message slices are then estimated with each invalid Arguments replaced by its
// JSON-quoted text, so the estimate stays deterministic. Any other value that cannot be encoded
// estimates as 0.
func Estimate(v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		switch m := v.(type) {
		case protocol.Message:
			b, err = json.Marshal(sanitize(m))
		case []protocol.Message:
			out := make([]protocol.Message, len(m))
			for i := range m {
				out[i] = sanitize(m[i])
			}
			b, err = json.Marshal(out)
		}
		if err != nil {
			return 0
		}
	}
	return (len(b)+2)/3 + 4
}

func sanitize(m protocol.Message) protocol.Message {
	if len(m.ToolCalls) == 0 {
		return m
	}
	calls := make([]protocol.ToolCall, len(m.ToolCalls))
	for i, call := range m.ToolCalls {
		calls[i] = call
		if len(call.Arguments) > 0 && !json.Valid(call.Arguments) {
			calls[i].Arguments, _ = json.Marshal(string(call.Arguments))
		}
	}
	m.ToolCalls = calls
	return m
}
