package lsp

import "encoding/json"

const (
	MaxMessageBytes    = 4 << 20
	MaxDocumentBytes   = 2 << 20
	MaxPendingRequests = 128
	MaxDiagnostics     = 2000
)

type Config struct {
	Name       string   `json:"name,omitempty"`
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	LanguageID string   `json:"languageId,omitempty"`
	Workspace  string   `json:"workspace"`
}

type Status struct {
	Running         bool   `json:"running"`
	Name            string `json:"name,omitempty"`
	Command         string `json:"command,omitempty"`
	PID             int    `json:"pid,omitempty"`
	PendingRequests int    `json:"pendingRequests"`
	Diagnostics     int    `json:"diagnostics"`
	LastError       string `json:"lastError,omitempty"`
}

type Position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

type Diagnostic struct {
	Range    Range           `json:"range"`
	Severity int             `json:"severity,omitempty"`
	Code     json.RawMessage `json:"code,omitempty"`
	Source   string          `json:"source,omitempty"`
	Message  string          `json:"message"`
}

type PublishDiagnosticsParams struct {
	URI         string       `json:"uri"`
	Version     *int         `json:"version,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics"`
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    error
}

type documentState struct {
	version int
	content string
}
