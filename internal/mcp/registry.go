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

const (
	ScopeGlobal    = "global"
	ScopeProject   = "project"
	ScopeEffective = "effective"
)

type Registry struct {
	workspace         string
	globalConfigPath  string
	projectConfigPath string

	mu             sync.RWMutex
	globalConfigs  map[string]Config
	projectConfigs map[string]Config
	clients        map[string]*Client
}

func NewRegistry(workspace, configPath string) (*Registry, error) {
	return NewLayeredRegistry(workspace, "", configPath)
}

func NewLayeredRegistry(workspace, globalConfigPath, projectConfigPath string) (*Registry, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("mcp registry workspace is required")
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if projectConfigPath == "" {
		projectConfigPath = filepath.Join(root, ".lumencortex", "mcp.json")
	}
	projectConfigPath, err = filepath.Abs(projectConfigPath)
	if err != nil {
		return nil, err
	}
	if globalConfigPath != "" {
		globalConfigPath, err = filepath.Abs(globalConfigPath)
		if err != nil {
			return nil, err
		}
	}
	r := &Registry{
		workspace:         root,
		globalConfigPath:  globalConfigPath,
		projectConfigPath: projectConfigPath,
		globalConfigs:     make(map[string]Config),
		projectConfigs:    make(map[string]Config),
		clients:           make(map[string]*Client),
	}
	if globalConfigPath != "" {
		r.globalConfigs, err = loadConfigFile(globalConfigPath)
		if err != nil {
			return nil, err
		}
	}
	r.projectConfigs, err = loadConfigFile(projectConfigPath)
	if err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Registry) Configs() []Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return sortedConfigs(effectiveConfigs(r.globalConfigs, r.projectConfigs))
}

func (r *Registry) ConfigsScope(scope string) ([]Config, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch scope {
	case ScopeGlobal:
		return sortedConfigs(r.globalConfigs), nil
	case ScopeProject:
		return sortedConfigs(r.projectConfigs), nil
	case ScopeEffective, "":
		return sortedConfigs(effectiveConfigs(r.globalConfigs, r.projectConfigs)), nil
	default:
		return nil, errors.New("unknown mcp config scope")
	}
}

func (r *Registry) Upsert(cfg Config) error {
	return r.UpsertScope(ScopeProject, cfg)
}

func (r *Registry) UpsertScope(scope string, cfg Config) error {
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
	target, path, err := r.scopeLocked(scope)
	if err != nil {
		return err
	}
	if _, exists := target[cfg.ID]; !exists && len(target) >= MaxServers {
		return errors.New("mcp server configuration limit exceeded")
	}
	target[cfg.ID] = cfg
	r.stopInstancesLocked(cfg.ID)
	return saveConfigFile(path, target)
}

func (r *Registry) Delete(id string) error {
	return r.DeleteScope(ScopeProject, id)
}

func (r *Registry) DeleteScope(scope, id string) error {
	id = strings.TrimSpace(id)
	r.mu.Lock()
	defer r.mu.Unlock()
	target, path, err := r.scopeLocked(scope)
	if err != nil {
		return err
	}
	delete(target, id)
	r.stopInstancesLocked(id)
	return saveConfigFile(path, target)
}

func (r *Registry) scopeLocked(scope string) (map[string]Config, string, error) {
	switch scope {
	case ScopeGlobal:
		if r.globalConfigPath == "" {
			return nil, "", errors.New("global mcp config path is unavailable")
		}
		return r.globalConfigs, r.globalConfigPath, nil
	case ScopeProject, "":
		return r.projectConfigs, r.projectConfigPath, nil
	default:
		return nil, "", errors.New("mcp config scope must be global or project")
	}
}

func (r *Registry) stopInstancesLocked(id string) {
	for key, client := range r.clients {
		if strings.HasSuffix(key, "\x00"+id) {
			_ = client.Close()
			delete(r.clients, key)
		}
	}
}

func (r *Registry) Start(ctx context.Context, id, workspace string) (Status, error) {
	id = strings.TrimSpace(id)
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return Status{}, err
	}

	r.mu.RLock()
	cfg, ok := r.projectConfigs[id]
	if !ok {
		cfg, ok = r.globalConfigs[id]
	}
	r.mu.RUnlock()
	if !ok {
		return Status{}, fmt.Errorf("unknown mcp server: %s", id)
	}
	if cfg.Disabled {
		return Status{}, fmt.Errorf("mcp server is disabled: %s", id)
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

func loadConfigFile(path string) (map[string]Config, error) {
	configs := make(map[string]Config)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return configs, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxConfigBytes {
		return nil, errors.New("mcp configuration exceeds runtime limit")
	}
	var file ConfigFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, err
	}
	if len(file.Servers) > MaxServers {
		return nil, errors.New("mcp configuration contains too many servers")
	}
	for id, cfg := range file.Servers {
		if cfg.ID == "" {
			cfg.ID = id
		}
		cfg.Workspace = ""
		if !serverIDPattern.MatchString(cfg.ID) || strings.TrimSpace(cfg.Command) == "" {
			return nil, fmt.Errorf("invalid mcp server configuration: %s", id)
		}
		if cfg.ProtocolMode == "" {
			cfg.ProtocolMode = ModeLegacy
		}
		configs[cfg.ID] = cfg
	}
	return configs, nil
}

func saveConfigFile(path string, configs map[string]Config) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("mcp config path is unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	servers := make(map[string]Config, len(configs))
	for id, cfg := range configs {
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func effectiveConfigs(global, project map[string]Config) map[string]Config {
	out := make(map[string]Config, len(global)+len(project))
	for id, cfg := range global {
		out[id] = cfg
	}
	for id, cfg := range project {
		out[id] = cfg
	}
	return out
}

func sortedConfigs(configs map[string]Config) []Config {
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Config, 0, len(ids))
	for _, id := range ids {
		cfg := configs[id]
		cfg.Workspace = ""
		out = append(out, cfg)
	}
	return out
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
