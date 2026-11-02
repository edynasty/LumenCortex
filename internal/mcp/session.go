package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// session is what the registry drives: the stdio client or the Streamable HTTP client.
type session interface {
	Request(ctx context.Context, method string, params map[string]any, result any) error
	Notify(method string, params map[string]any) error
	ListTools(ctx context.Context) ([]Tool, error)
	CallTool(ctx context.Context, name string, arguments map[string]any) (CallToolResult, error)
	Tools() []Tool
	Status() Status
	Close() error
}

// ErrNotConnected reports that a server is configured but not running, and the call that needed it
// could not start it (disabled, untrusted project entry, or a start failure reported as lastError).
var ErrNotConnected = errors.New("mcp server is not running")

// server is the lifecycle of one configured server in one runtime workspace. Nothing starts it until
// a run needs its tool list or calls one of its tools; an idle server is stopped after its configured
// idle timeout; a start, a call and a stop are serialized per server, and a bounded number of calls
// may run at once.
//
// The lifecycle is deliberately the same for both transports: a stdio server is a child process, an
// HTTP server is a connection pool, and both must be created on demand and released when idle.
type server struct {
	registry  *Registry
	key       string
	workspace string

	mu        sync.Mutex
	cfg       Config
	scope     string
	current   session
	starting  bool
	startDone chan struct{}
	// lastUsed is when the last call or tool listing touched the server; the idle timer stops it.
	lastUsed   time.Time
	idleTimer  *time.Timer
	lastErr    string
	calls      int
	catalog    []Tool
	catalogSet bool
	// sem bounds calls in flight; it is created when the server starts and sized from its config.
	sem chan struct{}
	// boundSessions are the server-side sessions this runtime opened through the configured start
	// tool, closed when the server stops.
	boundSessions []string
	// lastStartAttempt is when a start was last tried, and lastStartFailed whether it failed: an
	// automatic catalog discovery backs off after a failure so a broken server is not started on every
	// run (an explicit call still retries immediately).
	lastStartAttempt time.Time
	lastStartFailed  bool
}

func newServer(registry *Registry, key, workspace string, cfg Config, scope string) *server {
	return &server{registry: registry, key: key, workspace: workspace, cfg: cfg, scope: scope}
}

// configure replaces the configuration and scope. When the configuration that produced the running
// client changed, the client is taken with the server-side sessions it opened, for the caller to close
// outside every lock.
func (s *server) configure(cfg Config, scope string) release {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := !sameSpawnConfig(s.cfg, cfg) || s.scope != scope
	var taken release
	if changed {
		// The entry was edited or moved between files: the process it started no longer describes it.
		// The sessions it opened are closed with the binding that opened them, before s.cfg changes.
		taken = s.takeLocked()
		s.catalogSet = false
	}
	s.cfg = cfg
	s.scope = scope
	return taken
}

func sameSpawnConfig(a, b Config) bool {
	if a.Command != b.Command || a.URL != b.URL || a.transport() != b.transport() ||
		a.ProtocolMode != b.ProtocolMode || a.Disabled != b.Disabled ||
		a.CallTimeoutSeconds != b.CallTimeoutSeconds || a.IdleTimeoutSeconds != b.IdleTimeoutSeconds ||
		a.MaxConcurrentCalls != b.MaxConcurrentCalls || a.MaxResultBytes != b.MaxResultBytes ||
		len(a.Include) != len(b.Include) || len(a.Args) != len(b.Args) ||
		len(a.Headers) != len(b.Headers) || len(a.Env) != len(b.Env) {
		return false
	}
	for i := range a.Args {
		if a.Args[i] != b.Args[i] {
			return false
		}
	}
	for name, value := range a.Headers {
		if b.Headers[name] != value {
			return false
		}
	}
	for name, value := range a.Env {
		if b.Env[name] != value {
			return false
		}
	}
	for i := range a.Include {
		if a.Include[i] != b.Include[i] {
			return false
		}
	}
	return true
}

// config returns the current configuration and scope.
func (s *server) config() (Config, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg, s.scope
}

// status reports the server without starting it.
func (s *server) status(trusted bool) Status {
	s.mu.Lock()
	cfg, scope, current, lastErr, calls := s.cfg, s.scope, s.current, s.lastErr, s.calls
	s.mu.Unlock()
	var status Status
	if current != nil {
		status = current.Status()
	} else {
		status = Status{
			ID:           cfg.ID,
			Name:         cfg.Name,
			Command:      cfg.Command,
			URL:          cfg.URL,
			Transport:    cfg.transport(),
			Workspace:    s.workspace,
			ProtocolMode: cfg.ProtocolMode,
			Running:      false,
		}
	}
	status.Scope = scope
	status.BlockedByTrust = scope == ScopeProject && !trusted
	status.CallsInFlight = calls
	if lastErr != "" {
		status.LastError = lastErr
	}
	return status
}

// tools returns the server's tool catalog without starting it: the live list when it runs, else the
// catalog persisted by an earlier start. source says which.
func (s *server) tools() ([]Tool, string) {
	s.mu.Lock()
	current := s.current
	catalog, known := s.catalog, s.catalogSet
	cfg, scope := s.cfg, s.scope
	s.mu.Unlock()
	if current != nil {
		if live := current.Tools(); len(live) > 0 {
			return live, ToolSourceLive
		}
	}
	if known {
		return catalog, ToolSourceCache
	}
	if cached, ok := s.registry.loadCatalog(s.key, cfg, scope); ok {
		s.mu.Lock()
		if !s.catalogSet {
			s.catalog, s.catalogSet = cached, true
		}
		s.mu.Unlock()
		return cached, ToolSourceCache
	}
	return nil, ""
}

// ensure returns a running session, starting the server if it is not running. Concurrent callers
// share one start. The caller's context bounds its own wait; the start itself is bounded by the
// catalog timeout so a server that never answers cannot hold a run.
func (s *server) ensure(ctx context.Context) (session, error) {
	s.mu.Lock()
	if s.cfg.Disabled {
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrServerDisabled, s.cfg.ID)
	}
	if s.current != nil {
		current := s.current
		s.touchLocked()
		s.mu.Unlock()
		return current, nil
	}
	if s.starting {
		done := s.startDone
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		s.mu.Lock()
		current := s.current
		lastErr := s.lastErr
		s.mu.Unlock()
		if current != nil {
			return current, nil
		}
		if lastErr == "" {
			lastErr = ErrNotConnected.Error()
		}
		return nil, fmt.Errorf("%w: %s", ErrNotConnected, lastErr)
	}
	s.starting = true
	s.startDone = make(chan struct{})
	done := s.startDone
	cfg := s.cfg
	workspace := s.workspace
	s.mu.Unlock()

	started, err := s.start(ctx, cfg, workspace)

	s.mu.Lock()
	s.starting = false
	s.lastStartAttempt = time.Now()
	s.lastStartFailed = err != nil
	if err == nil {
		s.current = started
		s.sem = make(chan struct{}, cfg.maxConcurrent())
		s.lastErr = ""
		s.touchLocked()
	} else {
		s.lastErr = err.Error()
	}
	close(done)
	current := s.current
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return current, nil
}

// start creates the client for the configuration. A stdio command that cannot be resolved fails here,
// before a process exists, so a missing binary is a start error rather than a spawn failure.
func (s *server) start(ctx context.Context, cfg Config, workspace string) (session, error) {
	startCtx, cancel := context.WithTimeout(ctx, DefaultCatalogTimeout)
	defer cancel()
	spawn := cfg
	spawn.Workspace = workspace
	switch cfg.transport() {
	case TransportHTTP:
		client, err := startHTTPClient(startCtx, spawn)
		if err != nil {
			return nil, err
		}
		s.recordCatalog(cfg, client.Tools())
		return client, nil
	default:
		if path, err := exec.LookPath(cfg.Command); err != nil {
			return nil, fmt.Errorf("mcp server %s: command %q is not available: %w", cfg.ID, cfg.Command, err)
		} else {
			spawn.Command = path
		}
		client, err := Start(startCtx, spawn)
		if err != nil {
			return nil, err
		}
		s.recordCatalog(cfg, client.Tools())
		return client, nil
	}
}

// recordCatalog stores the tool list a start discovered, in memory and, when the registry has a state
// directory, on disk, so a later run can offer the tools without starting the server.
func (s *server) recordCatalog(cfg Config, tools []Tool) {
	if len(tools) == 0 {
		return
	}
	s.mu.Lock()
	s.catalog = tools
	s.catalogSet = true
	scope := s.scope
	s.mu.Unlock()
	s.registry.saveCatalog(s.key, cfg, scope, tools)
}

// refreshTools asks a running server for its tool list and records it.
func (s *server) refreshTools(ctx context.Context) ([]Tool, error) {
	client, err := s.ensure(ctx)
	if err != nil {
		return nil, err
	}
	tools, err := client.ListTools(ctx)
	if err != nil {
		s.setLastError(err.Error())
		return nil, err
	}
	cfg, _ := s.config()
	s.recordCatalog(cfg, tools)
	return tools, nil
}

// callTool runs one tool call with the server's own deadline, the run's tool deadline and the
// caller's cancellation, whichever is earliest, and with a bounded number of calls in flight.
func (s *server) callTool(ctx context.Context, toolName string, sessionID string, args map[string]any) (CallToolResult, error) {
	cfg, _ := s.config()
	// The include list is the server's exposure (ADR-0021 D4): a name outside it is refused here, before
	// anything starts, whatever tool list the caller was built from.
	if !cfg.included(toolName) {
		return CallToolResult{}, fmt.Errorf("mcp tool %s/%s is not in the server's include list", cfg.ID, toolName)
	}
	if binding := cfg.Session; binding != nil && sessionID != "" {
		// The runtime supplies the session id; a model-supplied value is replaced, so an action cannot
		// be attributed to another session.
		copied := make(map[string]any, len(args)+1)
		for key, value := range args {
			copied[key] = value
		}
		copied[binding.Argument] = sessionID
		args = copied
	}
	client, err := s.ensure(ctx)
	if err != nil {
		return CallToolResult{}, err
	}
	s.mu.Lock()
	sem := s.sem
	s.mu.Unlock()
	if sem != nil {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return CallToolResult{}, ctx.Err()
		}
		defer func() { <-sem }()
	}

	callCtx, cancel := context.WithTimeout(ctx, cfg.callTimeout())
	defer cancel()
	if deadline, ok := toolDeadline(callCtx); ok {
		var deadlineCancel context.CancelFunc
		callCtx, deadlineCancel = context.WithDeadline(callCtx, deadline)
		defer deadlineCancel()
	}

	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	result, callErr := client.CallTool(callCtx, toolName, args)
	// A call is use whatever its outcome: the idle timer counts from its end, not from its start.
	s.mu.Lock()
	s.calls--
	s.touchLocked()
	s.mu.Unlock()

	switch {
	case callErr == nil:
		s.clearLastError()
		s.noteBoundSession(cfg, toolName, result)
		return result, nil
	case errors.Is(callErr, context.DeadlineExceeded):
		err = fmt.Errorf("mcp tool %s/%s timed out: %w", cfg.ID, toolName, callErr)
	case errors.Is(callErr, ErrClosed), errors.Is(callErr, ErrToolResultTooLarge):
		err = fmt.Errorf("mcp tool %s/%s failed: %w", cfg.ID, toolName, callErr)
		// A closed client is a crashed or exited server: drop it so the next call starts a fresh one.
		s.dropClient(client)
	default:
		err = fmt.Errorf("mcp tool %s/%s failed: %w", cfg.ID, toolName, callErr)
		if isTransportFailure(callErr) {
			// The process is gone (EOF, a closed pipe): forget the client so the next call starts a
			// fresh one instead of reusing a dead session.
			s.dropClient(client)
		}
	}
	s.setLastError(err.Error())
	return CallToolResult{}, err
}

// isTransportFailure reports an error that means the connection to the server died, rather than the
// server refusing the call: an EOF, a closed client, a closed pipe or a reset connection.
func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, ErrClosed) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "broken pipe") || strings.Contains(message, "connection reset") ||
		strings.Contains(message, "use of closed network connection")
}

func (s *server) touchLocked() {
	s.lastUsed = time.Now()
	if s.current == nil {
		return
	}
	s.armIdleTimerLocked()
}

// armIdleTimerLocked (re)starts the timer that stops an idle server. The timer callback takes the
// lock, so a call that arrives first wins.
func (s *server) armIdleTimerLocked() {
	timeout := s.cfg.idleTimeout()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(timeout, s.stopIdle)
}

func (s *server) stopIdleTimerLocked() {
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// stopIdle closes a server that has not been used for its idle timeout. It is the only place a
// server is stopped for inactivity; Stop and Close stop explicitly. A server with a call in flight is
// in use: the timer is re-armed, and the call's end counts as its last use.
func (s *server) stopIdle() {
	s.mu.Lock()
	if s.current == nil {
		s.idleTimer = nil
		s.mu.Unlock()
		return
	}
	timeout := s.cfg.idleTimeout()
	elapsed := time.Since(s.lastUsed)
	if s.calls > 0 || elapsed < timeout {
		// A call is running, or one touched the server after the timer fired: re-arm.
		wait := timeout - elapsed
		if s.calls > 0 {
			wait = timeout
		}
		if s.idleTimer != nil {
			s.idleTimer.Stop()
		}
		s.idleTimer = time.AfterFunc(wait, s.stopIdle)
		s.mu.Unlock()
		return
	}
	taken := s.takeLocked()
	s.mu.Unlock()
	_ = taken.close()
	s.registry.publishStopped(s, "idle")
}

// dropClient forgets failed, a client whose process exited or whose connection died, without
// reporting an idle stop. A client that is no longer current was already taken by whoever replaced it
// — a late failure of a call on a crashed client must not drop the server started after it.
func (s *server) dropClient(failed session) {
	s.mu.Lock()
	if s.current != failed {
		s.mu.Unlock()
		return
	}
	s.current = nil
	s.sem = nil
	s.stopIdleTimerLocked()
	s.mu.Unlock()
	_ = failed.Close()
}

// detach forgets the server's client without closing it, returning it with its server-side sessions
// for the caller to close outside any lock.
func (s *server) detach() release {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeLocked()
}

// stop closes the server, whatever its state, after closing the server-side sessions this runtime
// opened through it.
func (s *server) stop() error {
	s.mu.Lock()
	taken := s.takeLocked()
	s.mu.Unlock()
	return taken.close()
}

// release is a client taken from its server to be closed outside every lock, with the server-side
// sessions it opened and the definition whose binding closes them.
type release struct {
	client session
	cfg    Config
	bound  []string
}

// takeLocked takes the running client and the sessions it opened; the server is stopped from then on.
// The caller holds s.mu.
func (s *server) takeLocked() release {
	taken := release{client: s.current, cfg: s.cfg, bound: s.boundSessions}
	s.current = nil
	s.sem = nil
	s.boundSessions = nil
	s.stopIdleTimerLocked()
	return taken
}

// close asks the server to close the sessions it opened, then closes the client. Every path that stops
// a client goes through it, so no server-side session outlives the client that opened it.
func (r release) close() error {
	if r.client == nil {
		return nil
	}
	closeBoundSessions(r.client, r.cfg, r.bound)
	return r.client.Close()
}

// closeReleases closes taken clients outside any registry lock.
func closeReleases(taken []release) {
	for _, r := range taken {
		_ = r.close()
	}
}

// noteBoundSession remembers the server-side session id a start tool returned, so stop can close it.
func (s *server) noteBoundSession(cfg Config, toolName string, result CallToolResult) {
	binding := cfg.Session
	if binding == nil || binding.StartTool == "" || toolName != binding.StartTool {
		return
	}
	id := sessionIDFromResult(result, binding.SessionIDField)
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.boundSessions {
		if existing == id {
			return
		}
	}
	if len(s.boundSessions) >= maxBoundSessions {
		return
	}
	s.boundSessions = append(s.boundSessions, id)
}

// maxBoundSessions bounds the ids one server remembers; a run that opens more than this is already
// outside what the runtime can clean up, and the oldest entries are dropped first.
const maxBoundSessions = 16

// closeBoundSessions asks the server to close the sessions it opened, with the binding that opened
// them. Each call is bounded and a failure is ignored: the client is going away either way. The stop
// tool is called only when the server's include list exposes it: the runtime calls no tool a run could
// not, so a binding cannot make it run an excluded one.
func closeBoundSessions(client session, cfg Config, ids []string) {
	binding := cfg.Session
	if binding == nil || binding.StopTool == "" || !cfg.included(binding.StopTool) {
		return
	}
	for _, id := range ids {
		ctx, cancel := context.WithTimeout(context.Background(), closeSessionTimeout)
		_, _ = client.CallTool(ctx, binding.StopTool, map[string]any{binding.StopArgument: id})
		cancel()
	}
}

// closeSessionTimeout bounds one stop call made while a server is being closed.
const closeSessionTimeout = 5 * time.Second

// sessionIDFromResult finds the server-side session id in a tool result: the structured content
// first, then the first JSON text content.
func sessionIDFromResult(result CallToolResult, field string) string {
	if field == "" {
		return ""
	}
	if value, ok := result.StructuredContent[field].(string); ok {
		return strings.TrimSpace(value)
	}
	for _, content := range result.Content {
		if content.Type != "text" || content.Text == "" {
			continue
		}
		var decoded map[string]any
		if json.Unmarshal([]byte(content.Text), &decoded) != nil {
			continue
		}
		if value, ok := decoded[field].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func (s *server) setLastError(value string) {
	s.mu.Lock()
	s.lastErr = value
	s.mu.Unlock()
}

func (s *server) clearLastError() {
	s.mu.Lock()
	s.lastErr = ""
	s.mu.Unlock()
}

// recentlyFailed reports a start that failed within the backoff window, so an automatic discovery does
// not retry it on every run while an explicit call still does.
func (s *server) recentlyFailed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastStartFailed && time.Since(s.lastStartAttempt) < startRetryBackoff
}

// startRetryBackoff is how long an automatic catalog discovery waits before trying a server whose start
// failed.
const startRetryBackoff = 2 * time.Minute

// lastError returns the most recent failure, "" when the last operation succeeded.
func (s *server) lastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// missingBinary reports the command a stdio server would need, "" when it resolves.
func (s *server) missingBinary() string {
	cfg, _ := s.config()
	if cfg.transport() != TransportStdio {
		return ""
	}
	if _, err := exec.LookPath(cfg.Command); err != nil {
		return cfg.Command
	}
	return ""
}

// configRequirements lists what a server needs before it can start, without starting it.
func (s *server) configRequirements() ConfigRequirements {
	cfg, scope := s.config()
	requirements := ConfigRequirements{}
	if scope == ScopeProject {
		requirements.NeedsTrust = true
	}
	for _, name := range cfg.MissingEnv() {
		requirements.MissingEnv = append(requirements.MissingEnv, name)
	}
	if binary := s.missingBinary(); binary != "" {
		requirements.MissingBinary = binary
	}
	sort.Strings(requirements.MissingEnv)
	return requirements
}
