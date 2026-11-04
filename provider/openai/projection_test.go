package openai

import (
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// A projected history (plan T3-S2): a new run, so no echo, with a read's tool row withheld in place and
// the run's resume row after it. The adapter sends it as a legal chat-completions conversation in every
// reasoning format and echo mode: every tool message answers a call of the nearest assistant message
// before it, every call is answered before the next non-tool message, the withheld row is sent
// withheld, and no reasoning_content is sent.
func TestProjectedHistoryIsAWellFormedConversation(t *testing.T) {
	req := protocol.ProviderRequest{ReasoningEffort: "high", Messages: []protocol.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "read the thread"},
		{Role: "assistant", ToolCalls: []protocol.ToolCall{
			{ID: "c1", Name: "read_thread", Arguments: json.RawMessage(`{"id":"x"}`)},
			{ID: "c2", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)},
		}},
		window.Withhold(protocol.Message{Role: "tool", ToolCallID: "c1", Name: "read_thread", Content: "SECRET"}),
		{Role: "tool", ToolCallID: "c2", Name: "read_file", Content: "package a"},
		{Role: "user", Content: "[runtime] A new run continues this session."},
		{Role: "assistant", Content: "noted"},
		{Role: "user", Content: "go on"},
	}}
	for _, format := range []string{"", "reasoning-object", "reasoning-effort", "deepseek"} {
		for _, echo := range []string{"", echoReasoningContent} {
			wire := requestFromProtocol("m", req, false, format, echo)
			open := map[string]bool{}
			withheld := false
			for i, m := range wire.Messages {
				if m.ReasoningContent != "" {
					t.Fatalf("format %q echo %q: message %d sends reasoning_content", format, echo, i)
				}
				if m.Role == "tool" {
					if !open[m.ToolCallID] {
						t.Fatalf("format %q echo %q: tool message %s answers no open call", format, echo, m.ToolCallID)
					}
					delete(open, m.ToolCallID)
					if m.ToolCallID == "c1" {
						withheld = m.Content == window.WithheldContent
					}
					continue
				}
				if len(open) != 0 {
					t.Fatalf("format %q echo %q: calls %v unanswered before a %s message", format, echo, open, m.Role)
				}
				for _, c := range m.ToolCalls {
					open[c.ID] = true
				}
			}
			if len(open) != 0 || !withheld {
				t.Fatalf("format %q echo %q: unanswered %v, withheld row sent withheld %v", format, echo, open, withheld)
			}
		}
	}
}
