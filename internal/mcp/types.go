package mcp

import "encoding/json"

const (
	ProtocolLegacy = "2025-11-25"
	ProtocolModern = "2026-07-28"

	MaxMessageBytes    = 4 << 20
	MaxToolResultBytes = 1 << 20
	MaxPendingRequests = 128
	MaxTools           = 512
	MaxToolPages       = 16
)

type ProtocolMode string

const (
	ModeLegacy ProtocolMode = "legacy"
	ModeModern ProtocolMode = "modern"
)

type Config struct {
	ID           string       `json:"id"`
	Name         string       `json:"name,omitempty"`
	Command      string       `json:"command"`
	Args         []string     `json:"args,omitempty"`
	Workspace    string       `json:"workspace,omitempty"`
	ProtocolMode ProtocolMode `json:"protocolMode,omitempty"`
	Disabled     bool         `json:"disabled,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations,omitempty"`
}

type ToolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
}

type CallToolResult struct {
	Content           []ToolContent  `json:"content,omitempty"`
	StructuredContent map[string]any `json:"structuredContent,omitempty"`
	IsError           bool           `json:"isError,omitempty"`
}

type Status struct {
	ID              string       `json:"id"`
	Name            string       `json:"name,omitempty"`
	Command         string       `json:"command,omitempty"`
	Workspace       string       `json:"workspace,omitempty"`
	ProtocolMode    ProtocolMode `json:"protocolMode"`
	ProtocolVersion string       `json:"protocolVersion,omitempty"`
	Running         bool         `json:"running"`
	PID             int          `json:"pid,omitempty"`
	PendingRequests int          `json:"pendingRequests"`
	Tools           int          `json:"tools"`
	LastError       string       `json:"lastError,omitempty"`
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return e.Message }

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    error
}
