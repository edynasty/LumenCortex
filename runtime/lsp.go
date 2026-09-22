package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/edynasty/LumenCortex/internal/lsp"
)

type LSPConfig = lsp.Config
type LSPStatus = lsp.Status
type LSPDiagnostic = lsp.Diagnostic

func (e *Engine) LSPStart(ctx context.Context, sessionID string, cfg LSPConfig) (LSPStatus, error) {
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return LSPStatus{}, err
	}
	manager, err := e.lspManager(workspace)
	if err != nil {
		return LSPStatus{}, err
	}
	cfg.Workspace = workspace
	status, err := manager.Start(ctx, cfg)
	if err == nil {
		e.events.publish("lsp.started", sessionID, map[string]any{
			"name": status.Name,
			"command": status.Command,
			"pid": status.PID,
			"workspace": workspace,
		})
	}
	return status, err
}

func (e *Engine) LSPStop(ctx context.Context, sessionID string) error {
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return err
	}
	e.lspMu.Lock()
	manager := e.lspManagers[workspace]
	delete(e.lspManagers, workspace)
	e.lspMu.Unlock()
	if manager == nil {
		return nil
	}
	err = manager.Stop()
	if err == nil {
		e.events.publish("lsp.stopped", sessionID, map[string]any{"workspace": workspace})
	}
	return err
}

func (e *Engine) LSPStatus(ctx context.Context, sessionID string) (LSPStatus, error) {
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return LSPStatus{}, err
	}
	e.lspMu.Lock()
	manager := e.lspManagers[workspace]
	e.lspMu.Unlock()
	if manager == nil {
		return LSPStatus{}, nil
	}
	return manager.Status(), nil
}

func (e *Engine) LSPHover(ctx context.Context, sessionID, path string, line, character int) (json.RawMessage, error) {
	manager, err := e.runningLSP(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Hover(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character))
}

func (e *Engine) LSPDefinition(ctx context.Context, sessionID, path string, line, character int) (json.RawMessage, error) {
	manager, err := e.runningLSP(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Definition(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character))
}

func (e *Engine) LSPReferences(ctx context.Context, sessionID, path string, line, character int, includeDeclaration bool) (json.RawMessage, error) {
	manager, err := e.runningLSP(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.References(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character), includeDeclaration)
}

func (e *Engine) LSPDocumentSymbols(ctx context.Context, sessionID, path string) (json.RawMessage, error) {
	manager, err := e.runningLSP(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.DocumentSymbols(ctx, path)
}

func (e *Engine) LSPWorkspaceSymbols(ctx context.Context, sessionID, query string) (json.RawMessage, error) {
	manager, err := e.runningLSP(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.WorkspaceSymbols(ctx, query)
}

func (e *Engine) LSPDiagnostics(ctx context.Context, sessionID, path string) ([]LSPDiagnostic, error) {
	manager, err := e.runningLSP(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Diagnostics(path)
}

func (e *Engine) LSPRename(ctx context.Context, sessionID, path string, line, character int, newName string) (json.RawMessage, error) {
	manager, err := e.runningLSP(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Rename(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character), newName)
}

func (e *Engine) runningLSP(ctx context.Context, sessionID string) (*lsp.Manager, error) {
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	e.lspMu.Lock()
	manager := e.lspManagers[workspace]
	e.lspMu.Unlock()
	if manager == nil || !manager.Status().Running {
		return nil, lsp.ErrNotRunning
	}
	return manager, nil
}

func (e *Engine) lspManager(workspace string) (*lsp.Manager, error) {
	e.lspMu.Lock()
	defer e.lspMu.Unlock()
	if manager := e.lspManagers[workspace]; manager != nil {
		return manager, nil
	}
	manager, err := lsp.NewManager(workspace)
	if err != nil {
		return nil, err
	}
	e.lspManagers[workspace] = manager
	return manager, nil
}

func (e *Engine) lspWorkspace(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return e.workspace, nil
	}
	return e.agentWorkspace(ctx, sessionID)
}

func (e *Engine) stopLSPWorkspace(workspace string) {
	e.lspMu.Lock()
	manager := e.lspManagers[workspace]
	delete(e.lspManagers, workspace)
	e.lspMu.Unlock()
	if manager != nil {
		_ = manager.Stop()
	}
}

func normalizeLSPPosition(value int) int {
	if value <= 0 {
		return 0
	}
	return value - 1
}

var _ = errors.Is
