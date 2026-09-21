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
	"time"

	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/resource"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/streambuf"
)

const GoRuntimePreviewVersion = "0.1.0-preview"

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
	shell     *shell.Runner
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
	Goal     string         `json:"goal"`
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
	if gitRoot, rootErr := repoService.GitRoot(context.Background()); rootErr == nil {
		worktreeRoot = filepath.Join(filepath.Dir(gitRoot), "."+filepath.Base(gitRoot)+".lumencortex-worktrees")
	}
	return &Engine{
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
		shell:     shell.New(workspace),
	}, nil
}

func (e *Engine) Close() error {
	e.events.close()
	return e.store.Close()
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

func newSessionID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "session_" + hex.EncodeToString(buf), nil
}

func (e *Engine) NewSession(ctx context.Context, opts SessionOptions) (*SessionHandle, error) {
	if opts.Goal == "" {
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
	if err := e.store.Create(ctx, v); err != nil {
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

func (s *SessionHandle) AppendMessage(ctx context.Context, role string, payload any) (int64, error) {
	seq, err := s.engine.store.AppendMessage(ctx, s.ID, role, payload)
	if err == nil {
		s.engine.events.publish("session.message", s.ID, map[string]any{"seq": seq, "role": role})
	}
	return seq, err
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
	result, err := s.engine.shell.Run(ctx, command, func(chunk shell.StreamEvent) {
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
