package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

func decodeRequest(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode request %s: %v", raw, err)
	}
	return out
}

// wireBody marshals the request the adapter would send, as a decoded object for assertions, for a
// model that declares no reasoning.format (the budget default).
func wireBody(t *testing.T, req protocol.ProviderRequest, defaultMaxTokens int) (map[string]any, error) {
	t.Helper()
	return wireBodyFormat(t, req, defaultMaxTokens, "")
}

// wireBodyFormat is wireBody with an explicit reasoning.format.
func wireBodyFormat(t *testing.T, req protocol.ProviderRequest, defaultMaxTokens int, format string) (map[string]any, error) {
	t.Helper()
	wire, err := requestFromProtocol("claude-test", req, false, defaultMaxTokens, format)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	return decodeRequest(t, raw), nil
}

func mustBody(t *testing.T, req protocol.ProviderRequest, defaultMaxTokens int) map[string]any {
	t.Helper()
	body, err := wireBody(t, req, defaultMaxTokens)
	if err != nil {
		t.Fatalf("requestFromProtocol: %v", err)
	}
	return body
}

// A plain conversation: the system messages become the top-level system, the user and assistant
// messages alternate, and max_tokens is always present.
func TestRequestShapeForTextConversation(t *testing.T) {
	body := mustBody(t, protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: "you are careful"},
			{Role: "user", Content: "fix it"},
			{Role: "assistant", Content: "looking"},
			{Role: "user", Content: "thanks"},
		},
		MaxTokens: 512,
	}, 4096)
	if body["model"] != "claude-test" || body["max_tokens"] != float64(512) {
		t.Fatalf("model/max_tokens: %v / %v", body["model"], body["max_tokens"])
	}
	if body["system"] != "you are careful" {
		// One system message is the documented string form; several become a block array.
		t.Fatalf("system=%v", body["system"])
	}
	messages := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages=%v", messages)
	}
	first := messages[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("first message role=%v", first["role"])
	}
	if _, has := body["tools"]; has {
		t.Fatal("tools sent with none in the request")
	}
	if _, has := body["thinking"]; has {
		t.Fatal("thinking sent with no effort")
	}
	if _, has := body["tool_choice"]; has {
		t.Fatal("tool_choice sent with none in the request")
	}
}

func TestRequestSystemBlocksForSeveralSystemMessages(t *testing.T) {
	body := mustBody(t, protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: "one"},
			{Role: "system", Content: "two"},
			{Role: "user", Content: "hi"},
		},
	}, 4096)
	blocks, ok := body["system"].([]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("system=%v", body["system"])
	}
	if blocks[0].(map[string]any)["text"] != "one" || blocks[1].(map[string]any)["text"] != "two" {
		t.Fatalf("system blocks=%v", blocks)
	}
}

// max_tokens is required by the API: the request's value wins, else the model's resolved output
// limit the factory passed, else the documented default.
func TestMaxTokensAlwaysPresent(t *testing.T) {
	req := protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}
	if got := mustBody(t, req, 0)["max_tokens"]; got != float64(4096) {
		t.Fatalf("documented default: %v", got)
	}
	if got := mustBody(t, req, 777)["max_tokens"]; got != float64(777) {
		t.Fatalf("configured default: %v", got)
	}
	if got := mustBody(t, protocol.ProviderRequest{Messages: req.Messages, MaxTokens: 99}, 777)["max_tokens"]; got != float64(99) {
		t.Fatalf("request value: %v", got)
	}
	// The request's explicit 0 does not override the default.
	if got := mustBody(t, protocol.ProviderRequest{Messages: req.Messages}, 512)["max_tokens"]; got != float64(512) {
		t.Fatalf("zero request max_tokens: %v", got)
	}
}

// Tools map to {name, description, input_schema}; the runtime's tool choice "auto" becomes the
// auto object.
func TestRequestToolsAndChoice(t *testing.T) {
	body := mustBody(t, protocol.ProviderRequest{
		Messages:   []protocol.Message{{Role: "user", Content: "list files"}},
		Tools:      []protocol.ToolSpec{{Name: "shell", Description: "run a command", Parameters: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}}}},
		ToolChoice: "auto",
	}, 4096)
	tools := body["tools"].([]any)
	tool := tools[0].(map[string]any)
	if tool["name"] != "shell" || tool["description"] != "run a command" {
		t.Fatalf("tool=%v", tool)
	}
	schema := tool["input_schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("input_schema=%v", schema)
	}
	if body["tool_choice"].(map[string]any)["type"] != "auto" {
		t.Fatalf("tool_choice=%v", body["tool_choice"])
	}
	// A tool without a schema still sends an (empty, permissive) object schema.
	body = mustBody(t, protocol.ProviderRequest{
		Messages: []protocol.Message{{Role: "user", Content: "x"}},
		Tools:    []protocol.ToolSpec{{Name: "noop"}},
	}, 4096)
	if empty := body["tools"].([]any)[0].(map[string]any)["input_schema"].(map[string]any); len(empty) != 0 {
		t.Fatalf("empty schema: %v", body["tools"])
	}
}

// An assistant turn with tool calls becomes text and tool_use blocks; the arguments are carried as
// the input object. Consecutive tool rows merge into one user message of tool_result blocks, and a
// user row after them (a resume row) joins the same user message.
func TestRequestToolUseAndToolResultMerging(t *testing.T) {
	body := mustBody(t, protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "check the readme"},
			{Role: "assistant", Content: "reading", ToolCalls: []protocol.ToolCall{
				{ID: "t1", Name: "read_file", Arguments: json.RawMessage(`{"path":"README.md"}`)},
				{ID: "t2", Name: "shell", Arguments: json.RawMessage(`{"command":"ls"}`)},
			}},
			{Role: "tool", ToolCallID: "t1", Name: "read_file", Content: "hello"},
			{Role: "tool", ToolCallID: "t2", Name: "shell", Content: "file list"},
			{Role: "user", Content: "[runtime] resume row"},
			{Role: "assistant", Content: "done"},
		},
	}, 4096)
	messages := body["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages=%v", messages)
	}
	assistant := messages[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("second message role=%v", assistant["role"])
	}
	blocks := assistant["content"].([]any)
	if len(blocks) != 3 {
		t.Fatalf("assistant blocks=%v", blocks)
	}
	if blocks[0].(map[string]any)["type"] != "text" || blocks[0].(map[string]any)["text"] != "reading" {
		t.Fatalf("text block=%v", blocks[0])
	}
	use := blocks[1].(map[string]any)
	if use["type"] != "tool_use" || use["id"] != "t1" || use["name"] != "read_file" {
		t.Fatalf("tool_use block=%v", use)
	}
	input := use["input"].(map[string]any)
	if input["path"] != "README.md" {
		t.Fatalf("input=%v", input)
	}
	// Both tool results and the resume row are one user message, in order.
	results := messages[2].(map[string]any)
	if results["role"] != "user" {
		t.Fatalf("third message role=%v", results["role"])
	}
	resultBlocks := results["content"].([]any)
	if len(resultBlocks) != 3 {
		t.Fatalf("result blocks=%v", resultBlocks)
	}
	first := resultBlocks[0].(map[string]any)
	if first["type"] != "tool_result" || first["tool_use_id"] != "t1" || first["content"] != "hello" {
		t.Fatalf("tool_result block=%v", first)
	}
	if resultBlocks[2].(map[string]any)["type"] != "text" {
		t.Fatalf("merged resume row=%v", resultBlocks[2])
	}
}

// Stored arguments that are not a JSON object (the model's raw text as a JSON string) cannot be a
// tool_use input; they become {} and the tool_result that follows reports the rejected call.
func TestNonObjectArgumentsBecomeEmptyInput(t *testing.T) {
	body := mustBody(t, protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "user", Content: "go"},
			{Role: "assistant", ToolCalls: []protocol.ToolCall{
				{ID: "bad", Name: "write_file", Arguments: json.RawMessage(`"{\"path\":\"x\"}"`)},
			}},
			{Role: "tool", ToolCallID: "bad", Name: "write_file", Content: "invalid tool arguments"},
		},
	}, 4096)
	assistant := body["messages"].([]any)[1].(map[string]any)
	use := assistant["content"].([]any)[0].(map[string]any)
	input := use["input"].(map[string]any)
	if len(input) != 0 {
		t.Fatalf("input=%v", input)
	}
}

func TestRequestRefusesImpossibleConversations(t *testing.T) {
	cases := map[string]protocol.ProviderRequest{
		"no messages":         {},
		"only system":         {Messages: []protocol.Message{{Role: "system", Content: "s"}}},
		"assistant first":     {Messages: []protocol.Message{{Role: "assistant", Content: "hi"}}},
		"unknown role":        {Messages: []protocol.Message{{Role: "user", Content: "x"}, {Role: "daemon", Content: "y"}}},
		"unknown tool choice": {Messages: []protocol.Message{{Role: "user", Content: "x"}}, ToolChoice: "sometimes"},
		"bad echo": {Messages: []protocol.Message{
			{Role: "user", Content: "x"},
			{Role: "assistant", Content: "y"},
		}, Echo: []protocol.Echo{protocol.Echo{}, protocol.NewEcho(echoThinking, []byte(`{"not":"an array"}`), 1)}},
	}
	for name, req := range cases {
		if _, err := requestFromProtocol("m", req, false, 4096, ""); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// The echo that is not a block array names what it refused.
	_, err := requestFromProtocol("m", cases["bad echo"], false, 4096, "")
	if err == nil || !strings.Contains(err.Error(), "thinking echo") {
		t.Fatalf("bad echo error: %v", err)
	}
}

// budgetRequest is the request the golden below pins: budget-format thinking with everything else
// the adapter can send (system, thinking echo, tool_use, tool_result, tools, a forced tool choice,
// a temperature and a max_tokens that clamps the budget).
func budgetRequest() protocol.ProviderRequest {
	temperature := 0.2
	return protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: "you are careful"},
			{Role: "user", Content: "fix the test"},
			{Role: "assistant", Content: "running it", ToolCalls: []protocol.ToolCall{
				{ID: "call-1", Name: "shell", Arguments: json.RawMessage(`{"command":"go test ./..."}`)},
			}},
			{Role: "tool", ToolCallID: "call-1", Content: "FAIL"},
			{Role: "user", Content: "try again"},
		},
		Tools: []protocol.ToolSpec{{Name: "shell", Description: "run a command", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}},
		}}},
		ToolChoice:      "required",
		Temperature:     &temperature,
		MaxTokens:       4096,
		ReasoningEffort: "high",
		Echo: []protocol.Echo{{}, {}, protocol.NewEcho(echoThinking,
			[]byte(`[{"type":"thinking","thinking":"hm","signature":"sig"}]`), 4)},
	}
}

// budgetGolden is the exact body budgetRequest produces. It was captured from the adapter before
// adaptive thinking was added: the default reasoning format is the budget encoding, and a model
// that declares no format must keep sending the same bytes.
const budgetGolden = `{"model":"claude-test","max_tokens":4096,"system":"you are careful","messages":[{"role":"user","content":[{"type":"text","text":"fix the test"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"hm","signature":"sig"},{"type":"text","text":"running it"},{"type":"tool_use","id":"call-1","name":"shell","input":{"command":"go test ./..."}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"FAIL"},{"type":"text","text":"try again"}]}],"tools":[{"name":"shell","description":"run a command","input_schema":{"properties":{"command":{"type":"string"}},"type":"object"}}],"tool_choice":{"type":"auto"},"thinking":{"type":"enabled","budget_tokens":3072}}`

// The budget format is byte-for-byte what earlier builds sent, streamed and not: the adaptive
// support must not reorder, rename or drop a field of an existing request.
func TestBudgetFormatRequestBytesUnchanged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		wire, err := requestFromProtocol("claude-test", budgetRequest(), stream, 8192, "")
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		want := budgetGolden
		if stream {
			want = strings.Replace(want, `"thinking":{"type":"enabled","budget_tokens":3072}`,
				`"thinking":{"type":"enabled","budget_tokens":3072},"stream":true`, 1)
		}
		if string(raw) != want {
			t.Fatalf("stream=%v: budget request changed:\n got %s\nwant %s", stream, raw, want)
		}
	}
}

// Adaptive thinking is the encoding of an adaptive model: a level the run asks for becomes
// output_config.effort with thinking type adaptive, and no level sends neither parameter. These
// models refuse a non-default temperature, so it is dropped whether or not a level is sent, and a
// forced tool choice is downgraded. Streamed and non-streamed requests carry the same parameters.
func TestAdaptiveReasoningFormat(t *testing.T) {
	temperature := 0.2
	for _, stream := range []bool{false, true} {
		for effort, wantEffort := range map[string]string{
			"":       "",
			"none":   "",
			"low":    "low",
			"medium": "medium",
			"high":   "high",
			"max":    "max",
		} {
			wire, err := requestFromProtocol("claude-test", protocol.ProviderRequest{
				Messages:        []protocol.Message{{Role: "user", Content: "hi"}},
				Tools:           []protocol.ToolSpec{{Name: "shell"}},
				ToolChoice:      "required",
				Temperature:     &temperature,
				ReasoningEffort: effort,
			}, stream, 4096, config.ReasoningAdaptive)
			if err != nil {
				t.Fatalf("stream=%v effort=%q: %v", stream, effort, err)
			}
			raw, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			body := decodeRequest(t, raw)
			name := fmt.Sprintf("stream=%v effort=%q", stream, effort)
			if _, sent := body["temperature"]; sent {
				t.Fatalf("%s: temperature sent: %s", name, raw)
			}
			if got := body["stream"] == true; got != stream {
				t.Fatalf("%s: stream=%v", name, got)
			}
			if choice := body["tool_choice"].(map[string]any); choice["type"] != "auto" {
				t.Fatalf("%s: tool_choice=%v, want the downgraded auto", name, choice)
			}
			thinking, hasThinking := body["thinking"].(map[string]any)
			output, hasOutput := body["output_config"].(map[string]any)
			if wantEffort == "" {
				if hasThinking || hasOutput {
					t.Fatalf("%s: thinking/output_config sent for no level: %s", name, raw)
				}
				continue
			}
			if !hasThinking || thinking["type"] != "adaptive" {
				t.Fatalf("%s: thinking=%v, want type adaptive", name, body["thinking"])
			}
			if _, budget := thinking["budget_tokens"]; budget {
				t.Fatalf("%s: adaptive thinking sent a budget: %s", name, raw)
			}
			if !hasOutput || output["effort"] != wantEffort {
				t.Fatalf("%s: output_config=%v, want effort %q", name, body["output_config"], wantEffort)
			}
		}
	}
	// The same request under the budget format still sends a budget and no output_config: the two
	// encodings differ on the wire, not only in the resolved format.
	body := mustBody(t, protocol.ProviderRequest{
		Messages:        []protocol.Message{{Role: "user", Content: "hi"}},
		ReasoningEffort: "high",
	}, 4096)
	thinking := body["thinking"].(map[string]any)
	// 8192 clamped to max_tokens - 1024.
	if thinking["type"] != "enabled" || thinking["budget_tokens"] != float64(3072) {
		t.Fatalf("budget thinking=%v", thinking)
	}
	if _, sent := body["output_config"]; sent {
		t.Fatalf("budget request sent output_config: %v", body["output_config"])
	}
}

// A reasoning.format this adapter does not implement is refused before a request is built: silently
// ignoring it is what made a model that requires adaptive thinking answer HTTP 400.
func TestUnimplementedReasoningFormatIsRefused(t *testing.T) {
	req := protocol.ProviderRequest{Messages: []protocol.Message{{Role: "user", Content: "hi"}}}
	for _, format := range []string{"deepseek", "reasoning-object", "reasoning-effort", "none"} {
		_, err := requestFromProtocol("m", req, false, 4096, format)
		if err == nil {
			t.Fatalf("format %q accepted", format)
		}
		if !strings.Contains(err.Error(), "reasoning format") || !strings.Contains(err.Error(), "adaptive") {
			t.Fatalf("format %q: error %v does not name the accepted values", format, err)
		}
		if class, _ := classify(context.Background(), err); class != protocol.ClassBadRequest {
			t.Fatalf("format %q: class %v, want bad_request", format, class)
		}
	}
	// Case and surrounding space do not matter: a hand-built Config may spell the format either way.
	if _, err := requestFromProtocol("m", req, false, 4096, " Adaptive "); err != nil {
		t.Fatalf("adaptive with space/case: %v", err)
	}
}
