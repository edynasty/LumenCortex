package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/editing"

	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/protocol"
)

// lspSpecs is the static list of the language-server tools. registerLSPTools binds each spec to its
// handler; the capability registry lists them through LSPSpecs; nothing else defines them.
func lspSpecs() []protocol.ToolSpec {
	positionProps := map[string]any{
		"path":   str(),
		"line":   map[string]any{"type": "integer", "minimum": 1},
		"column": map[string]any{"type": "integer", "minimum": 1},
	}
	return []protocol.ToolSpec{
		{
			Name:        "lsp_hover",
			Description: "Ask the workspace language server for hover/type information at a 1-based file position.",
			Permission:  "read",
			Parameters:  obj(positionProps, "path", "line", "column"),
		},
		{
			Name:        "lsp_definition",
			Description: "Find language-server definition targets for a 1-based file position.",
			Permission:  "read",
			Parameters:  obj(positionProps, "path", "line", "column"),
		},
		{
			Name:        "lsp_references",
			Description: "Find language-server references for a 1-based file position.",
			Permission:  "read",
			Parameters: obj(map[string]any{
				"path":                str(),
				"line":                map[string]any{"type": "integer", "minimum": 1},
				"column":              map[string]any{"type": "integer", "minimum": 1},
				"include_declaration": map[string]any{"type": "boolean"},
			}, "path", "line", "column"),
		},
		{
			Name:        "lsp_document_symbols",
			Description: "List language-server symbols in a document.",
			Permission:  "read",
			Parameters:  obj(map[string]any{"path": str()}, "path"),
		},
		{
			Name:        "lsp_workspace_symbols",
			Description: "Search language-server workspace symbols.",
			Permission:  "read",
			Parameters:  obj(map[string]any{"query": str()}, "query"),
		},
		{
			Name:        "lsp_diagnostics",
			Description: "Read language-server diagnostics for a workspace file.",
			Permission:  "read",
			Parameters:  obj(map[string]any{"path": str()}, "path"),
		},
		{
			Name: "lsp_rename",
			Description: "Rename a symbol through the workspace language server and apply the resulting workspace edit. " +
				"Every edited path passes the same guard as write_file and apply_patch (workspace confinement, " +
				"protected state, symlink escape) and the batch is written atomically: if any path is refused, " +
				"nothing is written.",
			Permission:       "write",
			MutatesWorkspace: true,
			Parameters: obj(map[string]any{
				"path":     str(),
				"line":     map[string]any{"type": "integer", "minimum": 1},
				"column":   map[string]any{"type": "integer", "minimum": 1},
				"new_name": str(),
			}, "path", "line", "column", "new_name"),
		},
	}
}

// LSPSpecs returns the specs of the language-server tools.
func LSPSpecs() []protocol.ToolSpec { return lspSpecs() }

// registerLSPTools offers the language-server tools only when a server can really serve this run: in a
// trusted workspace, a server whose command is on PATH and whose language the workspace holds (a file
// or a marker), started by the first call; in an untrusted one, a server the host already started
// (ADR-0013). A run without one gets no lsp_* tool, so the model is never offered a capability that
// cannot answer and falls back to search_text and find_files on its own.
func (s *Set) registerLSPTools() {
	if s.lsp == nil || !s.lsp.Available() {
		return
	}
	handlers := map[string]func(context.Context, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error){
		"lsp_hover":             s.lspHover,
		"lsp_definition":        s.lspDefinition,
		"lsp_references":        s.lspReferences,
		"lsp_document_symbols":  s.lspDocumentSymbols,
		"lsp_workspace_symbols": s.lspWorkspaceSymbols,
		"lsp_diagnostics":       s.lspDiagnostics,
		"lsp_rename":            s.lspRename,
	}
	for _, spec := range lspSpecs() {
		s.add(spec, handlers[spec.Name])
	}
}

func (s *Set) lspHover(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	started := time.Now()
	raw, err := s.lsp.Hover(ctx, stringArg(args, "path"), intArg(args, "line", 1)-1, intArg(args, "column", 1)-1)
	return s.lspResult(ctx, "lsp_hover", raw, err, started), nil
}

func (s *Set) lspDefinition(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	started := time.Now()
	raw, err := s.lsp.Definition(ctx, stringArg(args, "path"), intArg(args, "line", 1)-1, intArg(args, "column", 1)-1)
	return s.lspResult(ctx, "lsp_definition", raw, err, started), nil
}

func (s *Set) lspReferences(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	started := time.Now()
	raw, err := s.lsp.References(
		ctx,
		stringArg(args, "path"),
		intArg(args, "line", 1)-1,
		intArg(args, "column", 1)-1,
		boolArg(args, "include_declaration"),
	)
	return s.lspResult(ctx, "lsp_references", raw, err, started), nil
}

func (s *Set) lspDocumentSymbols(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	started := time.Now()
	raw, err := s.lsp.DocumentSymbols(ctx, stringArg(args, "path"))
	return s.lspResult(ctx, "lsp_document_symbols", raw, err, started), nil
}

func (s *Set) lspWorkspaceSymbols(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	started := time.Now()
	raw, err := s.lsp.WorkspaceSymbols(ctx, stringArg(args, "query"))
	return s.lspResult(ctx, "lsp_workspace_symbols", raw, err, started), nil
}

func (s *Set) lspDiagnostics(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	started := time.Now()
	items, err := s.lsp.Diagnostics(ctx, stringArg(args, "path"))
	if err != nil {
		return s.lspFailure(ctx, "lsp_diagnostics", err, started), nil
	}
	raw, err := json.Marshal(items)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

// lspRename asks the server for the workspace edit and applies it through the file tools' guard and
// atomic write path: a rename is a file modification like any other, and it is refused as a whole
// when one of its paths is.
func (s *Set) lspRename(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	started := time.Now()
	newName := strings.TrimSpace(stringArg(args, "new_name"))
	if newName == "" {
		return protocol.ToolResult{OK: false, Content: `{"error":"new_name is required"}`}, nil
	}
	raw, err := s.lsp.Rename(
		ctx,
		stringArg(args, "path"),
		intArg(args, "line", 1)-1,
		intArg(args, "column", 1)-1,
		newName,
	)
	if err != nil {
		return s.lspFailure(ctx, "lsp_rename", err, started), nil
	}
	var edit lsp.WorkspaceEdit
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &edit); err != nil {
			return protocol.ToolResult{OK: false, Content: `{"error":"the language server returned an edit that is not a workspace edit","hint":"nothing was written; use search_text and replace_in_file instead"}`}, nil
		}
	}
	applied, err := s.applyWorkspaceEdit(ctx, edit)
	if err != nil {
		code, hint := "refused", "the rename was refused as a whole; use search_text and replace_in_file instead"
		if editErr, ok := editing.AsError(err); ok {
			code, hint = editErr.Code, editErr.Hint()
		}
		content, _ := json.Marshal(map[string]any{
			"error": err.Error(),
			"code":  code,
			"hint":  hint,
		})
		s.log.Warn("lsp rename refused", "tool", "lsp_rename", "error_code", code,
			"duration_ms", time.Since(started).Milliseconds(), "tool_call_id", protocol.ToolCallID(ctx))
		return protocol.ToolResult{OK: false, Content: string(content)}, nil
	}
	content, err := json.Marshal(map[string]any{"applied": applied, "filesChanged": len(applied)})
	if err != nil {
		return protocol.ToolResult{}, err
	}
	s.log.Info("lsp rename applied", "tool", "lsp_rename", "files", len(applied),
		"duration_ms", time.Since(started).Milliseconds(), "tool_call_id", protocol.ToolCallID(ctx))
	return protocol.ToolResult{OK: true, Content: string(content), MutatesWorkspace: len(applied) > 0}, nil
}

// lspResult turns a server answer into a tool result, and a server failure into a result the model
// can act on instead of an opaque error.
func (s *Set) lspResult(ctx context.Context, tool string, raw json.RawMessage, err error, started time.Time) protocol.ToolResult {
	if err != nil {
		return s.lspFailure(ctx, tool, err, started)
	}
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}
}

func (s *Set) lspFailure(ctx context.Context, tool string, err error, started time.Time) protocol.ToolResult {
	code, hint := lspFailureReason(err)
	content, marshalErr := json.Marshal(map[string]any{"error": err.Error(), "code": code, "hint": hint})
	if marshalErr != nil {
		content = []byte(`{"error":"language server request failed"}`)
	}
	s.log.Warn("lsp tool failed", "tool", tool, "error_code", code,
		"duration_ms", time.Since(started).Milliseconds(), "tool_call_id", protocol.ToolCallID(ctx))
	return protocol.ToolResult{OK: false, Content: string(content)}
}

// lspFailureReason classifies a language-server failure and names the fallback the model should use.
func lspFailureReason(err error) (code, hint string) {
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled", "the request was cancelled; nothing was changed"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "the language server did not answer in time; use search_text or find_files to locate the code instead"
	case errors.Is(err, lsp.ErrServerLimit):
		return "busy", "every language server is busy; retry later, or use search_text or find_files instead"
	case errors.Is(err, lsp.ErrUnavailable), errors.Is(err, lsp.ErrNotRunning), errors.Is(err, lsp.ErrClosed):
		return "unavailable", "no language server is available for this file; use search_text or find_files to locate the code instead"
	}
	return "failed", "the language server failed; use search_text or find_files instead"
}

// newToolsetLogger keeps a nil logger from turning every log line into a nil dereference.
func newToolsetLogger(logger *slog.Logger) *slog.Logger {
	if logger != nil {
		return logger
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
