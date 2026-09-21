package protocol

import (
	"context"
	"encoding/json"
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
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
	Messages    []Message  `json:"messages"`
	Tools       []ToolSpec `json:"tools,omitempty"`
	ToolChoice  string     `json:"toolChoice,omitempty"`
	Temperature *float64   `json:"temperature,omitempty"`
	MaxTokens   int        `json:"maxTokens,omitempty"`
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
