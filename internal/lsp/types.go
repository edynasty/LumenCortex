package lsp

import (
	"encoding/json"
	"log/slog"
	"time"
)

const (
	MaxMessageBytes    = 4 << 20
	MaxDocumentBytes   = 2 << 20
	MaxPendingRequests = 128
	MaxDiagnostics     = 2000
)

// Manager defaults. A server is started by the first request that needs it, reused for the same
// workspace and language, and stopped once it has been unused for IdleTimeout.
const (
	DefaultIdleTimeout    = 10 * time.Minute
	DefaultMaxServers     = 3
	DefaultStartTimeout   = 20 * time.Second
	DefaultRequestTimeout = 20 * time.Second
	DefaultBackoffBase    = 500 * time.Millisecond
	// maxBackoff caps the wait before a crashed or unstartable server is tried again.
	maxBackoff = 30 * time.Second
	// The idle check runs on one manager-level ticker, never on a timer per request.
	minIdleTick = 25 * time.Millisecond
	maxIdleTick = 30 * time.Second
)

// Server states a status reports.
const (
	StateRunning     = "running"
	StateIdle        = "idle"
	StateStopped     = "stopped"
	StateUnavailable = "unavailable"
)

type Config struct {
	Name       string   `json:"name,omitempty"`
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	LanguageID string   `json:"languageId,omitempty"`
	Workspace  string   `json:"workspace"`
	// StartTimeout bounds the spawn and the initialize handshake; zero uses DefaultStartTimeout.
	StartTimeout time.Duration `json:"-"`
}

// LanguageServer is one language server a Manager may start: the command, and the files that show
// the workspace holds its language.
type LanguageServer struct {
	Language   string   `json:"language"`
	Name       string   `json:"name,omitempty"`
	Command    string   `json:"command"`
	Args       []string `json:"args,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
	Markers    []string `json:"markers,omitempty"`
}

// Options configures a Manager. Every zero tuning field takes its default; Servers empty uses
// DefaultServers. Logger nil discards the lifecycle log.
type Options struct {
	Workspace      string
	Servers        []LanguageServer
	Logger         *slog.Logger
	IdleTimeout    time.Duration
	MaxServers     int
	StartTimeout   time.Duration
	RequestTimeout time.Duration
	BackoffBase    time.Duration
	// OnEvent receives every lifecycle event. It must not block: the Engine publishes it on a bus
	// that drops events for a subscriber that cannot keep up.
	OnEvent func(Event)
}

// Event is one language server lifecycle step: started, ready, idle_stop, crashed, evicted,
// unavailable.
type Event struct {
	Type      string
	Language  string
	Workspace string
	PID       int
	Duration  time.Duration
	ErrorCode string
	Message   string
}

// ServerStatus is one language server as a host displays it. Nothing here starts a server.
type ServerStatus struct {
	Language   string     `json:"language"`
	Name       string     `json:"name,omitempty"`
	Command    string     `json:"command,omitempty"`
	Root       string     `json:"root"`
	State      string     `json:"state"`
	PID        int        `json:"pid,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
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

// TextEdit replaces the text in Range with NewText. Positions are UTF-16 code units, as the protocol
// specifies.
type TextEdit struct {
	Range   Range  `json:"range"`
	NewText string `json:"newText"`
}

// WorkspaceEdit is the edit textDocument/rename answers with. LSP allows two shapes: Changes keyed by
// URI, or DocumentChanges with a textDocument per entry. A DocumentChange with a Kind is a resource
// operation (create, rename, delete), which lsp_rename refuses rather than applies.
type WorkspaceEdit struct {
	Changes         map[string][]TextEdit `json:"changes,omitempty"`
	DocumentChanges []DocumentChange      `json:"documentChanges,omitempty"`
}

// DocumentChange is one entry of WorkspaceEdit.DocumentChanges.
type DocumentChange struct {
	Kind         string                           `json:"kind,omitempty"`
	TextDocument *VersionedTextDocumentIdentifier `json:"textDocument,omitempty"`
	Edits        []TextEdit                       `json:"edits,omitempty"`
}

// VersionedTextDocumentIdentifier names the document a change applies to.
type VersionedTextDocumentIdentifier struct {
	URI     string `json:"uri"`
	Version *int   `json:"version,omitempty"`
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
