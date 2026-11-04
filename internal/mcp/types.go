package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	ProtocolLegacy = "2025-11-25"
	ProtocolModern = "2026-07-28"

	MaxMessageBytes    = 4 << 20
	MaxToolResultBytes = 1 << 20
	MaxPendingRequests = 128
	MaxTools           = 512
	MaxToolPages       = 16
)

// Transports a configured server can be reached with.
const (
	// TransportStdio runs Command as a child process and speaks newline-delimited JSON-RPC over its
	// stdin/stdout. It is the default when Transport is empty.
	TransportStdio = "stdio"
	// TransportHTTP speaks MCP Streamable HTTP to URL. A URL of the form unix:///abs/socket reaches
	// an HTTP endpoint served on a Unix socket (HTTP path /mcp, Host localhost).
	TransportHTTP = "streamable-http"
)

// Per-server execution limits. A configuration may lower them; the defaults apply when a field is
// zero.
const (
	DefaultCallTimeout        = 120 * time.Second
	DefaultIdleTimeout        = 10 * time.Minute
	DefaultMaxConcurrentCalls = 4
	DefaultCatalogTimeout     = 8 * time.Second

	MaxCallTimeout        = time.Hour
	MaxIdleTimeout        = 24 * time.Hour
	MaxConcurrentCallsCap = 32
	MaxCatalogTimeout     = 30 * time.Second
)

type ProtocolMode string

const (
	ModeLegacy ProtocolMode = "legacy"
	ModeModern ProtocolMode = "modern"
)

// ErrUntrustedProject reports that a server is defined by the project mcp.json of a workspace the
// user has not trusted (ADR-0013). Nothing was started.
var ErrUntrustedProject = errors.New("mcp server is defined by the project configuration of an untrusted workspace")

// ErrServerDisabled reports that a configured server is disabled, so no process was started.
var ErrServerDisabled = errors.New("mcp server is disabled")

// ErrMissingEnv reports that a configuration references an environment variable that is not set. The
// error names the variable, never a value.
type ErrMissingEnv struct{ Name string }

func (e *ErrMissingEnv) Error() string {
	return fmt.Sprintf("mcp server configuration references environment variable %s, which is not set", e.Name)
}

// Permissions an MCP tool is exposed with. A server-declared readOnlyHint makes its tools read;
// anything else is exec. Config.Tools overrides that per tool.
const (
	PermissionRead  = "read"
	PermissionWrite = "write"
	PermissionExec  = "exec"
)

// Where AgentTool.Permission came from.
const (
	PermissionSourceAnnotation = "annotation"
	PermissionSourceOverride   = "override"
)

// Where an AgentTool's tool list came from.
const (
	// ToolSourceLive means the server answered tools/list during this call.
	ToolSourceLive = "live"
	// ToolSourceCache means the tool list is the catalog persisted from an earlier start.
	ToolSourceCache = "cache"
)

type Config struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Transport is "stdio" (default) or "streamable-http".
	Transport Transport `json:"transport,omitempty"`
	// Command and Args name the stdio server process.
	Command string   `json:"command,omitempty"`
	Args    []string `json:"args,omitempty"`
	// URL is the Streamable HTTP endpoint of a remote or local-http server. unix:///abs/socket reaches
	// an endpoint served on a Unix socket.
	URL string `json:"url,omitempty"`
	// Headers are sent with every HTTP request. A value may be an environment reference
	// ("{env:NAME}"), resolved when the request is made; the resolved value is never persisted.
	Headers map[string]string `json:"headers,omitempty"`
	// Env is the environment of a stdio server. A value may be an environment reference
	// ("{env:NAME}"), resolved when the process starts.
	Env map[string]string `json:"env,omitempty"`
	// Workspace is the directory a stdio server runs in; it is set by the runtime, never persisted.
	Workspace    string       `json:"workspace,omitempty"`
	ProtocolMode ProtocolMode `json:"protocolMode,omitempty"`
	Disabled     bool         `json:"disabled,omitempty"`
	// Tools overrides the permission of individual tools of this server, keyed by the tool name the
	// server reports. An override is honoured only when the entry is user-owned: from the global
	// file, or from the project file of a trusted workspace.
	Tools map[string]ToolConfig `json:"tools,omitempty"`
	// Include is the only set of tools a run may see from this server, by the name the server
	// reports. Empty means every tool the server reports, bounded by MaxTools.
	Include []string `json:"include,omitempty"`
	// CallTimeoutSeconds bounds one tool call; IdleTimeoutSeconds stops a server no call has used for
	// that long; MaxConcurrentCalls bounds calls in flight; MaxResultBytes lowers the tool result cap
	// for this server. Zero selects the default.
	CallTimeoutSeconds int `json:"callTimeoutSeconds,omitempty"`
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`
	MaxConcurrentCalls int `json:"maxConcurrentCalls,omitempty"`
	MaxResultBytes     int `json:"maxResultBytes,omitempty"`
	// Session ties the server's own sessions to the LumenCortex session that drove them: the id is
	// injected into one argument, and a session the server opened is closed when the runtime stops.
	Session *SessionBinding `json:"session,omitempty"`
	// Template names the capability template this entry was created from, when one was used.
	Template string `json:"template,omitempty"`
	// Scope and BlockedByTrust describe a listed configuration; they are never persisted.
	Scope          string `json:"scope,omitempty"`
	BlockedByTrust bool   `json:"blockedByTrust,omitempty"`
}

// SessionBinding ties a server's sessions to the LumenCortex session that drove them. It exists for
// servers that control a long-lived resource (a computer session): the runtime supplies the session id
// rather than trusting the model, remembers the server-side id a start tool returned, and closes it
// when the server is stopped, so nothing outlives the run that opened it.
type SessionBinding struct {
	// Argument is the tool argument that receives the LumenCortex session id.
	Argument string `json:"argument"`
	// StartTool opens a server-side session; SessionIDField names the field of its result that carries
	// the server-side id.
	StartTool      string `json:"startTool,omitempty"`
	SessionIDField string `json:"sessionIdField,omitempty"`
	// StopTool closes it; StopArgument is the argument that receives the server-side id.
	StopTool     string `json:"stopTool,omitempty"`
	StopArgument string `json:"stopArgument,omitempty"`
}

// Transport is how a server is reached.
type Transport string

// ToolConfig is one entry of Config.Tools.
type ToolConfig struct {
	Permission string `json:"permission,omitempty"`
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
	URL             string       `json:"url,omitempty"`
	Transport       Transport    `json:"transport"`
	Workspace       string       `json:"workspace,omitempty"`
	ProtocolMode    ProtocolMode `json:"protocolMode"`
	ProtocolVersion string       `json:"protocolVersion,omitempty"`
	Running         bool         `json:"running"`
	PID             int          `json:"pid,omitempty"`
	PendingRequests int          `json:"pendingRequests"`
	Tools           int          `json:"tools"`
	LastError       string       `json:"lastError,omitempty"`
	// CallsInFlight is the number of tool calls this server is running now.
	CallsInFlight int `json:"callsInFlight"`
	// Scope is the file the running configuration came from, and BlockedByTrust reports that the
	// process belongs to the project file of an untrusted workspace: it keeps running, but its
	// tools are not exposed to any run.
	Scope          string `json:"scope,omitempty"`
	BlockedByTrust bool   `json:"blockedByTrust,omitempty"`
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

// envReference matches an environment reference, "{env:NAME}". A configuration carries only the
// reference; the value is read from the environment when a process starts or a request is made, so a
// credential is never written into mcp.json, a listing, an error or a log line.
var envReference = regexp.MustCompile(`\{env:([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandEnv replaces every "{env:NAME}" reference in value with the environment. A reference to an
// unset or empty variable is an *ErrMissingEnv naming the variable and never the value.
func expandEnv(value string) (string, error) {
	if !strings.Contains(value, "{env:") {
		return value, nil
	}
	var missing string
	out := envReference.ReplaceAllStringFunc(value, func(reference string) string {
		name := envReference.FindStringSubmatch(reference)[1]
		resolved := os.Getenv(name)
		if resolved == "" {
			if missing == "" {
				missing = name
			}
			return ""
		}
		return resolved
	})
	if missing != "" {
		return "", &ErrMissingEnv{Name: missing}
	}
	return out, nil
}

// resolveEnvReference is expandEnv for a value that is entirely a reference or a literal.
func resolveEnvReference(value string) (string, error) { return expandEnv(value) }

// envRequirements lists the environment variables a configuration references, sorted and unique. It
// is what an availability check reports without starting anything.
func envRequirements(cfg Config) []string {
	seen := map[string]bool{}
	add := func(value string) {
		for _, match := range envReference.FindAllStringSubmatch(value, -1) {
			seen[match[1]] = true
		}
	}
	for _, value := range cfg.Env {
		add(value)
	}
	for _, value := range cfg.Headers {
		add(value)
	}
	add(cfg.URL)
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// MissingEnv lists the referenced environment variables that are unset, sorted.
func (c Config) MissingEnv() []string {
	var out []string
	for _, name := range envRequirements(c) {
		if os.Getenv(name) == "" {
			out = append(out, name)
		}
	}
	return out
}

func (c Config) transport() Transport {
	if c.Transport == "" {
		return TransportStdio
	}
	return c.Transport
}

func (c Config) callTimeout() time.Duration {
	if c.CallTimeoutSeconds <= 0 {
		return DefaultCallTimeout
	}
	return time.Duration(c.CallTimeoutSeconds) * time.Second
}

func (c Config) idleTimeout() time.Duration {
	if c.IdleTimeoutSeconds <= 0 {
		return DefaultIdleTimeout
	}
	return time.Duration(c.IdleTimeoutSeconds) * time.Second
}

func (c Config) maxConcurrent() int {
	if c.MaxConcurrentCalls <= 0 {
		return DefaultMaxConcurrentCalls
	}
	return c.MaxConcurrentCalls
}

func (c Config) maxResultBytes() int {
	if c.MaxResultBytes <= 0 || c.MaxResultBytes > MaxToolResultBytes {
		return MaxToolResultBytes
	}
	return c.MaxResultBytes
}

// included reports whether a server tool is exposed to runs. An empty Include exposes every tool.
func (c Config) included(toolName string) bool {
	if len(c.Include) == 0 {
		return true
	}
	for _, name := range c.Include {
		if name == toolName {
			return true
		}
	}
	return false
}

// ValidateConfig checks a server entry against the runtime's rules: the transport and its endpoint, the
// limit bounds, the include list and the session binding's tools. It is what a host write and a
// template apply, so neither can describe a server the runtime would refuse to start.
func ValidateConfig(cfg Config) error {
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := validateTools(cfg); err != nil {
		return err
	}
	return cfg.bindingError()
}

// bindingError reports a session binding whose start or stop tool is outside a non-empty include list:
// the runtime calls those tools itself (the stop tool when the server stops), and it calls no tool a
// run could not, so such a binding would leave every session a run opened open. A host write refuses
// it; an entry already on disk with it loads, but its server refuses to start (ADR-0021 D5).
func (c Config) bindingError() error {
	binding := c.Session
	if binding == nil {
		return nil
	}
	for _, tool := range []struct{ field, name string }{{"startTool", binding.StartTool}, {"stopTool", binding.StopTool}} {
		if tool.name != "" && !c.included(tool.name) {
			return fmt.Errorf("mcp server %q: session.%s %q is not in the include list; a bound server's start and stop tools must stay included", c.ID, tool.field, tool.name)
		}
	}
	return nil
}

// validateConfig checks the fields that decide whether a server can be started at all. The error
// names the server and the field, so a bad mcp.json entry points at what to fix.
func validateConfig(cfg Config) error {
	switch cfg.transport() {
	case TransportStdio:
		if strings.TrimSpace(cfg.Command) == "" {
			return fmt.Errorf("mcp server %q: command is required for the stdio transport", cfg.ID)
		}
		if strings.TrimSpace(cfg.URL) != "" {
			return fmt.Errorf("mcp server %q: url is only valid for the streamable-http transport", cfg.ID)
		}
	case TransportHTTP:
		if strings.TrimSpace(cfg.URL) == "" {
			return fmt.Errorf("mcp server %q: url is required for the streamable-http transport", cfg.ID)
		}
		if strings.TrimSpace(cfg.Command) != "" {
			return fmt.Errorf("mcp server %q: command is only valid for the stdio transport", cfg.ID)
		}
	default:
		return fmt.Errorf("mcp server %q: transport must be %q or %q", cfg.ID, TransportStdio, TransportHTTP)
	}
	if cfg.CallTimeoutSeconds < 0 || time.Duration(cfg.CallTimeoutSeconds)*time.Second > MaxCallTimeout {
		return fmt.Errorf("mcp server %q: callTimeoutSeconds must be between 0 and %d", cfg.ID, int(MaxCallTimeout.Seconds()))
	}
	if cfg.IdleTimeoutSeconds < 0 || time.Duration(cfg.IdleTimeoutSeconds)*time.Second > MaxIdleTimeout {
		return fmt.Errorf("mcp server %q: idleTimeoutSeconds must be between 0 and %d", cfg.ID, int(MaxIdleTimeout.Seconds()))
	}
	if cfg.MaxConcurrentCalls < 0 || cfg.MaxConcurrentCalls > MaxConcurrentCallsCap {
		return fmt.Errorf("mcp server %q: maxConcurrentCalls must be between 0 and %d", cfg.ID, MaxConcurrentCallsCap)
	}
	if cfg.MaxResultBytes < 0 || cfg.MaxResultBytes > MaxToolResultBytes {
		return fmt.Errorf("mcp server %q: maxResultBytes must be between 0 and %d", cfg.ID, MaxToolResultBytes)
	}
	if session := cfg.Session; session != nil {
		if strings.TrimSpace(session.Argument) == "" {
			return fmt.Errorf("mcp server %q: session.argument is required", cfg.ID)
		}
		if (session.StopTool == "") != (session.StopArgument == "") {
			return fmt.Errorf("mcp server %q: session.stopTool and session.stopArgument are set together", cfg.ID)
		}
		if session.StartTool != "" && strings.TrimSpace(session.SessionIDField) == "" {
			return fmt.Errorf("mcp server %q: session.sessionIdField is required with startTool", cfg.ID)
		}
	}
	for _, name := range cfg.Include {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("mcp server %q: include entries must be non-empty tool names", cfg.ID)
		}
	}
	return nil
}
