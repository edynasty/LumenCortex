package openai

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

type chatRequest struct {
	Model           string            `json:"model"`
	Messages        []wireMessage     `json:"messages"`
	Tools           []wireTool        `json:"tools,omitempty"`
	ToolChoice      any               `json:"tool_choice,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxTokens       int               `json:"max_tokens,omitempty"`
	Reasoning       *reasoningOptions `json:"reasoning,omitempty"`
	ReasoningEffort string            `json:"reasoning_effort,omitempty"`
	Thinking        *thinkingOptions  `json:"thinking,omitempty"`
	Stream          bool              `json:"stream,omitempty"`
	StreamOptions   *streamOpts       `json:"stream_options,omitempty"`
}

type reasoningOptions struct {
	Effort string `json:"effort"`
}

type thinkingOptions struct {
	Type string `json:"type"`
}

type streamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
	// ReasoningContent is read from responses into the response's Echo and written back only from a
	// request's Echo; it never reaches a protocol.Message.
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

type wireToolCall struct {
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type chatResponse struct {
	Choices []choice  `json:"choices"`
	Usage   wireUsage `json:"usage"`
}

type choice struct {
	Message      wireMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type wireUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type streamResponse struct {
	Choices []streamChoice `json:"choices"`
	Usage   *wireUsage     `json:"usage,omitempty"`
}

type streamChoice struct {
	Delta        streamDelta `json:"delta"`
	FinishReason *string     `json:"finish_reason"`
}

type streamDelta struct {
	Role             string           `json:"role,omitempty"`
	Content          string           `json:"content,omitempty"`
	ReasoningContent string           `json:"reasoning_content,omitempty"`
	ToolCalls        []streamToolCall `json:"tool_calls,omitempty"`
}

type streamToolCall struct {
	Index    int          `json:"index"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireFunction `json:"function"`
}

// echoReasoningContent is the reasoning.echo mode, and the Echo kind, that carries reasoning_content.
const echoReasoningContent = config.ReasoningEchoContent

func requestFromProtocol(model string, req protocol.ProviderRequest, stream bool, reasoningFormat, echo string) chatRequest {
	messages := make([]wireMessage, 0, len(req.Messages))
	for i, message := range req.Messages {
		wire := wireMessage{
			Role: message.Role, Content: message.Content, Name: message.Name, ToolCallID: message.ToolCallID,
		}
		for _, call := range message.ToolCalls {
			args := wireArguments(call.Arguments)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			wire.ToolCalls = append(wire.ToolCalls, wireToolCall{
				ID: call.ID, Type: "function",
				Function: wireFunction{Name: call.Name, Arguments: args},
			})
		}
		// The loop puts echo only on current-turn assistant messages of this model (design §4.6).
		if echo == echoReasoningContent && message.Role == "assistant" && i < len(req.Echo) && req.Echo[i].Kind() == echoReasoningContent {
			wire.ReasoningContent = string(req.Echo[i].Data())
		}
		messages = append(messages, wire)
	}
	tools := make([]wireTool, 0, len(req.Tools))
	for _, spec := range req.Tools {
		tools = append(tools, wireTool{
			Type:     "function",
			Function: wireToolFunction{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters},
		})
	}
	var toolChoice any
	if req.ToolChoice != "" {
		toolChoice = req.ToolChoice
	}
	out := chatRequest{
		Model: model, Messages: messages, Tools: tools, ToolChoice: toolChoice,
		Temperature: req.Temperature, MaxTokens: req.MaxTokens, Stream: stream,
	}
	applyReasoning(&out, reasoningFormat, req.ReasoningEffort)
	if stream {
		out.StreamOptions = &streamOpts{IncludeUsage: true}
	}
	return out
}

// wireArguments is the text sent as function.arguments. A stored JSON string always
// holds the model's raw text (arguments that were not valid JSON, or were a JSON
// string): the wire carries that text back unchanged instead of its quoted form.
// Any other value, null included, is sent as its JSON text.
func wireArguments(raw json.RawMessage) string {
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			return text
		}
	}
	return string(raw)
}

// responseEcho is the Echo of a response's reasoning_content under the echo mode; zero when the mode
// does not echo or the model returned none. Its token size is estimated like the window's (§5).
func responseEcho(echo, reasoning string) protocol.Echo {
	if echo != echoReasoningContent || reasoning == "" {
		return protocol.Echo{}
	}
	return protocol.NewEcho(echoReasoningContent, []byte(reasoning), (len(reasoning)+2)/3+4)
}

func (r chatResponse) toProtocol(echo string) (protocol.ProviderResponse, error) {
	if len(r.Choices) == 0 {
		return protocol.ProviderResponse{}, errors.New("provider response contained no choices")
	}
	message, err := r.Choices[0].Message.toProtocol()
	if err != nil {
		return protocol.ProviderResponse{}, err
	}
	return protocol.ProviderResponse{
		Message:      message,
		FinishReason: r.Choices[0].FinishReason,
		Usage: protocol.Usage{
			PromptTokens:     r.Usage.PromptTokens,
			CompletionTokens: r.Usage.CompletionTokens,
			TotalTokens:      r.Usage.TotalTokens,
		},
		Echo: responseEcho(echo, r.Choices[0].Message.ReasoningContent),
	}, nil
}

func (m wireMessage) toProtocol() (protocol.Message, error) {
	out := protocol.Message{Role: m.Role, Content: m.Content, Name: m.Name, ToolCallID: m.ToolCallID}
	for _, call := range m.ToolCalls {
		if strings.TrimSpace(call.Function.Name) == "" {
			return protocol.Message{}, errors.New("provider returned tool call without a function name")
		}
		args := strings.TrimSpace(call.Function.Arguments)
		if args == "" {
			args = "{}"
		}
		out.ToolCalls = append(out.ToolCalls, protocol.ToolCall{
			ID: call.ID, Name: call.Function.Name, Arguments: json.RawMessage(args),
		})
	}
	return out, nil
}

type streamBuilder struct {
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    map[int]*partialToolCall
	finishReason string
	usage        wireUsage
	maxBytes     int64
	usedBytes    int64
}

type partialToolCall struct {
	id        string
	name      strings.Builder
	arguments strings.Builder
}

func (b *streamBuilder) apply(event streamResponse) error {
	if event.Usage != nil {
		b.usage = *event.Usage
	}
	for _, choice := range event.Choices {
		if choice.FinishReason != nil {
			b.finishReason = *choice.FinishReason
		}
		if choice.Delta.Content != "" {
			if err := b.reserve(len(choice.Delta.Content)); err != nil {
				return err
			}
			b.content.WriteString(choice.Delta.Content)
		}
		if choice.Delta.ReasoningContent != "" {
			if err := b.reserve(len(choice.Delta.ReasoningContent)); err != nil {
				return err
			}
			b.reasoning.WriteString(choice.Delta.ReasoningContent)
		}
		for _, call := range choice.Delta.ToolCalls {
			if b.toolCalls == nil {
				b.toolCalls = map[int]*partialToolCall{}
			}
			partial := b.toolCalls[call.Index]
			if partial == nil {
				partial = &partialToolCall{}
				b.toolCalls[call.Index] = partial
			}
			if call.ID != "" {
				if err := b.reserve(len(call.ID)); err != nil {
					return err
				}
				partial.id = call.ID
			}
			if call.Function.Name != "" {
				if err := b.reserve(len(call.Function.Name)); err != nil {
					return err
				}
				partial.name.WriteString(call.Function.Name)
			}
			if call.Function.Arguments != "" {
				if err := b.reserve(len(call.Function.Arguments)); err != nil {
					return err
				}
				partial.arguments.WriteString(call.Function.Arguments)
			}
		}
	}
	return nil
}

func (b *streamBuilder) reserve(size int) error {
	b.usedBytes += int64(size)
	if b.usedBytes > b.maxBytes {
		return fmt.Errorf("provider assembled response exceeded %d bytes", b.maxBytes)
	}
	return nil
}

func (b *streamBuilder) result(echo string) protocol.ProviderResponse {
	message := protocol.Message{Role: "assistant", Content: b.content.String()}
	if len(b.toolCalls) > 0 {
		indexes := make([]int, 0, len(b.toolCalls))
		for index := range b.toolCalls {
			indexes = append(indexes, index)
		}
		sort.Ints(indexes)
		for _, index := range indexes {
			partial := b.toolCalls[index]
			args := partial.arguments.String()
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			message.ToolCalls = append(message.ToolCalls, protocol.ToolCall{
				ID: partial.id, Name: partial.name.String(), Arguments: json.RawMessage(args),
			})
		}
	}
	return protocol.ProviderResponse{
		Message:      message,
		FinishReason: b.finishReason,
		Usage: protocol.Usage{
			PromptTokens:     b.usage.PromptTokens,
			CompletionTokens: b.usage.CompletionTokens,
			TotalTokens:      b.usage.TotalTokens,
		},
		Echo: responseEcho(echo, b.reasoning.String()),
	}
}

func applyReasoning(out *chatRequest, format, effort string) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "reasoning-object":
		switch effort {
		case "none", "low", "medium", "high", "max":
			out.Reasoning = &reasoningOptions{Effort: effort}
		}
	case "reasoning-effort":
		if effort == "max" {
			effort = "high"
		}
		switch effort {
		case "low", "medium", "high":
			out.ReasoningEffort = effort
		}
	case "deepseek":
		if effort == "medium" {
			effort = "high"
		}
		switch effort {
		case "none":
			out.ReasoningEffort = effort
			out.Thinking = &thinkingOptions{Type: "disabled"}
		case "low", "high", "max":
			out.ReasoningEffort = effort
			out.Thinking = &thinkingOptions{Type: "enabled"}
		}
	}
}
