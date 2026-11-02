package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

var (
	// ErrNotRunning is a request for a language whose server never started and cannot start now.
	ErrNotRunning = errors.New("lsp server is not running")
	// ErrUnavailable is a server that is configured but cannot serve: its command is missing, it
	// crashed, or it is waiting out its restart backoff.
	ErrUnavailable = errors.New("lsp server is unavailable")
	// ErrServerLimit is a start refused because every running server is busy.
	ErrServerLimit = errors.New("lsp server limit reached and every server is busy")
)

// Manager owns the language servers of one workspace. A server is started by the first request that
// needs its language, reused for later requests, stopped after Options.IdleTimeout unused, and
// restarted after a crash on the next need. At most Options.MaxServers run at once.
//
// A call is bounded end to end by ctx and protocol.ToolDeadline: waiting behind another start of its
// language, the start, and the request. A call whose deadline has already passed starts and sends
// nothing. A start is also bounded by Options.StartTimeout, a request by Options.RequestTimeout. A
// cancelled request sends $/cancelRequest to the server.
type Manager struct {
	workspace string
	order     []string

	mu             sync.Mutex
	closed         bool
	byLanguage     map[string]*server
	logger         *slog.Logger
	idleTimeout    time.Duration
	maxServers     int
	startTimeout   time.Duration
	requestTimeout time.Duration
	backoffBase    time.Duration
	onEvent        func(Event)
	commandPaths   map[string]string
	// detectOnce runs the one bounded walk that decides which languages the workspace holds; a
	// workspace's languages are not re-detected while the manager lives.
	detectOnce    sync.Once
	detectedLangs map[string]bool
	ticker        *time.Ticker
	tickStop      chan struct{}
	tickerPeriod  time.Duration

	watch     sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// server is one language's server and its lifetime state. client, runStarted, state, lastError,
// startedAt, lastUsedAt, busy, failures and nextAttempt are guarded by Manager.mu; startGate (one
// slot) serializes starts so one language spawns one process even when two calls need it at once, and
// a caller waiting for it leaves when its context ends.
type server struct {
	language   string
	def        LanguageServer
	config     Config
	extensions map[string]bool
	startGate  chan struct{}

	client *Client
	// runStarted reports that a run, not the host, started client: a run in an untrusted workspace
	// may use only a server the host started.
	runStarted  bool
	state       string
	lastError   string
	startedAt   time.Time
	lastUsedAt  time.Time
	busy        int
	starting    int
	failures    int
	nextAttempt time.Time
	pid         int
}

// NewManager returns a manager for workspace. Nothing is started: the first request starts the server
// of its language, and Available reports whether any server could serve this workspace at all.
func NewManager(opts Options) (*Manager, error) {
	if strings.TrimSpace(opts.Workspace) == "" {
		return nil, errors.New("lsp workspace is required")
	}
	root, err := filepath.Abs(opts.Workspace)
	if err != nil {
		return nil, err
	}
	servers := opts.Servers
	if len(servers) == 0 {
		servers = DefaultServers()
	}
	m := &Manager{
		workspace:      root,
		byLanguage:     map[string]*server{},
		logger:         opts.Logger,
		idleTimeout:    opts.IdleTimeout,
		maxServers:     opts.MaxServers,
		startTimeout:   opts.StartTimeout,
		requestTimeout: opts.RequestTimeout,
		backoffBase:    opts.BackoffBase,
		onEvent:        opts.OnEvent,
		commandPaths:   map[string]string{},
	}
	if m.logger == nil {
		m.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m.applyDefaults()
	for _, def := range servers {
		def = normalizeServer(def)
		if def.Language == "" || def.Command == "" || len(def.Extensions) == 0 {
			m.logger.Warn("lsp server definition ignored", "workspace", root, "language", def.Language,
				"error_code", "invalid_server")
			continue
		}
		if _, exists := m.byLanguage[def.Language]; exists {
			continue
		}
		extensions := make(map[string]bool, len(def.Extensions))
		for _, ext := range def.Extensions {
			extensions[ext] = true
		}
		m.order = append(m.order, def.Language)
		m.byLanguage[def.Language] = &server{
			language:   def.Language,
			def:        def,
			extensions: extensions,
			config: Config{
				Name:       def.Name,
				Command:    def.Command,
				Args:       def.Args,
				LanguageID: def.Language,
				Workspace:  root,
			},
			state:     StateStopped,
			startGate: make(chan struct{}, 1),
		}
	}
	return m, nil
}

// Configure applies changed tuning to a live manager: the idle timeout, the server limit, the start
// and request timeouts, the restart backoff, the logger and the event hook. The server catalog is
// fixed at NewManager, because replacing it under running servers would leave their processes
// unowned.
func (m *Manager) Configure(opts Options) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	if opts.Logger != nil {
		m.logger = opts.Logger
	}
	if opts.OnEvent != nil {
		m.onEvent = opts.OnEvent
	}
	if opts.IdleTimeout > 0 {
		m.idleTimeout = opts.IdleTimeout
	}
	if opts.MaxServers > 0 {
		m.maxServers = opts.MaxServers
	}
	if opts.StartTimeout > 0 {
		m.startTimeout = opts.StartTimeout
	}
	if opts.RequestTimeout > 0 {
		m.requestTimeout = opts.RequestTimeout
	}
	if opts.BackoffBase > 0 {
		m.backoffBase = opts.BackoffBase
	}
	// The tick period follows the idle timeout; a changed timeout restarts the ticker so it takes
	// effect now, an unchanged one leaves it running.
	if m.ticker != nil && m.tickerPeriod != m.tickPeriodLocked() {
		m.stopTickerLocked()
		m.ensureTickerLocked()
	}
}

func (m *Manager) applyDefaults() {
	if m.idleTimeout <= 0 {
		m.idleTimeout = DefaultIdleTimeout
	}
	if m.maxServers <= 0 {
		m.maxServers = DefaultMaxServers
	}
	if m.startTimeout <= 0 {
		m.startTimeout = DefaultStartTimeout
	}
	if m.requestTimeout <= 0 {
		m.requestTimeout = DefaultRequestTimeout
	}
	if m.backoffBase <= 0 {
		m.backoffBase = DefaultBackoffBase
	}
}

// Available reports whether a configured server could serve this workspace: its command exists and
// the workspace holds a file or a marker of its language. It starts nothing.
func (m *Manager) Available() bool { return len(m.AvailableLanguages()) > 0 }

// AvailableLanguages lists the languages Available covers, in catalog order.
func (m *Manager) AvailableLanguages() []string {
	m.detectOnce.Do(func() {
		m.mu.Lock()
		defs := make([]LanguageServer, 0, len(m.order))
		for _, language := range m.order {
			if srv := m.byLanguage[language]; srv != nil {
				defs = append(defs, srv.def)
			}
		}
		m.mu.Unlock()
		m.detectedLangs = detectLanguages(m.workspace, defs)
	})
	m.mu.Lock()
	order := append([]string(nil), m.order...)
	m.mu.Unlock()
	out := make([]string, 0, len(order))
	for _, language := range order {
		m.mu.Lock()
		srv := m.byLanguage[language]
		m.mu.Unlock()
		if srv == nil || !m.detectedLangs[language] {
			continue
		}
		if m.commandPath(srv.def.Command) == "" {
			continue
		}
		out = append(out, language)
	}
	return out
}

// Servers reports every configured server of the workspace. It starts nothing and keeps no state
// beyond what the manager already knows.
func (m *Manager) Servers() []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(m.order))
	for _, language := range m.order {
		srv := m.byLanguage[language]
		if srv == nil {
			continue
		}
		status := ServerStatus{
			Language:  language,
			Name:      srv.def.Name,
			Command:   srv.def.Command,
			Root:      m.workspace,
			State:     srv.state,
			PID:       srv.pid,
			LastError: srv.lastError,
		}
		if status.State == "" {
			status.State = StateStopped
		}
		if srv.client != nil {
			status.State = StateIdle
			if srv.busy > 0 {
				status.State = StateRunning
			}
			if !srv.startedAt.IsZero() {
				started := srv.startedAt
				status.StartedAt = &started
			}
			if !srv.lastUsedAt.IsZero() {
				used := srv.lastUsedAt
				status.LastUsedAt = &used
			}
		} else if srv.state != StateUnavailable && m.commandPathLocked(srv.def.Command) == "" {
			status.State = StateUnavailable
			status.LastError = "server command not found: " + srv.def.Command
		}
		out = append(out, status)
	}
	return out
}

// Start starts (or restarts) the server of cfg's language now, ignoring a restart backoff: a host
// asking explicitly is not an on-demand call. An empty cfg.LanguageID names the language after
// cfg.Name, else "default".
func (m *Manager) Start(ctx context.Context, cfg Config) (Status, error) {
	if strings.TrimSpace(cfg.Command) == "" {
		return Status{}, errors.New("lsp command is required")
	}
	if cfg.Workspace == "" {
		cfg.Workspace = m.workspace
	}
	language := strings.TrimSpace(cfg.LanguageID)
	if language == "" {
		language = strings.TrimSpace(cfg.Name)
	}
	if language == "" {
		language = "default"
	}
	cfg.LanguageID = language
	srv := m.serverForLanguage(language, cfg)
	// An explicit start restarts: the host may name another command for the same language, and a
	// server started with the previous one must not keep answering.
	m.mu.Lock()
	previous := srv.client
	srv.client = nil
	if previous != nil {
		srv.state = StateStopped
		srv.pid = 0
	}
	m.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	client, err := m.client(ctx, srv, true, byHost)
	if err != nil {
		return Status{}, err
	}
	return client.Status(), nil
}

// Stop stops every server of the workspace and leaves the manager usable: the next request starts a
// server again.
func (m *Manager) Stop() error {
	type stopping struct {
		srv    *server
		client *Client
	}
	m.mu.Lock()
	stops := make([]stopping, 0, len(m.order))
	for _, language := range m.order {
		srv := m.byLanguage[language]
		if srv == nil || srv.client == nil {
			continue
		}
		stops = append(stops, stopping{srv: srv, client: srv.client})
		srv.client = nil
		srv.state = StateStopped
		srv.pid = 0
		srv.lastError = ""
	}
	m.stopTickerLocked()
	m.mu.Unlock()
	var err error
	for _, stop := range stops {
		err = errors.Join(err, stop.client.Close())
		m.logger.Info("lsp server stopped", "language", stop.srv.language, "workspace", m.workspace,
			"error_code", "")
		m.emit(Event{Type: "stopped", Language: stop.srv.language, Workspace: m.workspace})
	}
	return err
}

// Close stops every server (shutdown, exit, then kill) and waits for the processes: no child of a
// closed manager is left running.
func (m *Manager) Close() error {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.closeErr = m.Stop()
		m.watch.Wait()
	})
	return m.closeErr
}

// The request methods of a Manager are the host's: a host API asked for them, so they start the server
// a request needs. A run reaches the servers through ForRun.

func (m *Manager) Diagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	return m.host().Diagnostics(ctx, path)
}

func (m *Manager) Hover(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	return m.host().Hover(ctx, path, line, character)
}

func (m *Manager) Definition(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	return m.host().Definition(ctx, path, line, character)
}

func (m *Manager) References(ctx context.Context, path string, line, character int, includeDeclaration bool) (json.RawMessage, error) {
	return m.host().References(ctx, path, line, character, includeDeclaration)
}

func (m *Manager) DocumentSymbols(ctx context.Context, path string) (json.RawMessage, error) {
	return m.host().DocumentSymbols(ctx, path)
}

func (m *Manager) WorkspaceSymbols(ctx context.Context, query string) (json.RawMessage, error) {
	return m.host().WorkspaceSymbols(ctx, query)
}

func (m *Manager) Rename(ctx context.Context, path string, line, character int, newName string) (json.RawMessage, error) {
	return m.host().Rename(ctx, path, line, character, newName)
}

// origin is who a request is made for, which decides whether it may start the server it needs.
type origin int

const (
	// byHost: a host API. It starts what it needs; the server it starts is the host's.
	byHost origin = iota
	// byRun: a run in a trusted workspace. It starts what it needs; the server it starts is a run's.
	byRun
	// attachOnly: a run in an untrusted workspace (ADR-0013). It starts nothing — a language server
	// loads code from the repository it serves — and uses only a server the host started.
	attachOnly
)

// Access is the manager as one caller reaches it: the request methods, with the start rule of the
// caller's origin.
type Access struct {
	m      *Manager
	origin origin
}

func (m *Manager) host() *Access { return &Access{m: m, origin: byHost} }

// ForRun is the manager as an agent run reaches it. A run in a trusted workspace starts the server a
// request needs, as a host does. A run in an untrusted workspace never starts one and uses only a server
// the host started (LSPStart, or a host request), so repository code runs in a language server only by
// the host's action or the workspace's trust (ADR-0013).
func (m *Manager) ForRun(trusted bool) *Access {
	if trusted {
		return &Access{m: m, origin: byRun}
	}
	return &Access{m: m, origin: attachOnly}
}

// Available reports whether this caller is offered the language-server tools: for a caller that may
// start a server, Manager.Available; for one that may not, whether a server the host started is
// running. It starts nothing.
func (a *Access) Available() bool {
	if a.origin != attachOnly {
		return a.m.Available()
	}
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	for _, language := range a.m.order {
		if srv := a.m.byLanguage[language]; srv != nil && srv.client != nil && !srv.runStarted {
			return true
		}
	}
	return false
}

func (a *Access) Diagnostics(ctx context.Context, path string) ([]Diagnostic, error) {
	var out []Diagnostic
	err := a.m.call(ctx, path, "textDocument/diagnostic", a.origin, func(ctx context.Context, client *Client) error {
		items, err := client.Diagnostics(ctx, path)
		out = items
		return err
	})
	return out, err
}

func (a *Access) Hover(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	return a.raw(ctx, path, "textDocument/hover", func(ctx context.Context, client *Client) (json.RawMessage, error) {
		return client.Hover(ctx, path, line, character)
	})
}

func (a *Access) Definition(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	return a.raw(ctx, path, "textDocument/definition", func(ctx context.Context, client *Client) (json.RawMessage, error) {
		return client.Definition(ctx, path, line, character)
	})
}

func (a *Access) References(ctx context.Context, path string, line, character int, includeDeclaration bool) (json.RawMessage, error) {
	return a.raw(ctx, path, "textDocument/references", func(ctx context.Context, client *Client) (json.RawMessage, error) {
		return client.References(ctx, path, line, character, includeDeclaration)
	})
}

func (a *Access) DocumentSymbols(ctx context.Context, path string) (json.RawMessage, error) {
	return a.raw(ctx, path, "textDocument/documentSymbol", func(ctx context.Context, client *Client) (json.RawMessage, error) {
		return client.DocumentSymbols(ctx, path)
	})
}

func (a *Access) WorkspaceSymbols(ctx context.Context, query string) (json.RawMessage, error) {
	return a.raw(ctx, "", "workspace/symbol", func(ctx context.Context, client *Client) (json.RawMessage, error) {
		return client.WorkspaceSymbols(ctx, query)
	})
}

func (a *Access) Rename(ctx context.Context, path string, line, character int, newName string) (json.RawMessage, error) {
	return a.raw(ctx, path, "textDocument/rename", func(ctx context.Context, client *Client) (json.RawMessage, error) {
		return client.Rename(ctx, path, line, character, newName)
	})
}

func (a *Access) raw(ctx context.Context, path, method string, fn func(context.Context, *Client) (json.RawMessage, error)) (json.RawMessage, error) {
	var out json.RawMessage
	err := a.m.call(ctx, path, method, a.origin, func(ctx context.Context, client *Client) error {
		raw, err := fn(ctx, client)
		out = raw
		return err
	})
	return out, err
}

// call runs one request against the server of path's language, starting the server when needed and
// the origin allows it. The run's tool deadline and ctx bound the whole call, from the wait behind
// another start to the answer; Options.RequestTimeout bounds the request.
func (m *Manager) call(ctx context.Context, path, method string, from origin, fn func(context.Context, *Client) error) error {
	srv := m.serverForPath(path)
	if srv == nil {
		return fmt.Errorf("%w: no language server is configured for %q", ErrUnavailable, filepath.Ext(path))
	}
	ctx, cancelCall := callContext(ctx)
	defer cancelCall()
	if err := ctx.Err(); err != nil {
		return err
	}
	// The server is marked busy before it is looked up, so an idle reclaim or an eviction cannot take
	// it away between the lookup and the call.
	m.begin(srv)
	defer m.end(srv)
	client, err := m.client(ctx, srv, false, from)
	if err != nil {
		return err
	}

	requestCtx, cancel := m.requestContext(ctx)
	defer cancel()
	started := time.Now()
	if err := fn(requestCtx, client); err != nil {
		// A request whose server died under it is an unavailable server, not a protocol error the
		// caller has to interpret. The watcher that marks the server unavailable may not have run
		// yet, so the client's own end signal decides.
		dead := false
		select {
		case <-client.Done():
			dead = true
		default:
		}
		if dead || m.runningClient(srv) != client {
			err = fmt.Errorf("%w: %s server stopped: %w", ErrUnavailable, srv.language, err)
		}
		m.logger.Warn("lsp request failed",
			"language", srv.language, "workspace", m.workspace, "method", method,
			"duration_ms", time.Since(started).Milliseconds(), "error_code", errorCode(err))
		return err
	}
	return nil
}

// client returns the running client of srv, starting the server when it is not running and the origin
// may start one. force skips the restart backoff (an explicit Start).
func (m *Manager) client(ctx context.Context, srv *server, force bool, from origin) (*Client, error) {
	if from == attachOnly {
		m.mu.Lock()
		client, byRunStart := srv.client, srv.runStarted
		m.mu.Unlock()
		if client == nil || byRunStart {
			return nil, fmt.Errorf("%w: no %s server the host started is running, and a run in an untrusted workspace starts none",
				ErrNotRunning, srv.language)
		}
		return client, nil
	}
	if client := m.runningClient(srv); client != nil {
		return client, nil
	}
	// One start per language at a time. A caller queued behind another start leaves when its context
	// ends instead of waiting that start out.
	select {
	case srv.startGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-srv.startGate }()
	// The gate and ctx may have been ready together: a caller whose context ended evicts and starts
	// nothing.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if client := m.runningClient(srv); client != nil {
		return client, nil
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, ErrClosed
	}
	if !force && time.Now().Before(srv.nextAttempt) {
		wait := time.Until(srv.nextAttempt).Round(time.Millisecond)
		lastError := srv.lastError
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s server restarts in %s after: %s", ErrUnavailable, srv.language, wait, lastError)
	}
	// A start in flight holds a slot: two languages starting at once must not exceed the limit.
	srv.starting++
	victim, victimClient, err := m.reserveLocked(srv)
	startTimeout := m.startTimeout
	if err != nil {
		srv.starting--
	}
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	defer func() {
		m.mu.Lock()
		srv.starting--
		m.mu.Unlock()
	}()
	if victimClient != nil {
		_ = victimClient.Close()
		m.logger.Info("lsp server evicted for the server limit", "language", victim.language,
			"workspace", m.workspace, "error_code", "server_limit")
		m.emit(Event{Type: "evicted", Language: victim.language, Workspace: m.workspace, ErrorCode: "server_limit"})
	}
	cfg := srv.config
	cfg.Workspace = m.workspace
	cfg.StartTimeout = startTimeout
	startCtx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	// Start spawns the process before it looks at ctx, and closing an evicted server takes time.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	started := time.Now()
	m.logger.Info("lsp server starting", "language", srv.language, "workspace", m.workspace,
		"command", cfg.Command, "error_code", "")
	m.emit(Event{Type: "started", Language: srv.language, Workspace: m.workspace})
	client, err := Start(startCtx, cfg)
	if err != nil {
		m.markFailed(srv, err)
		return nil, fmt.Errorf("%w: %s: %w", ErrUnavailable, srv.language, err)
	}
	if err := m.install(srv, client, started, from == byRun); err != nil {
		_ = client.Close()
		return nil, err
	}
	return client, nil
}

// reserveLocked makes room for one more server: it detaches the least recently used idle server when
// the limit is reached. The caller closes the detached client outside the lock. srv's own slot is
// already counted by its starting counter, so it is not counted against the limit here.
func (m *Manager) reserveLocked(srv *server) (*server, *Client, error) {
	if m.liveLocked()-1 < m.maxServers {
		return nil, nil, nil
	}
	var victim *server
	for _, language := range m.order {
		candidate := m.byLanguage[language]
		if candidate == nil || candidate == srv || candidate.client == nil ||
			candidate.busy > 0 || candidate.starting > 0 {
			continue
		}
		if victim == nil || candidate.lastUsedAt.Before(victim.lastUsedAt) {
			victim = candidate
		}
	}
	if victim == nil {
		return nil, nil, ErrServerLimit
	}
	client := victim.client
	victim.client = nil
	victim.state = StateStopped
	victim.pid = 0
	return victim, client, nil
}

// install records a started client and whether a run started it. A manager closed while the start ran
// refuses it, so the process does not outlive Close.
func (m *Manager) install(srv *server, client *Client, started time.Time, byRun bool) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	srv.client = client
	srv.runStarted = byRun
	srv.state = StateIdle
	srv.startedAt = started
	srv.lastUsedAt = started
	srv.lastError = ""
	srv.failures = 0
	srv.nextAttempt = time.Time{}
	srv.pid = client.Status().PID
	m.ensureTickerLocked()
	m.mu.Unlock()

	m.watch.Add(1)
	go func() {
		defer m.watch.Done()
		<-client.Done()
		m.clientExited(srv, client)
	}()
	m.logger.Info("lsp server ready", "language", srv.language, "workspace", m.workspace,
		"pid", client.Status().PID, "duration_ms", time.Since(started).Milliseconds(), "error_code", "")
	m.emit(Event{Type: "ready", Language: srv.language, Workspace: m.workspace, PID: client.Status().PID,
		Duration: time.Since(started)})
	return nil
}

// clientExited marks a server that ended on its own unavailable and schedules its restart. A server
// the manager detached itself (idle stop, eviction, Stop, Close) is no longer srv.client, so a
// deliberate stop is not reported as a crash.
func (m *Manager) clientExited(srv *server, client *Client) {
	m.mu.Lock()
	if m.closed || srv.client != client {
		m.mu.Unlock()
		return
	}
	lastError := client.LastError()
	if strings.TrimSpace(lastError) == "" {
		lastError = "language server exited"
	}
	srv.client = nil
	srv.pid = 0
	srv.state = StateUnavailable
	srv.lastError = lastError
	srv.failures++
	srv.nextAttempt = time.Now().Add(m.backoff(srv.failures))
	backoff := time.Until(srv.nextAttempt).Round(time.Millisecond)
	if m.liveLocked() == 0 {
		m.stopTickerLocked()
	}
	m.mu.Unlock()

	m.logger.Warn("lsp server crashed", "language", srv.language, "workspace", m.workspace,
		"restart_in_ms", backoff.Milliseconds(), "error_code", "crashed")
	m.emit(Event{Type: "crashed", Language: srv.language, Workspace: m.workspace, ErrorCode: "crashed",
		Message: lastError})
	// The process is gone; closing the client reaps it (cmd.Wait) so no zombie is left behind. The
	// shutdown/exit handshake fails immediately on the dead pipes.
	_ = client.Close()
}

// markFailed records a start that did not reach initialize: the language is unavailable until its
// backoff ends, and the next need tries again.
func (m *Manager) markFailed(srv *server, err error) {
	m.mu.Lock()
	srv.state = StateUnavailable
	srv.pid = 0
	srv.lastError = err.Error()
	srv.failures++
	srv.nextAttempt = time.Now().Add(m.backoff(srv.failures))
	backoff := time.Until(srv.nextAttempt).Round(time.Millisecond)
	m.mu.Unlock()

	m.logger.Warn("lsp server failed to start", "language", srv.language, "workspace", m.workspace,
		"restart_in_ms", backoff.Milliseconds(), "error_code", "start_failed")
	m.emit(Event{Type: "unavailable", Language: srv.language, Workspace: m.workspace, ErrorCode: "start_failed",
		Message: err.Error()})
}

func (m *Manager) backoff(failures int) time.Duration {
	base := m.backoffBase
	if base <= 0 {
		base = DefaultBackoffBase
	}
	delay := base
	for i := 1; i < failures && delay < maxBackoff; i++ {
		delay *= 2
	}
	if delay > maxBackoff {
		delay = maxBackoff
	}
	return delay
}

func (m *Manager) runningClient(srv *server) *Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	return srv.client
}

func (m *Manager) liveLocked() int {
	live := 0
	for _, language := range m.order {
		if srv := m.byLanguage[language]; srv != nil && (srv.client != nil || srv.starting > 0) {
			live++
		}
	}
	return live
}

func (m *Manager) begin(srv *server) {
	m.mu.Lock()
	srv.busy++
	srv.lastUsedAt = time.Now()
	if srv.client != nil {
		srv.state = StateRunning
	}
	m.mu.Unlock()
}

func (m *Manager) end(srv *server) {
	m.mu.Lock()
	if srv.busy > 0 {
		srv.busy--
	}
	srv.lastUsedAt = time.Now()
	if srv.client != nil && srv.busy == 0 {
		srv.state = StateIdle
	}
	m.mu.Unlock()
}

// callContext bounds a whole call by the run's tool deadline. A deadline already past leaves the
// context done: it never becomes an unbounded wait.
func callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := protocol.ToolDeadline(ctx); ok {
		return context.WithDeadline(ctx, deadline)
	}
	return ctx, func() {}
}

// requestContext bounds one request by Options.RequestTimeout; ctx already carries the call's tool
// deadline (callContext).
func (m *Manager) requestContext(ctx context.Context) (context.Context, context.CancelFunc) {
	m.mu.Lock()
	timeout := m.requestTimeout
	m.mu.Unlock()
	if timeout <= 0 {
		timeout = DefaultRequestTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

// ensureTickerLocked starts the one manager-level idle check. It runs only while a server is running.
func (m *Manager) ensureTickerLocked() {
	if m.ticker != nil || m.closed {
		return
	}
	period := m.tickPeriodLocked()
	stop := make(chan struct{})
	m.ticker = time.NewTicker(period)
	m.tickerPeriod = period
	m.tickStop = stop
	go m.tickLoop(m.ticker, stop)
}

// tickPeriodLocked is how often the idle check runs: a quarter of the idle timeout, bounded so a very
// short or a very long timeout still costs one ticker at a sane rate.
func (m *Manager) tickPeriodLocked() time.Duration {
	period := m.idleTimeout / 4
	if period < minIdleTick {
		period = minIdleTick
	}
	if period > maxIdleTick {
		period = maxIdleTick
	}
	return period
}

func (m *Manager) stopTickerLocked() {
	if m.ticker == nil {
		return
	}
	m.ticker.Stop()
	close(m.tickStop)
	m.ticker = nil
	m.tickStop = nil
	m.tickerPeriod = 0
}

func (m *Manager) tickLoop(ticker *time.Ticker, stop chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			m.reclaimIdle()
		}
	}
}

// reclaimIdle stops the servers that have been unused for IdleTimeout. A busy server is never
// stopped.
func (m *Manager) reclaimIdle() {
	type idle struct {
		srv    *server
		client *Client
		unused time.Duration
	}
	now := time.Now()
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	var stops []idle
	for _, language := range m.order {
		srv := m.byLanguage[language]
		if srv == nil || srv.client == nil || srv.busy > 0 {
			continue
		}
		unused := now.Sub(srv.lastUsedAt)
		if unused < m.idleTimeout {
			continue
		}
		stops = append(stops, idle{srv: srv, client: srv.client, unused: unused})
		srv.client = nil
		srv.state = StateStopped
		srv.pid = 0
		srv.lastError = ""
	}
	if len(stops) > 0 && m.liveLocked() == 0 {
		m.stopTickerLocked()
	}
	m.mu.Unlock()

	for _, stop := range stops {
		_ = stop.client.Close()
		m.logger.Info("lsp server stopped after idle", "language", stop.srv.language,
			"workspace", m.workspace, "duration_ms", stop.unused.Milliseconds(), "error_code", "")
		m.emit(Event{Type: "idle_stop", Language: stop.srv.language, Workspace: m.workspace,
			Duration: stop.unused})
	}
}

func (m *Manager) emit(event Event) {
	m.mu.Lock()
	handler := m.onEvent
	m.mu.Unlock()
	if handler != nil {
		handler(event)
	}
}

func (m *Manager) serverForPath(path string) *server {
	ext := strings.ToLower(filepath.Ext(strings.TrimSpace(path)))
	if ext == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, language := range m.order {
		srv := m.byLanguage[language]
		if srv != nil && srv.extensions[ext] {
			return srv
		}
	}
	return nil
}

// serverForLanguage returns the server of language, adding one for an explicit start the catalog does
// not name.
func (m *Manager) serverForLanguage(language string, cfg Config) *server {
	language = strings.ToLower(strings.TrimSpace(language))
	m.mu.Lock()
	defer m.mu.Unlock()
	if srv := m.byLanguage[language]; srv != nil {
		srv.config = cfg
		return srv
	}
	srv := &server{
		language:   language,
		def:        LanguageServer{Language: language, Name: cfg.Name, Command: cfg.Command, Args: cfg.Args},
		config:     cfg,
		extensions: map[string]bool{},
		state:      StateStopped,
		startGate:  make(chan struct{}, 1),
	}
	m.order = append(m.order, language)
	m.byLanguage[language] = srv
	return srv
}

// commandPath resolves a server command once and caches the answer: the catalog is fixed, and a
// binary installed later is found when the Engine reopens the workspace.
func (m *Manager) commandPath(command string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.commandPathLocked(command)
}

func (m *Manager) commandPathLocked(command string) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	if path, ok := m.commandPaths[command]; ok {
		return path
	}
	path := resolveCommand(command)
	if path != "" {
		// A hit is cached. A miss is looked up again: a server installed while the Engine is open is
		// then found by the next availability check, at the cost of one PATH scan.
		m.commandPaths[command] = path
	}
	return path
}

func errorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, ErrServerLimit):
		return "busy"
	case errors.Is(err, ErrUnavailable), errors.Is(err, ErrNotRunning):
		return "unavailable"
	case errors.Is(err, ErrClosed):
		return "closed"
	}
	return "failed"
}
