package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// Effort maps to the thinking parameter: none and empty omit it, low/medium/high/max map to
// budgets clamped to max_tokens minus 1024, a max_tokens below 2048 leaves no legal budget, and
// while thinking is on temperature is dropped and a forced tool_choice is downgraded to auto.
func TestThinkingMapping(t *testing.T) {
	base := func(effort string, temp *float64, choice string) protocol.ProviderRequest {
		return protocol.ProviderRequest{
			Messages:        []protocol.Message{{Role: "user", Content: "x"}},
			ReasoningEffort: effort,
			Temperature:     temp,
			ToolChoice:      choice,
			MaxTokens:       8192,
		}
	}
	cases := []struct {
		effort      string
		maxTokens   int
		wantPresent bool
		wantBudget  int
	}{
		{"", 8192, false, 0},
		{"none", 8192, false, 0},
		{"low", 8192, true, 1024},
		{"medium", 8192, true, 4096},
		{"high", 8192, true, 8192 - 1024},
		{"max", 65536, true, 16384},
		{"low", 2048, true, 1024},
		{"low", 2047, false, 0},
		{"high", 1024, false, 0},
	}
	for _, c := range cases {
		req := base(c.effort, nil, "")
		req.MaxTokens = c.maxTokens
		body, err := wireBody(t, req, 4096)
		if err != nil {
			t.Fatalf("effort %s: %v", c.effort, err)
		}
		thinking, has := body["thinking"]
		if has != c.wantPresent {
			t.Fatalf("effort %s max %d: thinking=%v", c.effort, c.maxTokens, thinking)
		}
		if !c.wantPresent {
			continue
		}
		configured := thinking.(map[string]any)
		if configured["type"] != "enabled" || configured["budget_tokens"] != float64(c.wantBudget) {
			t.Fatalf("effort %s max %d: thinking=%v", c.effort, c.maxTokens, configured)
		}
	}
	// While thinking is on the temperature is dropped and a forced choice becomes auto.
	temp := 0.2
	body := mustBody(t, base("high", &temp, "required"), 4096)
	if _, has := body["temperature"]; has {
		t.Fatalf("temperature sent while thinking: %v", body["temperature"])
	}
	if body["tool_choice"].(map[string]any)["type"] != "auto" {
		t.Fatalf("forced tool_choice while thinking: %v", body["tool_choice"])
	}
	// Without thinking both pass through.
	body = mustBody(t, base("none", &temp, "required"), 4096)
	if body["temperature"] != 0.2 {
		t.Fatalf("temperature=%v", body["temperature"])
	}
	if body["tool_choice"].(map[string]any)["type"] != "any" {
		t.Fatalf("tool_choice=%v", body["tool_choice"])
	}
}

// The thinking echo of the current turn's assistant row is put back byte for byte, before the
// message's own blocks.
func TestThinkingEchoIsSentBackVerbatim(t *testing.T) {
	blocks := []byte(`[{"type":"thinking","thinking":"hidden","signature":"sig-1"},{"type":"redacted_thinking","data":"opaque"}]`)
	body := mustBody(t, protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "user", Content: "x"},
			{Role: "assistant", Content: "answer", ToolCalls: []protocol.ToolCall{
				{ID: "t1", Name: "read_file", Arguments: json.RawMessage(`{}`)},
			}},
			{Role: "tool", ToolCallID: "t1", Name: "read_file", Content: "result"},
		},
		Echo: []protocol.Echo{protocol.Echo{}, protocol.NewEcho(echoThinking, blocks, 10)},
	}, 8192)
	assistant := body["messages"].([]any)[1].(map[string]any)
	content := assistant["content"].([]any)
	if len(content) != 4 {
		t.Fatalf("assistant blocks=%v", content)
	}
	first := content[0].(map[string]any)
	if first["type"] != "thinking" || first["thinking"] != "hidden" || first["signature"] != "sig-1" {
		t.Fatalf("thinking block=%v", first)
	}
	if content[1].(map[string]any)["type"] != "redacted_thinking" || content[1].(map[string]any)["data"] != "opaque" {
		t.Fatalf("redacted block=%v", content[1])
	}
	if content[2].(map[string]any)["type"] != "text" || content[2].(map[string]any)["text"] != "answer" {
		t.Fatalf("text block=%v", content[2])
	}
	use := content[3].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "t1" {
		t.Fatalf("tool_use block=%v", use)
	}
}

// A non-streamed response: text concatenates, tool_use becomes tool calls, thinking blocks become
// the Echo untouched, the stop reason maps and usage maps (cache fields dropped, total computed).
func TestResponseToProtocol(t *testing.T) {
	raw := `{
		"content": [
			{"type":"thinking","thinking":"ponder","signature":"s"},
			{"type":"text","text":"one "},
			{"type":"text","text":"two"},
			{"type":"tool_use","id":"t1","name":"read_file","input":{"path":"README.md"}},
			{"type":"tool_use","id":"t2","name":"shell","input":{}},
			{"type":"redacted_thinking","data":"blob"}
		],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":11,"output_tokens":7,"cache_creation_input_tokens":5,"cache_read_input_tokens":3}
	}`
	var resp messagesResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	out, err := responseToProtocol(resp)
	if err != nil {
		t.Fatal(err)
	}
	if out.Message.Content != "one two" {
		t.Fatalf("content=%q", out.Message.Content)
	}
	if len(out.Message.ToolCalls) != 2 {
		t.Fatalf("tool calls=%v", out.Message.ToolCalls)
	}
	call := out.Message.ToolCalls[0]
	if call.ID != "t1" || call.Name != "read_file" || string(call.Arguments) != `{"path":"README.md"}` {
		t.Fatalf("call=%v", call)
	}
	if string(out.Message.ToolCalls[1].Arguments) != "{}" {
		t.Fatalf("empty input=%s", out.Message.ToolCalls[1].Arguments)
	}
	if out.FinishReason != "tool_calls" {
		t.Fatalf("finish=%q", out.FinishReason)
	}
	if out.Usage.PromptTokens != 11 || out.Usage.CompletionTokens != 7 || out.Usage.TotalTokens != 18 {
		t.Fatalf("usage=%+v", out.Usage)
	}
	if out.Echo.IsZero() || out.Echo.Kind() != echoThinking {
		t.Fatalf("echo=%v", out.Echo)
	}
	// The echo data is the thinking blocks as they arrived.
	var blocks []map[string]any
	if err := json.Unmarshal(out.Echo.Data(), &blocks); err != nil || len(blocks) != 2 {
		t.Fatalf("echo data=%s", out.Echo.Data())
	}
	if blocks[0]["thinking"] != "ponder" || blocks[0]["signature"] != "s" || blocks[1]["data"] != "blob" {
		t.Fatalf("echo blocks=%v", blocks)
	}
}

func TestStopReasonMapping(t *testing.T) {
	cases := map[string]string{
		"end_turn":      "stop",
		"stop_sequence": "stop",
		"refusal":       "stop",
		"tool_use":      "tool_calls",
		"max_tokens":    "length",
		"":              "",
		"model_turn":    "model_turn",
	}
	for in, want := range cases {
		if got := mapStopReason(in); got != want {
			t.Fatalf("mapStopReason(%q)=%q want %q", in, got, want)
		}
	}
}

// A response without a tool_use name is refused, like the openai adapter refuses a call without a
// function name.
func TestResponseRefusesToolUseWithoutName(t *testing.T) {
	raw := `{"content":[{"type":"tool_use","id":"t1","input":{}}],"stop_reason":"tool_use","usage":{}}`
	var resp messagesResponse
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatal(err)
	}
	if _, err := responseToProtocol(resp); err == nil {
		t.Fatal("tool_use without a name accepted")
	}
}
