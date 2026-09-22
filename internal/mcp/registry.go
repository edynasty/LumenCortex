package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const (
	MaxServers     = 64
	MaxConfigBytes = 1 << 20
)

var serverIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

type ConfigFile struct {
	Servers map[string]Config `json:"servers"`
}

type AgentTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
	ServerID    string         `json:"serverId"`
	ToolName    string         `json:"toolName"`
	ReadOnly    bool           `json:"readOnly"`
}

type Registry struct {
	workspace  string
	configPath string

	mu      sync.RWMutex
	configs map[string]Config
	clients map[string]*Client
}

func NewRegistry(workspace, configPath string) (*Registry, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("mcp registry workspace is required")
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if configPath == "" {
		configPath = filepath.Join(root, ".lumencortex", "mcp.json")
	}
	configPath, err = filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	r := &Registry{
		workspace: root,
		configPath: configPath,
		configs: make(map[string]Config),
		clients: make(map[string]*Client),
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) Configs() []Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.configs))
	for id := range r.configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Config, 0, len(ids))
	for _, id := range ids {
		cfg := r.configs[id]
		cfg.Workspace = ""
		out = append(out, cfg)
	}
	return out
}

func (r *Registry) Upsert(cfg Config) error {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.Command = strings.TrimSpace(cfg.Command)
	cfg.Workspace = ""
	if !serverIDPattern.MatchString(cfg.ID) {
		return errors.New("mcp server id must match [A-Za-z0-9._-] and be at most 64 characters")
	}
	if cfg.Command == "" {
		return errors.New("mcp command is required")
	}
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = ModeLegacy
	}
	if cfg.ProtocolMode != ModeLegacy && cfg.ProtocolMode != ModeModern {
		return errors.New("unsupported mcp protocol mode")
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.configs[cfg.ID]; !exists && len(r.configs) >= MaxServers {
		return errors.New("mcp server configuration limit exceeded")
	}
	r.configs[cfg.ID] = cfg
	return r.saveLocked()
}

func (r *Registry) Delete(id string) error {
	id = strings.TrimSpace(id)
	r.mu.Lock()
	for key, client := range r.clients {
		if strings.HasSuffix(key, "\x00"+id) {
			_ = client.Close()
			delete(r.clients, key)
		}
	}
	delete(r.configs, id)
	err := r.saveLocked()
	r.mu.Unlock()
	return err
}

func (r *Registry) Start(ctx context.Context, id, workspace string) (Status, error) {
	id = strings.TrimSpace(id)
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return Status{}, err
	}

	r.mu.RLock()
	cfg, ok := r.configs[id]
	r.mu.RUnlock()
	if !ok {
		return Status{}, fmt.Errorf("unknown mcp server: %s", id)
	}
	cfg.Workspace = workspace

	client, err := Start(ctx, cfg)
	if err != nil {
		return Status{}, err
	}
	key := instanceKey(workspace, id)

	r.mu.Lock()
	previous := r.clients[key]
	r.clients[key] = client
	r.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	return client.Status(), nil
}

func (r *Registry) Stop(id, workspace string) error {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return err
	}
	key := instanceKey(workspace, strings.TrimSpace(id))
	r.mu.Lock()
	client := r.clients[key]
	delete(r.clients, key)
	r.mu.Unlock()
	if client == nil {
		return nil
	}
	return client.Close()
}

func (r *Registry) StopWorkspace(workspace string) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return
	}
	prefix := workspace + "\x00"
	r.mu.Lock()
	clients := make([]*Client, 0)
	for key, client := range r.clients {
		if strings.HasPrefix(key, prefix) {
			clients = append(clients, client)
			delete(r.clients, key)
		}
	}
	r.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
}

func (r *Registry) Statuses(workspace string) ([]Status, error) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	prefix := workspace + "\x00"
	r.mu.RLock()
	statuses := make([]Status, 0)
	for key, client := range r.clients {
		if strings.HasPrefix(key, prefix) {
			statuses = append(statuses, client.Status())
		}
	}
	r.mu.RUnlock()
	sort.Slice(statuses, func(i, j int) bool { return statuses[i].ID < statuses[j].ID })
	return statuses, nil
}

func (r *Registry) RefreshTools(ctx context.Context, id, workspace string) ([]Tool, error) {
	client, err := r.client(id, workspace)
	if err != nil {
		return nil, err
	}
	return client.RefreshTools(ctx)
}

func (r *Registry) AgentTools(workspace string) ([]AgentTool, error) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	prefix := workspace + "\x00"
	r.mu.RLock()
	clients := make([]*Client, 0)
	for key, client := range r.clients {
		if strings.HasPrefix(key, prefix) && client.Status().Running {
			clients = append(clients, client)
		}
	}
	r.mu.RUnlock()

	out := make([]AgentTool, 0)
	seen := map[string]bool{}
	for _, client := range clients {
		status := client.Status()
		for _, tool := range client.Tools() {
			name := agentToolName(status.ID, tool.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			readOnly, _ := tool.Annotations["readOnlyHint"].(bool)
			out = append(out, AgentTool{
				Name: name,
				Description: tool.Description,
				InputSchema: tool.InputSchema,
				ServerID: status.ID,
				ToolName: tool.Name,
				ReadOnly: readOnly,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *Registry) CallTool(ctx context.Context, workspace, serverID, toolName string, args map[string]any) (CallToolResult, error) {
	client, err := r.client(serverID, workspace)
	if err != nil {
		return CallToolResult{}, err
	}
	return client.CallTool(ctx, toolName, args)
}

func (r *Registry) Close() error {
	r.mu.Lock()
	clients := r.clients
	r.clients = make(map[string]*Client)
	r.mu.Unlock()
	var firstErr error
	for _, client := range clients {
		if err := client.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (r *Registry) client(id, workspace string) (*Client, error) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	key := instanceKey(workspace, strings.TrimSpace(id))
	r.mu.RLock()
	client := r.clients[key]
	r.mu.RUnlock()
	if client == nil || !client.Status().Running {
		return nil, errors.New("mcp server is not running in this runtime")
	}
	return client, nil
}

func (r *Registry) runtimeWorkspace(workspace string) (string, error) {
	if strings.TrimSpace(workspace) == "" {
		workspace = r.workspace
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("mcp runtime workspace is not a directory")
	}
	return filepath.Clean(abs), nil
}

func (r *Registry) load() error {
	raw, err := os.ReadFile(r.configPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(raw) > MaxConfigBytes {
		return errors.New("mcp configuration exceeds runtime limit")
	}
	var file ConfigFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return err
	}
	if len(file.Servers) > MaxServers {
		return errors.New("mcp configuration contains too many servers")
	}
	for id, cfg := range file.Servers {
		if cfg.ID == "" {
			cfg.ID = id
		}
		cfg.Workspace = ""
		if !serverIDPattern.MatchString(cfg.ID) || strings.TrimSpace(cfg.Command) == "" {
			return fmt.Errorf("invalid mcp server configuration: %s", id)
		}
		if cfg.ProtocolMode == "" {
			cfg.ProtocolMode = ModeLegacy
		}
		r.configs[cfg.ID] = cfg
	}
	return nil
}

func (r *Registry) saveLocked() error {
	if err := os.MkdirAll(filepath.Dir(r.configPath), 0o700); err != nil {
		return err
	}
	servers := make(map[string]Config, len(r.configs))
	for id, cfg := range r.configs {
		cfg.Workspace = ""
		servers[id] = cfg
	}
	raw, err := json.MarshalIndent(ConfigFile{Servers: servers}, "", "  ")
	if err != nil {
		return err
	}
	if len(raw) > MaxConfigBytes {
		return errors.New("mcp configuration exceeds runtime limit")
	}
	tmp := r.configPath + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.configPath)
}

func instanceKey(workspace, id string) string {
	return filepath.Clean(workspace) + "\x00" + id
}

func agentToolName(serverID, toolName string) string {
	return "mcp__" + sanitizeToolPart(serverID, 16) + "__" + sanitizeToolPart(toolName, 40)
}

func sanitizeToolPart(value string, maxLen int) string {
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
		if b.Len() >= maxLen {
			break
		}
	}
	if b.Len() == 0 {
		return "tool"
	}
	return b.String()
}
