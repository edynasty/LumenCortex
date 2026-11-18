// Package window holds the deterministic token estimator and contiguous suffix request-window builder.
package window

import (
	"encoding/json"

	"github.com/edynasty/LumenCortex/protocol"
)

// EstimateBytes returns a deterministic conservative token estimate for raw UTF-8/JSON bytes.
// Empty input estimates as 0; non-empty input classifies bytes in a single pass into ASCII letters
// and space (weight 4/12), other single-byte ASCII < 0x80 (weight 7/12), and multi-byte UTF-8 >= 0x80
// (weight 6/12): floor((4*A + 7*S + 6*U + 11)/12) + 4.
func EstimateBytes(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	var a, s, u int
	for _, c := range b {
		switch {
		case (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == ' ':
			a++
		case c < 0x80:
			s++
		default:
			u++
		}
	}
	return (4*a+7*s+6*u+11)/12 + 4
}

// ReserveTokensForBytes returns the worst-case EstimateBytes upper bound for any byte slice of
// length in (0, n]: floor((7*n + 11)/12) + 4, or 0 when n <= 0.
func ReserveTokensForBytes(n int) int {
	if n <= 0 {
		return 0
	}
	return (7*n+11)/12 + 4
}

// Estimate is a deterministic token estimate of v's JSON encoding via EstimateBytes.
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
	return EstimateBytes(b)
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
