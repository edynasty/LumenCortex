package protocol

import (
	"context"
	"encoding/json"
)

// Origins of rows the runtime itself wrote to the transcript. Origin is a field
// of the stored message JSON; provider adapters never send it.
const (
	// OriginToolInterrupted marks a tool row that reconciliation appended for a
	// call whose result was never saved.
	OriginToolInterrupted = "tool_interrupted"
	// OriginCompletionBlocked marks a user row the completion gate appended when it
	// refused the model's completion proposal.
	OriginCompletionBlocked = "completion_blocked"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	// Origin is set only on rows the runtime wrote (see the Origin constants).
	Origin string `json:"origin,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolSpec struct {
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	Parameters       map[string]any `json:"parameters"`
	Permission       string         `json:"permission,omitempty"`
	MutatesWorkspace bool           `json:"mutatesWorkspace,omitempty"`
}

type ToolResult struct {
	OK               bool   `json:"ok"`
	Denied           bool   `json:"denied,omitempty"`
	Permission       string `json:"permission,omitempty"`
	Content          string `json:"content"`
	MutatesWorkspace bool   `json:"mutatesWorkspace,omitempty"`
}

type ToolOutput struct {
	Stream string `json:"stream"`
	Chunk  string `json:"chunk"`
}

type Usage struct {
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	Requests         int64 `json:"requests"`
}

type ProviderRequest struct {
	Messages        []Message  `json:"messages"`
	Tools           []ToolSpec `json:"tools,omitempty"`
	ToolChoice      string     `json:"toolChoice,omitempty"`
	Temperature     *float64   `json:"temperature,omitempty"`
	MaxTokens       int        `json:"maxTokens,omitempty"`
	ReasoningEffort string     `json:"reasoningEffort,omitempty"`
}

type ProviderResponse struct {
	Message      Message `json:"message"`
	Usage        Usage   `json:"usage"`
	FinishReason string  `json:"finishReason,omitempty"`
}

type Provider interface {
	Complete(context.Context, ProviderRequest) (ProviderResponse, error)
	Model() string
}
