package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/internal/mcp"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/resource"
	"github.com/edynasty/LumenCortex/internal/runlock"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/skills"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/streambuf"
	"github.com/edynasty/LumenCortex/protocol"
)

const GoRuntimePreviewVersion = "0.1.0-preview"

// ErrRunHeld reports that another Engine, in this process or another, holds the
// session's run lock. Nothing was written.
var ErrRunHeld = errors.New("run is active in another process or engine")

type Budget struct {
	SoftBytes int64 `json:"softBytes"`
	HardBytes int64 `json:"hardBytes"`
	MaxAgents int   `json:"maxAgents"`
}

type Options struct {
	Workspace string
	RepoDir   string
	Budget    Budget
}

type Engine struct {
	workspace string
	repoDir      string
	worktreeRoot string
	store        *session.Store
	resources *resource.Manager
	repo      *repository.Service
	events    *eventBus
	shell       *shell.Runner
	lspMu       sync.Mutex
	lspManagers map[string]*lsp.Manager
	mcpRegistry  *mcp.Registry
	skillRegistry *skills.Registry
	cognitionHealth *cognition.HealthRegistry
}

type CognitionHealthOptions struct {
	FailureThreshold int   `json:"failureThreshold,omitempty"`
	CooldownMS       int64 `json:"cooldownMs,omitempty"`
}

type Health struct {
	Version   string `json:"version"`
	Workspace string `json:"workspace"`
	Database  string `json:"database"`
	Budget    Budget `json:"budget"`
	UsedBytes    int64 `json:"usedBytes"`
	ActiveAgents int   `json:"activeAgents"`
	Pressure     bool  `json:"pressure"`
}

type SessionInfo struct {
	ID        string          `json:"id"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
	Status    string          `json:"status"`
	Provider  string          `json:"provider,omitempty"`
	Model     string          `json:"model,omitempty"`
	Goal      string          `json:"goal"`
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	Final     *string         `json:"final,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
}

type Message struct {
	SessionID string          `json:"sessionId"`
	Seq       int64           `json:"seq"`
	Role      string          `json:"role"`
	JSON      json.RawMessage `json:"json"`
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
	Provider string         `json:"provider,omitempty"`
	Model    string         `json:"model,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type SessionHandle struct {
	engine *Engine
	ID     string `json:"id"`
}

func Open(opts Options) (*Engine, error) {
	workspace := opts.Workspace
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	repoDir := opts.RepoDir
	if repoDir == "" {
		repoDir = filepath.Join(workspace, ".lumencortex")
	}
	worktreeRoot := filepath.Join(filepath.Dir(workspace), "."+filepath.Base(workspace)+".lumencortex-worktrees")
	store, err := session.Open(repoDir)
	if err != nil {
		return nil, err
	}
	repoService, err := repository.New(workspace)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	globalMCPPath := ""
	globalSkillRoot := ""
	if userConfigDir, configErr := os.UserConfigDir(); configErr == nil && userConfigDir != "" {
		globalMCPPath = filepath.Join(userConfigDir, "lumencortex", "mcp.json")
		globalSkillRoot = filepath.Join(userConfigDir, "lumencortex", "skills")
	}
	mcpRegistry, err := mcp.NewLayeredRegistry(
		workspace,
		globalMCPPath,
		filepath.Join(repoDir, "mcp.json"),
	)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	skillRegistry, err := skills.NewRegistry(
		globalSkillRoot,
		filepath.Join(workspace, ".lumencortex", "skills"),
	)
	if err != nil {
		_ = mcpRegistry.Close()
		_ = store.Close()
		return nil, err
	}
	if gitRoot, rootErr := repoService.GitRoot(context.Background()); rootErr == nil {
		worktreeRoot = filepath.Join(filepath.Dir(gitRoot), "."+filepath.Base(gitRoot)+".lumencortex-worktrees")
		_ = repoService.EnsureLocalExclude(context.Background(), repoDir)
	}
	engine := &Engine{
		workspace:    workspace,
		repoDir:      repoDir,
		worktreeRoot: worktreeRoot,
		store:     store,
		resources: resource.New(resource.Budget{
			SoftBytes: opts.Budget.SoftBytes,
			HardBytes: opts.Budget.HardBytes,
			MaxAgents: opts.Budget.MaxAgents,
		}),
		repo:      repoService,
		events:    newEventBus(),
		shell:       shell.New(workspace),
		lspManagers: make(map[string]*lsp.Manager),
		mcpRegistry:   mcpRegistry,
		skillRegistry: skillRegistry,
		cognitionHealth: cognition.NewHealthRegistry(cognition.HealthConfig{
			FailureThreshold: 3,
			Cooldown: 30 * time.Second,
		}),
	}
	if err := engine.reconcileRunning(context.Background()); err != nil {
		_ = engine.Close()
		return nil, err
	}
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

// DeleteSession removes a session and its transcript. It takes the session's run
// lock and returns ErrRunHeld while a run holds it.
func (e *Engine) DeleteSession(ctx context.Context, sessionID string) error {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return err
	}
	err = e.store.Delete(ctx, sessionID)
	if releaseErr := lock.Release(); err != nil || releaseErr != nil {
		return errors.Join(err, releaseErr)
	}
	_ = runlock.Remove(e.runDir(), sessionID)
	_ = e.store.Journal(ctx, "session.delete", map[string]any{"sessionId": sessionID})
	return nil
}

func (e *Engine) Close() error {
	e.events.close()
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
	return e.store.Close()
}

func (e *Engine) ConfigureCognitionHealth(opts CognitionHealthOptions) {
	cooldown := time.Duration(opts.CooldownMS) * time.Millisecond
	e.cognitionHealth = cognition.NewHealthRegistry(cognition.HealthConfig{
		FailureThreshold: opts.FailureThreshold,
		Cooldown: cooldown,
		CooldownMS: opts.CooldownMS,
	})
}

func (e *Engine) Health() Health {
	return Health{
		Version:   GoRuntimePreviewVersion,
		Workspace: e.workspace,
		Database:  e.store.Path(),
		Budget: Budget{
			SoftBytes: e.resources.Budget().SoftBytes,
			HardBytes: e.resources.Budget().HardBytes,
			MaxAgents: e.resources.Budget().MaxAgents,
		},
		UsedBytes:    e.resources.UsedBytes(),
		ActiveAgents: e.resources.ActiveAgents(),
		Pressure:     e.resources.UnderPressure(),
	}
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
// core-owned keys runtime or subagent.
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
		Provider: opts.Provider, Model: opts.Model, Goal: opts.Goal, Metadata: metadata,
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
		Provider: v.Provider, Model: v.Model, Goal: v.Goal, Metadata: v.Metadata,
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
	out := make([]Message, 0, len(items))
	for _, item := range items {
		out = append(out, Message{SessionID: item.SessionID, Seq: item.Seq, Role: item.Role, JSON: item.JSON})
	}
	return out, nil
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
	workspace, err := s.engine.agentWorkspace(ctx, s.ID)
	if err != nil {
		return ShellResult{}, err
	}
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
	_ = s.engine.store.Journal(context.Background(), "tool.shell", payload)
	return shellResult(result), err
}

func (e *Engine) ReserveWorkingMemory(bytes int64) (func(), error) {
	return e.resources.Reserve(bytes)
}

func (e *Engine) String() string {
	h := e.Health()
	return fmt.Sprintf("LumenCortex Go runtime %s (%s)", h.Version, h.Workspace)
}
