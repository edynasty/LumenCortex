package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

func (c *Client) Hover(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	doc, err := c.syncDocument(path)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	err = c.Request(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": doc.uri},
		"position": Position{Line: max(0, line), Character: max(0, character)},
	}, &result)
	return result, err
}

func (c *Client) Definition(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	doc, err := c.syncDocument(path)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	err = c.Request(ctx, "textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": doc.uri},
		"position": Position{Line: max(0, line), Character: max(0, character)},
	}, &result)
	return result, err
}

func (c *Client) References(ctx context.Context, path string, line, character int, includeDeclaration bool) (json.RawMessage, error) {
	doc, err := c.syncDocument(path)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	err = c.Request(ctx, "textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": doc.uri},
		"position": Position{Line: max(0, line), Character: max(0, character)},
		"context": map[string]any{"includeDeclaration": includeDeclaration},
	}, &result)
	return result, err
}

func (c *Client) DocumentSymbols(ctx context.Context, path string) (json.RawMessage, error) {
	doc, err := c.syncDocument(path)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	err = c.Request(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": doc.uri},
	}, &result)
	return result, err
}

func (c *Client) WorkspaceSymbols(ctx context.Context, query string) (json.RawMessage, error) {
	var result json.RawMessage
	err := c.Request(ctx, "workspace/symbol", map[string]any{"query": query}, &result)
	return result, err
}

func (c *Client) Rename(ctx context.Context, path string, line, character int, newName string) (json.RawMessage, error) {
	if strings.TrimSpace(newName) == "" {
		return nil, errors.New("rename new name is required")
	}
	doc, err := c.syncDocument(path)
	if err != nil {
		return nil, err
	}
	var result json.RawMessage
	err = c.Request(ctx, "textDocument/rename", map[string]any{
		"textDocument": map[string]any{"uri": doc.uri},
		"position": Position{Line: max(0, line), Character: max(0, character)},
		"newName": newName,
	}, &result)
	return result, err
}

type syncedDocument struct {
	uri string
}

func (c *Client) syncDocument(path string) (syncedDocument, error) {
	abs, err := c.resolvePath(path)
	if err != nil {
		return syncedDocument{}, err
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return syncedDocument{}, err
	}
	if len(raw) > MaxDocumentBytes {
		return syncedDocument{}, ErrDocumentTooLarge
	}
	content := string(raw)
	uri := fileURI(abs)
	languageID := c.languageID(abs)

	c.docMu.Lock()
	state, opened := c.documents[abs]
	if !opened {
		state = documentState{version: 1, content: content}
		c.documents[abs] = state
		c.docMu.Unlock()
		if err := c.Notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{
				"uri": uri,
				"languageId": languageID,
				"version": state.version,
				"text": content,
			},
		}); err != nil {
			return syncedDocument{}, err
		}
		return syncedDocument{uri: uri}, nil
	}
	if state.content == content {
		c.docMu.Unlock()
		return syncedDocument{uri: uri}, nil
	}
	state.version++
	state.content = content
	c.documents[abs] = state
	c.docMu.Unlock()

	if err := c.Notify("textDocument/didChange", map[string]any{
		"textDocument": map[string]any{"uri": uri, "version": state.version},
		"contentChanges": []map[string]any{{"text": content}},
	}); err != nil {
		return syncedDocument{}, err
	}
	return syncedDocument{uri: uri}, nil
}

func (c *Client) resolvePath(input string) (string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", errors.New("lsp path is required")
	}
	candidate := input
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(c.cfg.Workspace, candidate)
	}
	candidate, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(c.cfg.Workspace, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("lsp path escapes workspace")
	}
	realWorkspace, err := filepath.EvalSymlinks(c.cfg.Workspace)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	realRel, err := filepath.Rel(realWorkspace, real)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", errors.New("lsp path escapes workspace through symlink")
	}
	return candidate, nil
}

func (c *Client) languageID(path string) string {
	if strings.TrimSpace(c.cfg.LanguageID) != "" {
		return c.cfg.LanguageID
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts", ".tsx":
		return "typescript"
	case ".js", ".jsx", ".mjs", ".cjs":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	case ".java":
		return "java"
	case ".json":
		return "json"
	case ".yaml", ".yml":
		return "yaml"
	default:
		return "plaintext"
	}
}
