package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
)

var ErrNotRunning = errors.New("lsp server is not running")

type Manager struct {
	workspace string
	mu sync.RWMutex
	client *Client
}

func NewManager(workspace string) (*Manager, error) {
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	return &Manager{workspace: root}, nil
}

func (m *Manager) Start(ctx context.Context, cfg Config) (Status, error) {
	if cfg.Workspace == "" {
		cfg.Workspace = m.workspace
	}
	client, err := Start(ctx, cfg)
	if err != nil {
		return Status{}, err
	}

	m.mu.Lock()
	previous := m.client
	m.client = client
	m.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	return client.Status(), nil
}

func (m *Manager) Stop() error {
	m.mu.Lock()
	client := m.client
	m.client = nil
	m.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Close()
}

func (m *Manager) Close() error { return m.Stop() }

func (m *Manager) Status() Status {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()
	if client == nil {
		return Status{}
	}
	return client.Status()
}

func (m *Manager) Diagnostics(path string) ([]Diagnostic, error) {
	client, err := m.current()
	if err != nil {
		return nil, err
	}
	return client.Diagnostics(path), nil
}

func (m *Manager) Hover(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	client, err := m.current()
	if err != nil {
		return nil, err
	}
	return client.Hover(ctx, path, line, character)
}

func (m *Manager) Definition(ctx context.Context, path string, line, character int) (json.RawMessage, error) {
	client, err := m.current()
	if err != nil {
		return nil, err
	}
	return client.Definition(ctx, path, line, character)
}

func (m *Manager) References(ctx context.Context, path string, line, character int, includeDeclaration bool) (json.RawMessage, error) {
	client, err := m.current()
	if err != nil {
		return nil, err
	}
	return client.References(ctx, path, line, character, includeDeclaration)
}

func (m *Manager) DocumentSymbols(ctx context.Context, path string) (json.RawMessage, error) {
	client, err := m.current()
	if err != nil {
		return nil, err
	}
	return client.DocumentSymbols(ctx, path)
}

func (m *Manager) WorkspaceSymbols(ctx context.Context, query string) (json.RawMessage, error) {
	client, err := m.current()
	if err != nil {
		return nil, err
	}
	return client.WorkspaceSymbols(ctx, query)
}

func (m *Manager) Rename(ctx context.Context, path string, line, character int, newName string) (json.RawMessage, error) {
	client, err := m.current()
	if err != nil {
		return nil, err
	}
	return client.Rename(ctx, path, line, character, newName)
}

func (m *Manager) current() (*Client, error) {
	m.mu.RLock()
	client := m.client
	m.mu.RUnlock()
	if client == nil {
		return nil, ErrNotRunning
	}
	return client, nil
}
