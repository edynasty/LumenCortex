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

	"github.com/edynasty/LumenCortex/internal/resource"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
)

const GoRuntimePreviewVersion = "0.1.0-preview"

type Options struct {
	Workspace string
	RepoDir   string
	Budget    resource.Budget
}

type Engine struct {
	workspace string
	repoDir   string
	store     *session.Store
	resources *resource.Manager
	events    *eventBus
	shell     *shell.Runner
}

type Health struct {
	Version   string          `json:"version"`
	Workspace string          `json:"workspace"`
	Database  string          `json:"database"`
	Budget    resource.Budget `json:"budget"`
	UsedBytes int64           `json:"usedBytes"`
	Pressure  bool            `json:"pressure"`
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
	store, err := session.Open(repoDir)
	if err != nil {
		return nil, err
	}
	return &Engine{
		workspace: workspace,
		repoDir:   repoDir,
		store:     store,
		resources: resource.New(opts.Budget),
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
		Budget:    e.resources.Budget(),
		UsedBytes: e.resources.UsedBytes(),
		Pressure:  e.resources.UnderPressure(),
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
	metadata, err := json.Marshal(opts.Metadata)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	v := session.Session{
		ID: id, CreatedAt: now, UpdatedAt: now, Status: "running",
		Provider: opts.Provider, Model: opts.Model, Goal: opts.Goal, Metadata: metadata,
	}
	if err := e.store.Create(ctx, v); err != nil {
		return nil, err
	}
	e.events.publish("session.start", id, map[string]any{"goal": opts.Goal})
	_ = e.store.Journal(ctx, "session.start", map[string]any{"sessionId": id, "goal": opts.Goal})
	return &SessionHandle{engine: e, ID: id}, nil
}

func (e *Engine) Session(ctx context.Context, id string) (*SessionHandle, session.Session, error) {
	v, err := e.store.Get(ctx, id)
	if err != nil {
		return nil, session.Session{}, err
	}
	return &SessionHandle{engine: e, ID: id}, v, nil
}

func (e *Engine) ListSessions(ctx context.Context, limit, offset int) ([]session.Session, error) {
	return e.store.List(ctx, limit, offset)
}

func (s *SessionHandle) AppendMessage(ctx context.Context, role string, payload any) (int64, error) {
	seq, err := s.engine.store.AppendMessage(ctx, s.ID, role, payload)
	if err == nil {
		s.engine.events.publish("session.message", s.ID, map[string]any{"seq": seq, "role": role})
	}
	return seq, err
}

func (s *SessionHandle) RecentMessages(ctx context.Context, limit int) ([]session.Message, error) {
	return s.engine.store.RecentMessages(ctx, s.ID, limit)
}

func (s *SessionHandle) RunShell(ctx context.Context, command string) (shell.Result, error) {
	if command == "" {
		return shell.Result{}, errors.New("shell command is required")
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
	return result, err
}

func (e *Engine) ReserveWorkingMemory(bytes int64) (func(), error) {
	return e.resources.Reserve(bytes)
}

func (e *Engine) String() string {
	h := e.Health()
	return fmt.Sprintf("LumenCortex Go runtime %s (%s)", h.Version, h.Workspace)
}
