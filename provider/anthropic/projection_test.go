package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/protocol"
)

// projectedHistory is a projected history (plan T3-S2): a new run, so no echo, with a read's tool row
// withheld in place and the run's resume row after it.
func projectedHistory() protocol.ProviderRequest {
	return protocol.ProviderRequest{ReasoningEffort: "high", Messages: []protocol.Message{
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
}

type block struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

// The adapter sends a projected history as a legal Messages API conversation in every reasoning
// format: roles alternate and start with user, every tool_use is answered in the next user message and
// every tool_result answers a tool_use of the message before it, the withheld row is sent withheld,
// and no thinking block is sent.
func TestProjectedHistoryIsAWellFormedConversation(t *testing.T) {
	for _, format := range []string{"", config.ReasoningAdaptive} {
		wire, err := requestFromProtocol("claude-test", projectedHistory(), false, 4096, format)
		if err != nil {
			t.Fatalf("format %q: %v", format, err)
		}
		open := map[string]bool{}
		withheld := false
		for i, m := range wire.Messages {
			if want := []string{"user", "assistant"}[i%2]; m.Role != want {
				t.Fatalf("format %q: message %d is %s, want %s", format, i, m.Role, want)
			}
			uses := map[string]bool{}
			for _, raw := range m.Content {
				var b block
				if err := json.Unmarshal(raw, &b); err != nil {
					t.Fatal(err)
				}
				switch b.Type {
				case "thinking", "redacted_thinking":
					t.Fatalf("format %q: a projected history sends a %s block", format, b.Type)
				case "tool_use":
					if m.Role != "assistant" {
						t.Fatalf("format %q: tool_use in a %s message", format, m.Role)
					}
					uses[b.ID] = true
				case "tool_result":
					if !open[b.ToolUseID] {
						t.Fatalf("format %q: tool_result %s answers no tool_use of the previous message", format, b.ToolUseID)
					}
					delete(open, b.ToolUseID)
					if b.ToolUseID == "c1" {
						withheld = b.Content == window.WithheldContent
					}
				}
			}
			if len(open) != 0 {
				t.Fatalf("format %q: tool_use %v not answered in the next message", format, open)
			}
			open = uses
		}
		if len(open) != 0 || !withheld {
			t.Fatalf("format %q: unanswered %v, withheld row sent withheld %v", format, open, withheld)
		}
	}
}
