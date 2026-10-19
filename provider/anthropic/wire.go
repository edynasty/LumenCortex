// Package anthropic is the Anthropic Messages wire adapter: it converts protocol requests to
// Messages API requests (top-level system, alternating user/assistant messages, tool_use and
// tool_result content blocks), reads streamed and non-streamed responses, and classifies failures
// into protocol error classes.
//
// Extended thinking is run memory (design §4.6): thinking and redacted_thinking blocks the model
// returned travel back as an opaque protocol.Echo and are put on the wire again, byte for byte,
// only for the assistant rows of the current turn the same model produced. They are never logged,
// summarized or persisted; see docs/go-provider.md.
//
// Mapping notes, kept honest to what the protocol carries:
//   - max_tokens is required by the API, so the adapter always sends one: the request's MaxTokens,
//     else the model's resolved output limit (config.DefaultOutput when unconfigured).
//   - reasoning effort maps to the thinking parameter: none or empty omits it; low, medium, high
//     and max map to budgets of 1024, 4096, 8192 and 16384 tokens, clamped to max_tokens minus
//     1024. A max_tokens below 2048 leaves no legal budget, so thinking is omitted. While thinking
//     is on the adapter omits temperature (the API allows only 1) and downgrades tool_choice to
//     auto (the API allows only auto or none).
//   - a protocol tool row has no success flag, so tool_result blocks carry the result text only:
//     is_error is omitted rather than guessed, and the text (which states failures) says it anyway.
//   - prompt caching usage (cache_creation_input_tokens, cache_read_input_tokens) has no protocol
//     Usage field; it is dropped, not remapped onto unrelated fields.
package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// echoThinking is the Echo kind that carries thinking blocks; its data is the JSON array of
// thinking/redacted_thinking blocks exactly as the API returned them.
const echoThinking = "anthropic-thinking"

// defaultAnthropicVersion is sent as anthropic-version unless the provider's configured headers
// override it.
const defaultAnthropicVersion = "2023-06-01"

// thinkingBudgets maps the provider-neutral effort levels to thinking budget tokens.
var thinkingBudgets = map[string]int{
	"low":    1024,
	"medium": 4096,
	"high":   8192,
	"max":    16384,
}

// minThinkingBudget is the smallest budget the API accepts.
const minThinkingBudget = 1024

type messagesRequest struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	System      json.RawMessage `json:"system,omitempty"`
	Messages    []wireMessage   `json:"messages"`
	Tools       []wireTool      `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	Thinking    *thinkingConfig `json:"thinking,omitempty"`
	Stream      bool            `json:"stream,omitempty"`
}

type wireMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type wireTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type thinkingConfig struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content,omitempty"`
}

type systemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// requestFromProtocol builds the Messages API request. defaultMaxTokens is the max_tokens sent
// when the request carries none.
func requestFromProtocol(model string, req protocol.ProviderRequest, stream bool, defaultMaxTokens int) (messagesRequest, error) {
	if len(req.Messages) == 0 {
		return messagesRequest{}, &badRequestError{errors.New("request has no messages")}
	}
	var system []string
	type merged struct {
		role   string
		blocks []json.RawMessage
	}
	var conversation []merged
	add := func(role string, blocks []json.RawMessage) {
		if len(blocks) == 0 {
			return
		}
		if n := len(conversation); n > 0 && conversation[n-1].role == role {
			conversation[n-1].blocks = append(conversation[n-1].blocks, blocks...)
			return
		}
		conversation = append(conversation, merged{role: role, blocks: blocks})
	}
	for i, message := range req.Messages {
		switch message.Role {
		case "system":
			system = append(system, message.Content)
		case "user":
			if message.Content != "" {
				add("user", []json.RawMessage{mustJSON(textBlock{Type: "text", Text: message.Content})})
			}
		case "tool":
			// One tool row is one tool_result block; consecutive rows (and a user row that follows
			// them, e.g. a resume row) merge into the same user message, in order.
			add("user", []json.RawMessage{mustJSON(toolResultBlock{Type: "tool_result", ToolUseID: message.ToolCallID, Content: message.Content})})
		case "assistant":
			blocks, err := assistantBlocks(message, echoAt(req.Echo, i))
			if err != nil {
				return messagesRequest{}, err
			}
			add("assistant", blocks)
		default:
			return messagesRequest{}, &badRequestError{fmt.Errorf("message %d has role %q, want system, user, assistant or tool", i, message.Role)}
		}
	}
	if len(conversation) == 0 {
		return messagesRequest{}, &badRequestError{errors.New("request has only system messages")}
	}
	if conversation[0].role != "user" {
		return messagesRequest{}, &badRequestError{fmt.Errorf("the first message is a %s message: the API requires the conversation to start with a user message", conversation[0].role)}
	}
	messages := make([]wireMessage, len(conversation))
	for i, m := range conversation {
		messages[i] = wireMessage{Role: m.role, Content: m.blocks}
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	if maxTokens <= 0 {
		// The documented default: a hand-built Config with no MaxTokensDefault still sends a
		// legal request.
		maxTokens = config.DefaultOutput
	}
	out := messagesRequest{
		Model:     model,
		MaxTokens: maxTokens,
		Messages:  messages,
	}
	if len(system) == 1 {
		out.System = mustJSON(system[0])
	} else if len(system) > 1 {
		blocks := make([]systemBlock, len(system))
		for i, text := range system {
			blocks[i] = systemBlock{Type: "text", Text: text}
		}
		out.System = mustJSON(blocks)
	}
	for _, spec := range req.Tools {
		schema := spec.Parameters
		if schema == nil {
			schema = map[string]any{}
		}
		out.Tools = append(out.Tools, wireTool{Name: spec.Name, Description: spec.Description, InputSchema: schema})
	}
	toolChoice, choiceName, err := wireToolChoice(req.ToolChoice)
	if err != nil {
		return messagesRequest{}, err
	}
	temperature := req.Temperature
	if budget, ok := thinkingBudget(req.ReasoningEffort, maxTokens); ok {
		out.Thinking = &thinkingConfig{Type: "enabled", BudgetTokens: budget}
		// The API allows only temperature 1 while thinking; the request's temperature is dropped
		// instead of silently sending a value that would be refused.
		temperature = nil
		// The API allows only auto or none while thinking; a forced choice is downgraded to auto.
		if choiceName == "any" {
			toolChoice = mustJSON(struct {
				Type string `json:"type"`
			}{Type: "auto"})
		}
	}
	out.ToolChoice = toolChoice
	out.Temperature = temperature
	out.Stream = stream
	return out, nil
}

// assistantBlocks converts one assistant message: the turn's thinking echo first, byte for byte,
// then the text, then one tool_use block per call.
func assistantBlocks(message protocol.Message, echo protocol.Echo) ([]json.RawMessage, error) {
	var blocks []json.RawMessage
	if echo.Kind() == echoThinking {
		var echoed []json.RawMessage
		if err := json.Unmarshal(echo.Data(), &echoed); err != nil || len(echoed) == 0 {
			return nil, &badRequestError{errors.New("the assistant message's thinking echo is not a block array")}
		}
		blocks = append(blocks, echoed...)
	}
	if message.Content != "" {
		blocks = append(blocks, mustJSON(textBlock{Type: "text", Text: message.Content}))
	}
	for _, call := range message.ToolCalls {
		blocks = append(blocks, mustJSON(toolUseBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: toolInput(call.Arguments)}))
	}
	return blocks, nil
}

// toolInput is the tool_use input object. Stored arguments that are not a JSON object (a JSON
// string holding the model's raw text, a number, null) become {}: the API requires an object, the
// following tool_result reports the rejected call, and nothing is guessed.
func toolInput(raw json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return json.RawMessage("{}")
	}
	var object map[string]any
	if err := json.Unmarshal(trimmed, &object); err != nil {
		return json.RawMessage("{}")
	}
	return trimmed
}

// wireToolChoice maps the protocol tool choice. "" and "none" send no tool_choice (the API has no
// none type; the runtime only sends auto).
func wireToolChoice(choice string) (json.RawMessage, string, error) {
	switch strings.ToLower(strings.TrimSpace(choice)) {
	case "", "none":
		return nil, "", nil
	case "auto":
		return mustJSON(struct {
			Type string `json:"type"`
		}{Type: "auto"}), "auto", nil
	case "required", "any":
		return mustJSON(struct {
			Type string `json:"type"`
		}{Type: "any"}), "any", nil
	default:
		return nil, "", &badRequestError{fmt.Errorf("tool choice %q is not one of auto, required, none", choice)}
	}
}

// thinkingBudget maps a reasoning effort to a thinking budget within maxTokens. ok is false when
// the effort asks for no thinking or the max_tokens leaves no legal budget.
func thinkingBudget(effort string, maxTokens int) (int, bool) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	switch effort {
	case "", "none":
		return 0, false
	case "low", "medium", "high", "max":
		budget := thinkingBudgets[effort]
		if budget > maxTokens-minThinkingBudget {
			budget = maxTokens - minThinkingBudget
		}
		if budget < minThinkingBudget {
			return 0, false
		}
		return budget, true
	default:
		// The runtime validates efforts; an unknown one reaching the adapter changes nothing.
		return 0, false
	}
}

func echoAt(echo []protocol.Echo, i int) protocol.Echo {
	if i < len(echo) {
		return echo[i]
	}
	return protocol.Echo{}
}

func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic("anthropic wire: marshal: " + err.Error())
	}
	return raw
}

// messagesResponse is one non-streamed response. Content stays raw so thinking blocks can be
// echoed back byte for byte.
type messagesResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      responseUsage     `json:"usage"`
}

type responseUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

func (u responseUsage) toProtocol() protocol.Usage {
	return protocol.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.OutputTokens,
	}
}

// responseToProtocol converts one response: text blocks concatenate, tool_use blocks become tool
// calls, thinking blocks become the response's Echo.
func responseToProtocol(resp messagesResponse) (protocol.ProviderResponse, error) {
	message := protocol.Message{Role: "assistant"}
	var thinking []json.RawMessage
	for _, raw := range resp.Content {
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return protocol.ProviderResponse{}, err
		}
		switch head.Type {
		case "text":
			var block textBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				return protocol.ProviderResponse{}, err
			}
			message.Content += block.Text
		case "tool_use":
			var block toolUseBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				return protocol.ProviderResponse{}, err
			}
			if strings.TrimSpace(block.Name) == "" {
				return protocol.ProviderResponse{}, errors.New("provider returned a tool_use block without a name")
			}
			input := bytes.TrimSpace(block.Input)
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			message.ToolCalls = append(message.ToolCalls, protocol.ToolCall{ID: block.ID, Name: block.Name, Arguments: input})
		case "thinking", "redacted_thinking":
			thinking = append(thinking, raw)
		}
	}
	return protocol.ProviderResponse{
		Message:      message,
		FinishReason: mapStopReason(resp.StopReason),
		Usage:        resp.Usage.toProtocol(),
		Echo:         thinkingEcho(thinking),
	}, nil
}

// mapStopReason maps an API stop reason onto the finish reasons the runtime records.
func mapStopReason(reason string) string {
	switch reason {
	case "end_turn", "stop_sequence", "refusal":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "":
		return ""
	default:
		return strings.ToLower(reason)
	}
}

// thinkingEcho wraps the raw thinking blocks as the response's Echo. The token size is estimated
// like the openai adapter's, from the bytes on the wire.
func thinkingEcho(blocks []json.RawMessage) protocol.Echo {
	if len(blocks) == 0 {
		return protocol.Echo{}
	}
	raw := mustJSON(blocks)
	return protocol.NewEcho(echoThinking, raw, (len(raw)+2)/3+4)
}

// streamEvent is one decoded SSE event. Delta stays raw: it is a content delta for
// content_block_delta and a stop-reason delta for message_delta.
type streamEvent struct {
	Type string `json:"type"`
	// Message carries the usage of message_start.
	Message *struct {
		Usage responseUsage `json:"usage"`
	} `json:"message"`
	Index        int             `json:"index"`
	ContentBlock json.RawMessage `json:"content_block"`
	Delta        json.RawMessage `json:"delta"`
	// Usage is the usage of message_delta.
	Usage *responseUsage `json:"usage"`
	// Error is the payload of an error event.
	Error *wireError `json:"error"`
}

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type deltaEvent struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Thinking  string `json:"thinking"`
	Signature string `json:"signature"`
	// PartialJSON is the fragment of a tool_use input.
	PartialJSON string `json:"partial_json"`
}

type streamBlock struct {
	kind string // text, tool_use, thinking, redacted_thinking
	id   string
	name string
	data string

	text      strings.Builder
	input     strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	hasSigned bool
}

type streamBuilder struct {
	blocks     map[int]*streamBlock
	stopReason string
	usage      responseUsage
	maxBytes   int64
	usedBytes  int64
}

func (b *streamBuilder) block(index int) *streamBlock {
	if b.blocks == nil {
		b.blocks = map[int]*streamBlock{}
	}
	block := b.blocks[index]
	if block == nil {
		block = &streamBlock{}
		b.blocks[index] = block
	}
	return block
}

func (b *streamBuilder) reserve(size int) error {
	b.usedBytes += int64(size)
	if b.usedBytes > b.maxBytes {
		return fmt.Errorf("provider assembled response exceeded %d bytes", b.maxBytes)
	}
	return nil
}

// apply folds one event into the response under construction.
func (b *streamBuilder) apply(event streamEvent) error {
	switch event.Type {
	case "message_start":
		if event.Message != nil {
			b.usage.InputTokens = event.Message.Usage.InputTokens
			b.usage.CacheCreationInputTokens = event.Message.Usage.CacheCreationInputTokens
			b.usage.CacheReadInputTokens = event.Message.Usage.CacheReadInputTokens
		}
	case "content_block_start":
		block := b.block(event.Index)
		var head struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(event.ContentBlock, &head); err != nil {
			return err
		}
		block.kind = head.Type
		switch head.Type {
		case "tool_use":
			var start struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if err := json.Unmarshal(event.ContentBlock, &start); err != nil {
				return err
			}
			block.id, block.name = start.ID, start.Name
			if err := b.reserve(len(start.ID) + len(start.Name)); err != nil {
				return err
			}
		case "redacted_thinking":
			var start struct {
				Data string `json:"data"`
			}
			if err := json.Unmarshal(event.ContentBlock, &start); err != nil {
				return err
			}
			block.data = start.Data
			if err := b.reserve(len(start.Data)); err != nil {
				return err
			}
		}
	case "content_block_delta":
		var delta deltaEvent
		if err := json.Unmarshal(event.Delta, &delta); err != nil {
			return err
		}
		block := b.block(event.Index)
		switch delta.Type {
		case "text_delta":
			if err := b.reserve(len(delta.Text)); err != nil {
				return err
			}
			block.text.WriteString(delta.Text)
		case "input_json_delta":
			if err := b.reserve(len(delta.PartialJSON)); err != nil {
				return err
			}
			block.input.WriteString(delta.PartialJSON)
		case "thinking_delta":
			if err := b.reserve(len(delta.Thinking)); err != nil {
				return err
			}
			block.thinking.WriteString(delta.Thinking)
		case "signature_delta":
			if err := b.reserve(len(delta.Signature)); err != nil {
				return err
			}
			block.signature.WriteString(delta.Signature)
			block.hasSigned = true
		}
	case "content_block_stop":
		// Nothing to fold: the block is complete in place.
	case "message_delta":
		if len(event.Delta) > 0 {
			var delta struct {
				StopReason string `json:"stop_reason"`
			}
			if err := json.Unmarshal(event.Delta, &delta); err != nil {
				return err
			}
			if delta.StopReason != "" {
				b.stopReason = delta.StopReason
			}
		}
		if event.Usage != nil {
			b.usage.OutputTokens = event.Usage.OutputTokens
		}
	case "message_stop", "ping":
		// message_stop ends the stream (the reader stops on it); ping is a keepalive.
	case "error":
		if event.Error == nil {
			return &streamError{errType: "invalid_request_error", message: "the stream failed without an error payload"}
		}
		return &streamError{errType: event.Error.Type, message: event.Error.Message}
	default:
		// Unknown event types are ignored: the API adds keepalives and annotations without notice.
	}
	return nil
}

// result assembles the protocol response from the folded events.
func (b *streamBuilder) result() (protocol.ProviderResponse, error) {
	indexes := make([]int, 0, len(b.blocks))
	for index := range b.blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	message := protocol.Message{Role: "assistant"}
	var thinking []json.RawMessage
	for _, index := range indexes {
		block := b.blocks[index]
		switch block.kind {
		case "text":
			message.Content += block.text.String()
		case "tool_use":
			if strings.TrimSpace(block.name) == "" {
				return protocol.ProviderResponse{}, errors.New("provider streamed a tool_use block without a name")
			}
			input := strings.TrimSpace(block.input.String())
			if input == "" {
				input = "{}"
			}
			message.ToolCalls = append(message.ToolCalls, protocol.ToolCall{ID: block.id, Name: block.name, Arguments: json.RawMessage(input)})
		case "thinking":
			// Reassembled to the non-streamed shape: the same fields the API sends in one block.
			thinking = append(thinking, mustJSON(struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature,omitempty"`
			}{Type: "thinking", Thinking: block.thinking.String(), Signature: block.signature.String()}))
		case "redacted_thinking":
			thinking = append(thinking, mustJSON(struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}{Type: "redacted_thinking", Data: block.data}))
		}
	}
	return protocol.ProviderResponse{
		Message:      message,
		FinishReason: mapStopReason(b.stopReason),
		Usage:        b.usage.toProtocol(),
		Echo:         thinkingEcho(thinking),
	}, nil
}

// streamError is an error event the API sent mid-stream.
type streamError struct {
	errType string
	message string
}

func (e *streamError) Error() string {
	if strings.TrimSpace(e.message) == "" {
		return "provider stream error: " + e.errType
	}
	return "provider stream error: " + e.errType + ": " + e.message
}
