package toolset

import (
	"context"
	"encoding/json"

	"github.com/edynasty/LumenCortex/protocol"
)

func (s *Set) registerLSPTools() {
	positionProps := map[string]any{
		"path": str(),
		"line": map[string]any{"type": "integer", "minimum": 1},
		"column": map[string]any{"type": "integer", "minimum": 1},
	}
	s.add(protocol.ToolSpec{
		Name: "lsp_hover",
		Description: "Ask the active language server for hover/type information at a 1-based file position.",
		Permission: "read",
		Parameters: obj(positionProps, "path", "line", "column"),
	}, s.lspHover)
	s.add(protocol.ToolSpec{
		Name: "lsp_definition",
		Description: "Find language-server definition targets for a 1-based file position.",
		Permission: "read",
		Parameters: obj(positionProps, "path", "line", "column"),
	}, s.lspDefinition)
	s.add(protocol.ToolSpec{
		Name: "lsp_references",
		Description: "Find language-server references for a 1-based file position.",
		Permission: "read",
		Parameters: obj(map[string]any{
			"path": str(),
			"line": map[string]any{"type": "integer", "minimum": 1},
			"column": map[string]any{"type": "integer", "minimum": 1},
			"include_declaration": map[string]any{"type": "boolean"},
		}, "path", "line", "column"),
	}, s.lspReferences)
	s.add(protocol.ToolSpec{
		Name: "lsp_document_symbols",
		Description: "List language-server symbols in a document.",
		Permission: "read",
		Parameters: obj(map[string]any{"path": str()}, "path"),
	}, s.lspDocumentSymbols)
	s.add(protocol.ToolSpec{
		Name: "lsp_workspace_symbols",
		Description: "Search language-server workspace symbols.",
		Permission: "read",
		Parameters: obj(map[string]any{"query": str()}, "query"),
	}, s.lspWorkspaceSymbols)
	s.add(protocol.ToolSpec{
		Name: "lsp_diagnostics",
		Description: "Read cached language-server diagnostics for a workspace file.",
		Permission: "read",
		Parameters: obj(map[string]any{"path": str()}, "path"),
	}, s.lspDiagnostics)
	s.add(protocol.ToolSpec{
		Name: "lsp_rename",
		Description: "Preview the language-server workspace edit for renaming a symbol. This tool does not apply the edit.",
		Permission: "read",
		Parameters: obj(map[string]any{
			"path": str(),
			"line": map[string]any{"type": "integer", "minimum": 1},
			"column": map[string]any{"type": "integer", "minimum": 1},
			"new_name": str(),
		}, "path", "line", "column", "new_name"),
	}, s.lspRename)
}

func (s *Set) lspHover(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	raw, err := s.lsp.Hover(ctx, stringArg(args, "path"), intArg(args, "line", 1)-1, intArg(args, "column", 1)-1)
	return rawLSPResult(raw, err)
}

func (s *Set) lspDefinition(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	raw, err := s.lsp.Definition(ctx, stringArg(args, "path"), intArg(args, "line", 1)-1, intArg(args, "column", 1)-1)
	return rawLSPResult(raw, err)
}

func (s *Set) lspReferences(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	raw, err := s.lsp.References(
		ctx,
		stringArg(args, "path"),
		intArg(args, "line", 1)-1,
		intArg(args, "column", 1)-1,
		boolArg(args, "include_declaration"),
	)
	return rawLSPResult(raw, err)
}

func (s *Set) lspDocumentSymbols(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	raw, err := s.lsp.DocumentSymbols(ctx, stringArg(args, "path"))
	return rawLSPResult(raw, err)
}

func (s *Set) lspWorkspaceSymbols(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	raw, err := s.lsp.WorkspaceSymbols(ctx, stringArg(args, "query"))
	return rawLSPResult(raw, err)
}

func (s *Set) lspDiagnostics(_ context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	items, err := s.lsp.Diagnostics(stringArg(args, "path"))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, _ := json.Marshal(items)
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

func (s *Set) lspRename(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	raw, err := s.lsp.Rename(
		ctx,
		stringArg(args, "path"),
		intArg(args, "line", 1)-1,
		intArg(args, "column", 1)-1,
		stringArg(args, "new_name"),
	)
	return rawLSPResult(raw, err)
}

func rawLSPResult(raw json.RawMessage, err error) (protocol.ToolResult, error) {
	if err != nil {
		return protocol.ToolResult{}, err
	}
	if len(raw) == 0 {
		raw = json.RawMessage("null")
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}
