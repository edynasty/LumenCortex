package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/edynasty/LumenCortex/internal/session"
)

const (
	RuntimeLocal    = "local"
	RuntimeWorktree = "worktree"
)

type SessionRuntime struct {
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	Base   string `json:"base,omitempty"`
	Head   string `json:"head,omitempty"`
}

func (e *Engine) SessionRuntime(ctx context.Context, sessionID string) (SessionRuntime, error) {
	current, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return SessionRuntime{}, err
	}
	metadata := map[string]any{}
	if len(current.Metadata) > 0 {
		if err := json.Unmarshal(current.Metadata, &metadata); err != nil {
			return SessionRuntime{}, err
		}
	}
	rawRuntime, ok := metadata["runtime"]
	if !ok {
		return SessionRuntime{Kind: RuntimeLocal, Path: e.workspace}, nil
	}
	raw, err := json.Marshal(rawRuntime)
	if err != nil {
		return SessionRuntime{}, err
	}
	var identity SessionRuntime
	if err := json.Unmarshal(raw, &identity); err != nil {
		return SessionRuntime{}, err
	}
	if identity.Kind == "" || identity.Kind == RuntimeLocal {
		return SessionRuntime{Kind: RuntimeLocal, Path: e.workspace}, nil
	}
	if identity.Kind != RuntimeWorktree {
		return SessionRuntime{}, errors.New("unsupported session runtime kind")
	}
	if err := e.validateManagedWorktreePath(identity.Path); err != nil {
		return SessionRuntime{}, err
	}
	return identity, nil
}

func (e *Engine) AttachWorktree(ctx context.Context, sessionID, base string) (SessionRuntime, error) {
	current, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return SessionRuntime{}, err
	}
	existing, err := e.SessionRuntime(ctx, sessionID)
	if err != nil {
		return SessionRuntime{}, err
	}
	if existing.Kind == RuntimeWorktree {
		return existing, nil
	}
	base = strings.TrimSpace(base)
	if base == "" {
		base = "HEAD"
	}
	if err := os.MkdirAll(e.worktreeRoot, 0o755); err != nil {
		return SessionRuntime{}, err
	}
	short := strings.TrimPrefix(sessionID, "session_")
	if len(short) > 16 {
		short = short[:16]
	}
	branch := "lumencortex/" + short
	path := filepath.Join(e.worktreeRoot, sessionID)
	if err := e.validateManagedWorktreePath(path); err != nil {
		return SessionRuntime{}, err
	}
	worktree, err := e.repo.GitCreateWorktree(ctx, path, branch, base)
	if err != nil {
		return SessionRuntime{}, err
	}
	identity := SessionRuntime{
		Kind:   RuntimeWorktree,
		Path:   filepath.Clean(worktree.Path),
		Branch: worktree.Branch,
		Base:   base,
		Head:   worktree.Head,
	}
	if identity.Branch == "" {
		identity.Branch = branch
	}

	metadata := map[string]any{}
	if len(current.Metadata) > 0 {
		if err := json.Unmarshal(current.Metadata, &metadata); err != nil {
			return SessionRuntime{}, err
		}
	}
	metadata["runtime"] = identity
	raw, err := json.Marshal(metadata)
	if err != nil {
		return SessionRuntime{}, err
	}
	message := json.RawMessage(raw)
	if err := e.store.Update(ctx, sessionID, session.Patch{Metadata: &message}); err != nil {
		_ = e.repo.GitRemoveWorktree(context.Background(), identity.Path, true)
		return SessionRuntime{}, err
	}
	e.events.publish("worktree.created", sessionID, map[string]any{
		"path": identity.Path, "branch": identity.Branch, "base": identity.Base,
	})
	return identity, nil
}

func (e *Engine) RemoveSessionWorktree(ctx context.Context, sessionID string, force bool) error {
	identity, err := e.SessionRuntime(ctx, sessionID)
	if err != nil {
		return err
	}
	if identity.Kind != RuntimeWorktree {
		return nil
	}
	if err := e.validateManagedWorktreePath(identity.Path); err != nil {
		return err
	}
	e.stopLSPWorkspace(identity.Path)
	if e.mcpRegistry != nil {
		e.mcpRegistry.StopWorkspace(identity.Path)
	}
	if err := e.repo.GitRemoveWorktree(ctx, identity.Path, force); err != nil {
		return err
	}

	current, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return err
	}
	metadata := map[string]any{}
	if len(current.Metadata) > 0 {
		if err := json.Unmarshal(current.Metadata, &metadata); err != nil {
			return err
		}
	}
	delete(metadata, "runtime")
	raw, err := json.Marshal(metadata)
	if err != nil {
		return err
	}
	message := json.RawMessage(raw)
	if err := e.store.Update(ctx, sessionID, session.Patch{Metadata: &message}); err != nil {
		return err
	}
	e.events.publish("worktree.removed", sessionID, map[string]any{"path": identity.Path})
	return nil
}

func (e *Engine) agentWorkspace(ctx context.Context, sessionID string) (string, error) {
	identity, err := e.SessionRuntime(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if identity.Kind == RuntimeLocal {
		return e.workspace, nil
	}
	if err := e.validateManagedWorktreePath(identity.Path); err != nil {
		return "", err
	}
	info, err := os.Stat(identity.Path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("session worktree is not a directory")
	}
	return identity.Path, nil
}

func (e *Engine) validateManagedWorktreePath(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("worktree path is required")
	}
	root, err := filepath.Abs(e.worktreeRoot)
	if err != nil {
		return err
	}
	candidate, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, candidate)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("worktree path is outside the managed worktree root")
	}
	return nil
}
