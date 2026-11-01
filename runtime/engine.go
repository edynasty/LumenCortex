package runtime

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/diag"
	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/internal/mcp"
	"github.com/edynasty/LumenCortex/internal/migrate"
	"github.com/edynasty/LumenCortex/internal/ompsession"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/resource"
	"github.com/edynasty/LumenCortex/internal/runlock"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/skills"
	"github.com/edynasty/LumenCortex/internal/streambuf"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/pluginapi"
	"github.com/edynasty/LumenCortex/protocol"
)

const GoRuntimePreviewVersion = "0.1.4-preview"

// ErrRunHeld reports that another Engine, in this process or another, holds the
// session's run lock. Nothing was written.
var ErrRunHeld = errors.New("run is active in another process or engine")

// ErrSchemaTooNew reports that the workspace database was migrated by a newer binary. Open wrote
// nothing; upgrade the binary, or run `db downgrade` with the newer one.
var ErrSchemaTooNew = migrate.ErrSchemaTooNew

// ErrLegacyNodeDatabase reports that the workspace database was created by the legacy Node runtime.
// Node databases are not supported or migrated; Open wrote nothing. Use a new state directory.
var ErrLegacyNodeDatabase = migrate.ErrLegacyNodeDatabase

// ErrDatabaseInUse reports that a schema change could not take the exclusive schema lock because
// other processes kept the workspace open for the 30 s wait. Close them and retry.
var ErrDatabaseInUse = migrate.ErrDatabaseInUse

// DowngradeSchema returns the workspace database to schema version `to` with the numbered down
// migrations, newest first, in one transaction, after the same exclusive schema lock and scrubbed
// backup as an upgrade. The workspace must be closed everywhere: otherwise it fails with
// ErrDatabaseInUse after 30 s. A version without a down migration is refused with the database
// unchanged. It returns the version the database had and the one it has now.
func DowngradeSchema(ctx context.Context, opts Options, to int) (from, now int, err error) {
	_, loc, err := locateState(ctx, opts)
	if err != nil {
		return 0, 0, err
	}
	set, err := migrate.Embedded()
	if err != nil {
		return 0, 0, err
	}
	return migrate.Downgrade(ctx, loc.Path, set, to)
}

// Budget bounds one Engine's resources. MaxAgents > 0 is a host override: it replaces the
// configuration's concurrency.maxAgents for this Engine's lifetime (tests, embedded hosts).
type Budget struct {
	SoftBytes int64 `json:"softBytes"`
	HardBytes int64 `json:"hardBytes"`
	MaxAgents int   `json:"maxAgents"`
}

type Options struct {
	Workspace string
	// RepoDir overrides the state directory (tests and self-checks). Leave it empty: Core decides
	// where a workspace's state lives (ADR-0016). The binding check applies to it all the same.
	RepoDir string
	Budget  Budget
	// Plugins are host-supplied in-process extensions, registered after the built-in ones and started
	// with them (ADR-0012). A plugin that fails to start is marked failed; Open still succeeds.
	Plugins []pluginapi.Plugin
	// ProviderFactory builds the client of a catalog model before a run starts. nil uses the
	// OpenAI-compatible adapter (openai.FromModel).
	ProviderFactory ProviderFactory
}

type Engine struct {
	workspace    string
	repoDir      string
	state        StateInfo
	protected    toolset.Protected
	worktreeRoot string
	store        *session.Store
	resources    *resource.Manager
	repo         *repository.Service
	events       *eventBus
	plugins      *PluginRegistry
	hooks        *hookDispatcher
	shell        *shell.Runner
	lspMu        sync.Mutex
	// noteMu makes the user-note limit check and the queueing of the add one step in this Engine.
	noteMu        sync.Mutex
	lspManagers   map[string]*lsp.Manager
	mcpRegistry   *mcp.Registry
	skillRegistry *skills.Registry
	// health is the model health registry: one per Engine, advisory, never persisted.
	health          *agent.Health
	providerFactory ProviderFactory
	// fixedMaxAgents is set when the host passed Budget.MaxAgents: the configuration's
	// concurrency.maxAgents is then not applied.
	fixedMaxAgents bool
	// runs are the loops executing in this Engine now, by session; a revocation cancels the ones it
	// fences and waits for them (revoke.go). runsMu guards it.
	runsMu sync.Mutex
	runs   map[string]*trackedRun
	// log is the diagnostics logger every runtime component writes to; it is never nil, a discard
	// logger stands in when the diagnostics log could not be opened (diagnostics.go).
	log *slog.Logger
	// diag is the diagnostics log; nil when it could not be opened, and diagErr says why.
	diag    *diag.Log
	diagErr error
	// bridge is the event-to-log goroutine's state; nil when the log is unavailable.
	bridge *diagBridge
}

type Health struct {
	Version      string `json:"version"`
	Workspace    string `json:"workspace"`
	Database     string `json:"database"`
	Budget       Budget `json:"budget"`
	UsedBytes    int64  `json:"usedBytes"`
	ActiveAgents int    `json:"activeAgents"`
	Pressure     bool   `json:"pressure"`
}

type SessionInfo struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Status    string    `json:"status"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	// ModelRef is the catalog reference ("<provider>/<model>") the session is pinned to, empty
	// before the first Start of a session created by an older build. Provider and Model are
	// the provider id and upstream model id of the last Start, for display.
	ModelRef string `json:"modelRef,omitempty"`
	// Agent is the catalog agent the session is pinned to (ADR-0009), empty before its first Start.
	Agent    string          `json:"agent,omitempty"`
	Goal     string          `json:"goal"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
	Final    *string         `json:"final,omitempty"`
	Usage    json.RawMessage `json:"usage,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}

type Message struct {
	SessionID string          `json:"sessionId"`
	Seq       int64           `json:"seq"`
	Role      string          `json:"role"`
	JSON      json.RawMessage `json:"json"`
	// Withheld marks a row whose content no run of the session sends again: it carries content of a
	// revoked source, or the whole session is withdrawn or cannot be projected (DATA_AND_FAILURES §5).
	// The host still reads the row; privacy deletion is what removes it.
	Withheld bool `json:"withheld,omitempty"`
}

type StreamSnapshot struct {
	Head      []byte `json:"head"`
	Tail      []byte `json:"tail"`
	Total     int64  `json:"total"`
	Truncated bool   `json:"truncated"`
}

type ShellResult struct {
	Command   string         `json:"command"`
	ExitCode  int            `json:"exitCode"`
	StartedAt time.Time      `json:"startedAt"`
	Duration  time.Duration  `json:"duration"`
	Stdout    StreamSnapshot `json:"stdout"`
	Stderr    StreamSnapshot `json:"stderr"`
	Cancelled bool           `json:"cancelled"`
}

type SessionOptions struct {
	Goal string `json:"goal"`
	// Input is the session's first transcript row, written in the transaction that
	// creates the session. When empty the goal is the first row.
	Input    string         `json:"input,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type SessionHandle struct {
	engine *Engine
	ID     string `json:"id"`
}

// Open opens the workspace engine. It runs any pending schema migration and holds the schema lock
// shared until Close; see OpenContext.
func Open(opts Options) (*Engine, error) {
	return OpenContext(context.Background(), opts)
}

// OpenContext opens the workspace engine. Core decides where its state lives (ADR-0016): the
// external state of the workspace, the legacy <workspace>/.lumencortex database opened in place
// when that is all there is, or Options.RepoDir. The state is bound to the canonical workspace path;
// ErrStateBindingMismatch refuses a database bound to another workspace and
// ErrRepositoryCarriedState one that Git tracks, in both cases without opening a new empty state.
// Every Engine holds <state>/db.lock shared from Open to Close, and an older schema is migrated
// first under the exclusive lock (backup, scrub, checkpoint). ctx bounds the wait for that lock (at
// most 30 s) and cancels a migration in progress. It returns ErrSchemaTooNew, writing nothing, for a
// database a newer binary migrated, and ErrDatabaseInUse when other processes keep the workspace
// open for the whole wait.
func OpenContext(ctx context.Context, opts Options) (*Engine, error) {
	workspace, loc, err := locateState(ctx, opts)
	if err != nil {
		return nil, err
	}
	repoDir := loc.Path
	worktreeRoot := filepath.Join(filepath.Dir(workspace), "."+filepath.Base(workspace)+".lumencortex-worktrees")
	if loc.Mode == StateExternal {
		if err := os.MkdirAll(repoDir, 0o700); err != nil {
			return nil, fmt.Errorf("create the state directory: %w", err)
		}
	}
	store, err := session.OpenBound(ctx, repoDir, session.Binding{CanonicalPath: loc.Workspace, CoreVersion: GoRuntimePreviewVersion})
	if err != nil {
		return nil, err
	}
	// The diagnostics log lives beside the database. A log that cannot be opened must not fail the
	// Engine: it falls back to a discard logger and DiagnosticStats reports the reason.
	diagLog, diagErr := openDiagnostics(repoDir, workspace)
	var engine *Engine
	defer func() {
		if engine == nil && diagLog != nil {
			_ = diagLog.Close()
		}
	}()
	// A run that writes the workspace's check definition file revokes the project's check approval in
	// the transaction that records the write (ADR-0017). The global file lies outside every workspace a
	// run's snapshots cover; a change to it still changes the set digest.
	store.WatchCheckDefinitions(loc.Workspace, config.FileName)
	repoService, err := repository.New(workspace)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	globalMCPPath := ""
	skillGlobalDir := ""
	pluginStatePath := ""
	// F9: the one global directory, the same on every OS. Files in directories earlier builds
	// used are reported by config.Load and are not read.
	if globalDir, dirErr := config.GlobalDir(); dirErr == nil {
		globalMCPPath = filepath.Join(globalDir, config.MCPFileName)
		skillGlobalDir = globalDir
		pluginStatePath = filepath.Join(globalDir, PluginStateFileName)
	}
	mcpRegistry, err := mcp.NewLayeredRegistry(
		workspace,
		globalMCPPath,
		filepath.Join(workspace, legacyDirName, "mcp.json"),
	)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	// ADR-0013: the project mcp.json is a project input. The check runs on every start and every
	// tool listing, against the Engine workspace and never a session worktree, so withdrawing trust
	// blocks a project server without reopening the workspace. A configuration that does not load is
	// not trusted.
	mcpRegistry.SetProjectTrust(func() bool {
		cfg, err := config.Load(workspace)
		if err != nil {
			return false
		}
		return cfg.Effective().Trusted
	})
	// Skills come from LumenCortex's own directories and, read in place, from the cross-agent ones
	// other tools install into (~/.agents/skills, ~/.claude/skills and the workspace's).
	home, _ := os.UserHomeDir()
	globalSkills, projectSkills := skills.DefaultRoots(skillGlobalDir, home, workspace)
	skillRegistry, err := skills.NewRegistry(globalSkills, projectSkills)
	if err != nil {
		_ = mcpRegistry.Close()
		_ = store.Close()
		return nil, err
	}
	if gitRoot, rootErr := repoService.GitRoot(context.Background()); rootErr == nil {
		worktreeRoot = filepath.Join(filepath.Dir(gitRoot), "."+filepath.Base(gitRoot)+".lumencortex-worktrees")
		_ = repoService.EnsureLocalExclude(context.Background(), repoDir)
	}
	factory := opts.ProviderFactory
	if factory == nil {
		factory = defaultProviderFactory
	}
	engine = &Engine{
		workspace:    workspace,
		repoDir:      repoDir,
		state:        loc.StateInfo,
		protected:    loc.protected(),
		worktreeRoot: worktreeRoot,
		store:        store,
		resources: resource.New(resource.Budget{
			SoftBytes: opts.Budget.SoftBytes,
			HardBytes: opts.Budget.HardBytes,
			MaxAgents: cmp.Or(opts.Budget.MaxAgents, config.DefaultMaxAgents),
		}),
		fixedMaxAgents:  opts.Budget.MaxAgents > 0,
		repo:            repoService,
		events:          newEventBus(),
		shell:           shell.New(workspace),
		lspManagers:     make(map[string]*lsp.Manager),
		mcpRegistry:     mcpRegistry,
		skillRegistry:   skillRegistry,
		health:          agent.NewHealth(nil),
		providerFactory: factory,
		log:             discardLogger(),
		diag:            diagLog,
		diagErr:         diagErr,
	}
	if diagLog != nil {
		engine.log = diagLog.Logger()
	}
	engine.bridge = engine.startDiagnosticsBridge()
	engine.hooks = newHookDispatcher(engine.events)
	engine.plugins = newPluginRegistry(engine.events, engine.hooks, pluginStatePath)
	if err := engine.plugins.Register(ompsession.Plugin{}, true); err != nil {
		_ = engine.Close()
		return nil, err
	}
	for _, plugin := range opts.Plugins {
		if err := engine.plugins.Register(plugin, false); err != nil {
			_ = engine.Close()
			return nil, err
		}
	}
	engine.plugins.StartAll(ctx)
	if err := engine.reconcileRunning(ctx); err != nil {
		_ = engine.Close()
		return nil, err
	}
	engine.resumeDeletions(ctx)
	return engine, nil
}

// runDir is where the per-session run locks live, beside the database.
func (e *Engine) runDir() string { return filepath.Join(e.repoDir, "runs") }

// acquireRun takes the session's run lock without blocking. It returns
// session.ErrNotFound for an unknown session and ErrRunHeld when the lock is held.
func (e *Engine) acquireRun(ctx context.Context, sessionID string) (*runlock.Lock, error) {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	lock, err := runlock.TryAcquire(e.runDir(), sessionID)
	if errors.Is(err, runlock.ErrHeld) {
		return nil, ErrRunHeld
	}
	return lock, err
}

// reconcileRunning reconciles every session whose durable status is "running"
// and whose run lock is free: nobody is running it, so its status and tail gap
// are residue of a dead process. A held lock means the run is live elsewhere and
// the session is left alone.
func (e *Engine) reconcileRunning(ctx context.Context) error {
	ids, err := e.store.RunningSessionIDs(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		lock, err := runlock.TryAcquire(e.runDir(), id)
		if errors.Is(err, runlock.ErrHeld) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = e.store.Reconcile(ctx, id)
		err = errors.Join(err, lock.Release())
		if err != nil {
			return fmt.Errorf("reconcile session %s: %w", id, err)
		}
	}
	return nil
}

func (e *Engine) Close() error {
	var closeErr error
	if e.plugins != nil {
		closeErr = errors.Join(closeErr, e.plugins.StopAll(context.Background()))
	}
	e.events.close()
	// The bus is closed: the bridge drains what it was delivered and returns, so no event record is
	// lost to Close and no goroutine outlives the Engine.
	if e.bridge != nil {
		e.bridge.stop()
		<-e.bridge.done
	}
	e.lspMu.Lock()
	managers := make([]*lsp.Manager, 0, len(e.lspManagers))
	for _, manager := range e.lspManagers {
		managers = append(managers, manager)
	}
	e.lspManagers = make(map[string]*lsp.Manager)
	e.lspMu.Unlock()
	for _, manager := range managers {
		_ = manager.Close()
	}
	if e.mcpRegistry != nil {
		_ = e.mcpRegistry.Close()
	}
	// The diagnostics log is closed last, after the store: everything that can log during Close has
	// logged by then, and the writer flushes what is left.
	return errors.Join(closeErr, e.store.Close(), e.closeDiagnostics())
}

// Plugins lists the registered extensions with their enablement, state and failures (ADR-0012).
func (e *Engine) Plugins() PluginCatalog { return e.plugins.Catalog() }

// SetPluginEnabled records a plugin's enablement for every workspace and applies it to this Engine
// now. Other open Engines apply it when they next open.
func (e *Engine) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	return e.plugins.SetEnabled(ctx, id, enabled)
}

// Health reports the Engine's state. The agent limit is the configured one when the configuration
// loads, else the limit the last run start applied.
func (e *Engine) Health() Health {
	_, _ = e.concurrency()
	budget := e.resources.Budget()
	return Health{
		Version:   GoRuntimePreviewVersion,
		Workspace: e.workspace,
		Database:  e.store.Path(),
		Budget: Budget{
			SoftBytes: budget.SoftBytes,
			HardBytes: budget.HardBytes,
			MaxAgents: budget.MaxAgents,
		},
		UsedBytes:    e.resources.UsedBytes(),
		ActiveAgents: e.resources.ActiveAgents(),
		Pressure:     e.resources.UnderPressure(),
	}
}

// concurrency reads the concurrency limits in force now and applies the process limit to the
// resource manager, so a saved change takes effect at the next run start without reopening the
// Engine. A host-fixed Budget.MaxAgents wins over the configuration.
func (e *Engine) concurrency() (config.Concurrency, error) {
	cfg, err := config.Load(e.workspace)
	if err != nil {
		return config.Concurrency{}, err
	}
	limits := cfg.Effective().Concurrency
	if e.fixedMaxAgents {
		limits.MaxAgents = e.resources.MaxAgents()
	} else {
		e.resources.SetMaxAgents(limits.MaxAgents)
	}
	return limits, nil
}

func (e *Engine) Events(buffer int) (<-chan Event, func()) { return e.events.subscribe(buffer) }

// EventStats returns the event bus counters (see EventStats). Publishing never
// waits for a subscriber; a subscriber that cannot keep up loses its oldest events
// and Dropped counts each loss.
func (e *Engine) EventStats() EventStats { return e.events.stats() }

func newSessionID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "session_" + hex.EncodeToString(buf), nil
}

// NewSession creates a session and writes its first transcript row (opts.Input,
// else the goal) in the same transaction. A goal that is empty after
// strings.TrimSpace is rejected. opts.Metadata is host-owned: it cannot set the
// core-owned keys runtime, subagent or import.
func (e *Engine) NewSession(ctx context.Context, opts SessionOptions) (*SessionHandle, error) {
	if opts.Metadata != nil {
		raw, err := json.Marshal(opts.Metadata)
		if err != nil {
			return nil, err
		}
		if err := session.CheckHostMetadata(raw); err != nil {
			return nil, err
		}
	}
	return e.createSession(ctx, opts)
}

// createSession is the core path: opts.Metadata may carry core-owned keys.
func (e *Engine) createSession(ctx context.Context, opts SessionOptions) (*SessionHandle, error) {
	if strings.TrimSpace(opts.Goal) == "" {
		return nil, errors.New("session goal is required")
	}
	id, err := newSessionID()
	if err != nil {
		return nil, err
	}
	metadata := []byte("{}")
	if opts.Metadata != nil {
		var err error
		metadata, err = json.Marshal(opts.Metadata)
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	v := session.Session{
		ID: id, CreatedAt: now, UpdatedAt: now, Status: "created",
		Goal: opts.Goal, Metadata: metadata,
	}
	first := opts.Input
	if first == "" {
		first = opts.Goal
	}
	if err := e.store.CreateWithMessage(ctx, v, protocol.Message{Role: "user", Content: first}); err != nil {
		return nil, err
	}
	e.events.publish("session.start", id, map[string]any{"goal": opts.Goal})
	_ = e.store.Journal(ctx, "session.start", map[string]any{"sessionId": id, "goal": opts.Goal})
	return &SessionHandle{engine: e, ID: id}, nil
}

func sessionInfo(v session.Session) SessionInfo {
	return SessionInfo{
		ID: v.ID, CreatedAt: v.CreatedAt, UpdatedAt: v.UpdatedAt, Status: v.Status,
		Provider: v.Provider, Model: v.Model, ModelRef: v.ModelRef, Agent: v.AgentID, Goal: v.Goal, Metadata: v.Metadata,
		Final: v.Final, Usage: v.Usage, Error: v.Error,
	}
}

func (e *Engine) Session(ctx context.Context, id string) (*SessionHandle, SessionInfo, error) {
	v, err := e.store.Get(ctx, id)
	if err != nil {
		return nil, SessionInfo{}, err
	}
	return &SessionHandle{engine: e, ID: id}, sessionInfo(v), nil
}

func (e *Engine) ListSessions(ctx context.Context, limit, offset int) ([]SessionInfo, error) {
	items, err := e.store.List(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, 0, len(items))
	for _, item := range items {
		out = append(out, sessionInfo(item))
	}
	return out, nil
}

func (s *SessionHandle) RecentMessages(ctx context.Context, limit int) ([]Message, error) {
	items, err := s.engine.store.RecentMessages(ctx, s.ID, limit)
	if err != nil {
		return nil, err
	}
	return s.engine.hostMessages(ctx, s.ID, items)
}

func streamSnapshot(v streambuf.Snapshot) StreamSnapshot {
	return StreamSnapshot{Head: v.Head, Tail: v.Tail, Total: v.Total, Truncated: v.Truncated}
}

func shellResult(v shell.Result) ShellResult {
	return ShellResult{
		Command: v.Command, ExitCode: v.ExitCode, StartedAt: v.StartedAt, Duration: v.Duration,
		Stdout: streamSnapshot(v.Stdout), Stderr: streamSnapshot(v.Stderr), Cancelled: v.Cancelled,
	}
}

func (s *SessionHandle) RunShell(ctx context.Context, command string) (ShellResult, error) {
	if command == "" {
		return ShellResult{}, errors.New("shell command is required")
	}
	s.engine.events.publish("tool.start", s.ID, map[string]any{"name": "shell", "command": command})
	workspace, lease, err := s.engine.leaseWorkspace(ctx, s.ID)
	if err != nil {
		return ShellResult{}, err
	}
	defer lease.Release()
	runner := s.engine.shell
	if workspace != s.engine.workspace {
		runner = shell.New(workspace)
	}
	result, err := runner.Run(ctx, command, func(chunk shell.StreamEvent) {
		s.engine.events.publish("tool.output", s.ID, map[string]any{
			"name": "shell", "stream": chunk.Stream, "chunk": string(chunk.Chunk),
		})
	})
	payload := map[string]any{
		"name": "shell", "command": command, "exitCode": result.ExitCode,
		"stdoutBytes": result.Stdout.Total, "stderrBytes": result.Stderr.Total,
		"cancelled": result.Cancelled,
	}
	if err != nil {
		payload["error"] = err.Error()
	}
	s.engine.events.publish("tool.end", s.ID, payload)
	// The journal row names the session and the outcome, never the command: the command line is
	// content, and this row is a trace. The event above still carries it for the Desktop.
	_ = s.engine.store.Journal(context.Background(), "tool.shell", map[string]any{
		"sessionId": s.ID, "name": "shell", "exitCode": result.ExitCode,
		"stdoutBytes": result.Stdout.Total, "stderrBytes": result.Stderr.Total,
		"cancelled": result.Cancelled,
	})
	return shellResult(result), err
}

func (e *Engine) ReserveWorkingMemory(bytes int64) (func(), error) {
	return e.resources.Reserve(bytes)
}

func (e *Engine) String() string {
	h := e.Health()
	return fmt.Sprintf("LumenCortex Go runtime %s (%s)", h.Version, h.Workspace)
}
