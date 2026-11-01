package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/lsp"
)

type LSPConfig = lsp.Config
type LSPStatus = lsp.Status
type LSPDiagnostic = lsp.Diagnostic
type LSPServerStatus = lsp.ServerStatus

// LSPStatus reports every language server of the session's workspace: its language, root, state
// (running, idle, stopped, unavailable), pid, when it started and was last used, and the last error.
// It starts nothing; a manager and its catalog exist as objects, not as processes.
func (e *Engine) LSPStatus(ctx context.Context, sessionID string) ([]LSPServerStatus, error) {
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	manager, err := e.lspManagerFor(workspace)
	if err != nil {
		return nil, err
	}
	return manager.Servers(), nil
}

// LSPStart starts the language server of cfg's language now, rather than on the first lsp_* call. It
// is the host's explicit start; a run does not need it.
func (e *Engine) LSPStart(ctx context.Context, sessionID string, cfg LSPConfig) (LSPStatus, error) {
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return LSPStatus{}, err
	}
	manager, err := e.lspManagerFor(workspace)
	if err != nil {
		return LSPStatus{}, err
	}
	cfg.Workspace = workspace
	return manager.Start(ctx, cfg)
}

// LSPStop stops every language server of the session's workspace and forgets the manager: no process
// is left, and the next request starts one again.
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
	return manager.Close()
}

func (e *Engine) LSPHover(ctx context.Context, sessionID, path string, line, character int) (json.RawMessage, error) {
	manager, err := e.lspFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Hover(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character))
}

func (e *Engine) LSPDefinition(ctx context.Context, sessionID, path string, line, character int) (json.RawMessage, error) {
	manager, err := e.lspFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Definition(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character))
}

func (e *Engine) LSPReferences(ctx context.Context, sessionID, path string, line, character int, includeDeclaration bool) (json.RawMessage, error) {
	manager, err := e.lspFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.References(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character), includeDeclaration)
}

func (e *Engine) LSPDocumentSymbols(ctx context.Context, sessionID, path string) (json.RawMessage, error) {
	manager, err := e.lspFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.DocumentSymbols(ctx, path)
}

func (e *Engine) LSPWorkspaceSymbols(ctx context.Context, sessionID, query string) (json.RawMessage, error) {
	manager, err := e.lspFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.WorkspaceSymbols(ctx, query)
}

func (e *Engine) LSPDiagnostics(ctx context.Context, sessionID, path string) ([]LSPDiagnostic, error) {
	manager, err := e.lspFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Diagnostics(ctx, path)
}

// LSPRename returns the workspace edit of a rename without applying it. The agent tool lsp_rename
// applies it through the file tools' guard; a host that applies the edit itself owns the same rules.
func (e *Engine) LSPRename(ctx context.Context, sessionID, path string, line, character int, newName string) (json.RawMessage, error) {
	manager, err := e.lspFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return manager.Rename(ctx, path, normalizeLSPPosition(line), normalizeLSPPosition(character), newName)
}

// lspFor returns the workspace's manager when a server can serve it. The manager starts the server of
// the requested file's language on the first call.
func (e *Engine) lspFor(ctx context.Context, sessionID string) (*lsp.Manager, error) {
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	manager, err := e.lspManagerFor(workspace)
	if err != nil {
		return nil, err
	}
	if !manager.Available() {
		return nil, fmt.Errorf("%w: no language server is configured for a file in %s", lsp.ErrUnavailable, workspace)
	}
	return manager, nil
}

// lspManagerFor returns the manager of a workspace, creating it with the configuration in force. The
// manager holds no process until a request needs one.
func (e *Engine) lspManagerFor(workspace string) (*lsp.Manager, error) {
	settings := e.lspSettings()
	settings.Workspace = workspace
	e.lspMu.Lock()
	defer e.lspMu.Unlock()
	if manager := e.lspManagers[workspace]; manager != nil {
		manager.Configure(settings)
		return manager, nil
	}
	manager, err := lsp.NewManager(settings)
	if err != nil {
		return nil, err
	}
	e.lspManagers[workspace] = manager
	return manager, nil
}

// lspSettings is the global file's lsp section with its defaults, as the manager options of a
// workspace. A configuration that does not load gives the defaults: the run that read the catalog
// reports that error.
func (e *Engine) lspSettings() lsp.Options {
	settings := (*config.LSP)(nil).Limits()
	if cfg, err := config.Load(e.workspace); err == nil {
		settings = cfg.Effective().LSP
	}
	options := lsp.Options{
		Logger:         e.log,
		IdleTimeout:    time.Duration(settings.IdleTimeoutSeconds) * time.Second,
		MaxServers:     settings.MaxServers,
		StartTimeout:   time.Duration(settings.StartTimeoutSeconds) * time.Second,
		RequestTimeout: time.Duration(settings.RequestTimeoutSeconds) * time.Second,
		OnEvent:        e.publishLSPEvent,
		Servers:        lspServers(settings.Servers),
	}
	return options
}

// lspServers converts the configured catalog into manager entries, in a stable order. An entry
// without extensions or markers keeps the built-in ones of its language.
func lspServers(configured map[string]config.LSPServer) []lsp.LanguageServer {
	if len(configured) == 0 {
		return nil
	}
	languages := make([]string, 0, len(configured))
	for language := range configured {
		languages = append(languages, language)
	}
	sort.Strings(languages)
	out := make([]lsp.LanguageServer, 0, len(languages))
	for _, language := range languages {
		entry := configured[language]
		out = append(out, lsp.LanguageServer{
			Language:   language,
			Command:    entry.Command,
			Args:       entry.Args,
			Extensions: entry.Extensions,
			Markers:    entry.Markers,
		})
	}
	return out
}

// publishLSPEvent puts a language server lifecycle step on the event bus. The manager is shared by
// every session of a workspace, so the event names the workspace rather than a session.
func (e *Engine) publishLSPEvent(event lsp.Event) {
	payload := map[string]any{
		"language":  event.Language,
		"workspace": event.Workspace,
	}
	if event.PID != 0 {
		payload["pid"] = event.PID
	}
	if event.Duration > 0 {
		payload["durationMs"] = event.Duration.Milliseconds()
	}
	if event.ErrorCode != "" {
		payload["errorCode"] = event.ErrorCode
	}
	if event.Message != "" {
		payload["message"] = event.Message
	}
	e.events.publish("lsp."+event.Type, "", payload)
}

func (e *Engine) lspWorkspace(ctx context.Context, sessionID string) (string, error) {
	if sessionID == "" {
		return e.workspace, nil
	}
	return e.agentWorkspace(ctx, sessionID)
}

func normalizeLSPPosition(value int) int {
	if value <= 0 {
		return 0
	}
	return value - 1
}

// stopLSPWorkspace closes the language servers of a workspace that is going away (a deleted
// worktree): the manager and its processes are dropped, and a later manager for the same path starts
// from nothing.
func (e *Engine) stopLSPWorkspace(workspace string) {
	e.lspMu.Lock()
	manager := e.lspManagers[workspace]
	delete(e.lspManagers, workspace)
	e.lspMu.Unlock()
	if manager != nil {
		_ = manager.Close()
	}
}
