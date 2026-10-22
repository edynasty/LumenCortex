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
	// Permission is the effective permission (read, write or exec) the tool is exposed with;
	// PermissionSource says whether the server annotation or a user override decided it.
	Permission       string `json:"permission"`
	PermissionSource string `json:"permissionSource,omitempty"`
	Scope            string `json:"scope,omitempty"`
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

	trustMu sync.RWMutex
	trust   func() bool

	mu             sync.RWMutex
	globalConfigs  map[string]Config
	projectConfigs map[string]Config
	instances      map[string]*instance
}

// instance is one running client of this Registry with the configuration and scope it was started
// from. The scope decides whether the project trust gate applies to it.
type instance struct {
	client *Client
	cfg    Config
	scope  string
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
		instances:         make(map[string]*instance),
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

// SetProjectTrust installs the check that reports whether the workspace whose project file this
// registry reads is trusted (ADR-0013). It runs on every operation that can start a process or
// expose a tool, so withdrawing trust takes effect for a running server without reopening the
// registry. A registry without a check treats its project configuration as untrusted.
func (r *Registry) SetProjectTrust(check func() bool) {
	r.trustMu.Lock()
	r.trust = check
	r.trustMu.Unlock()
}

func (r *Registry) projectTrusted() bool {
	r.trustMu.RLock()
	check := r.trust
	r.trustMu.RUnlock()
	return check != nil && check()
}

func (r *Registry) Configs() []Config {
	trusted := r.projectTrusted()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.effectiveConfigsLocked(trusted)
}

// effectiveConfigsLocked lists the configurations that can be started, with their scope. While the
// project is untrusted its file is ignored entirely: a project entry that shadows a global id is
// replaced by the global entry, and a project-only entry is listed as blocked so the reason is
// visible instead of the server silently disappearing.
func (r *Registry) effectiveConfigsLocked(trusted bool) []Config {
	out := make([]Config, 0, len(r.globalConfigs)+len(r.projectConfigs))
	for id, cfg := range r.globalConfigs {
		if _, shadowed := r.projectConfigs[id]; shadowed && trusted {
			continue
		}
		out = append(out, listingConfig(cfg, ScopeGlobal, false))
	}
	for id, cfg := range r.projectConfigs {
		if _, shadowed := r.globalConfigs[id]; shadowed && !trusted {
			continue
		}
		out = append(out, listingConfig(cfg, ScopeProject, !trusted))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) ConfigsScope(scope string) ([]Config, error) {
	trusted := r.projectTrusted()
	r.mu.RLock()
	defer r.mu.RUnlock()
	switch scope {
	case ScopeGlobal:
		return listingConfigs(r.globalConfigs, ScopeGlobal, false), nil
	case ScopeProject:
		return listingConfigs(r.projectConfigs, ScopeProject, !trusted), nil
	case ScopeEffective, "":
		return r.effectiveConfigsLocked(trusted), nil
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
	cfg.Scope = ""
	cfg.BlockedByTrust = false
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
	if err := validateTools(cfg); err != nil {
		return err
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
	for key, inst := range r.instances {
		if strings.HasSuffix(key, "\x00"+id) {
			_ = inst.client.Close()
			delete(r.instances, key)
		}
	}
}

// Start starts one server for workspace. A server defined only by the project file of an untrusted
// workspace is refused with ErrUntrustedProject before any process is created; a global server, or
// one the project file shadows while untrusted, starts from its global entry as if the project file
// did not exist.
func (r *Registry) Start(ctx context.Context, id, workspace string) (Status, error) {
	id = strings.TrimSpace(id)
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return Status{}, err
	}

	trusted := r.projectTrusted()
	r.mu.RLock()
	cfg, scope, ok := r.resolveLocked(id, trusted)
	_, projectDefined := r.projectConfigs[id]
	r.mu.RUnlock()
	if !ok {
		if projectDefined && !trusted {
			return Status{}, fmt.Errorf("%w: %s", ErrUntrustedProject, id)
		}
		return Status{}, fmt.Errorf("unknown mcp server: %s", id)
	}
	if cfg.Disabled {
		return Status{}, fmt.Errorf("mcp server is disabled: %s", id)
	}
	spawn := cfg
	spawn.Workspace = workspace

	client, err := Start(ctx, spawn)
	if err != nil {
		return Status{}, err
	}
	key := instanceKey(workspace, id)

	r.mu.Lock()
	previous := r.instances[key]
	r.instances[key] = &instance{client: client, cfg: cfg, scope: scope}
	r.mu.Unlock()
	if previous != nil {
		_ = previous.client.Close()
	}
	status := client.Status()
	status.Scope = scope
	status.BlockedByTrust = scope == ScopeProject && !trusted
	return status, nil
}

// resolveLocked returns the configuration a start would use and the file it came from. While the
// project is untrusted its file is ignored entirely.
func (r *Registry) resolveLocked(id string, trusted bool) (Config, string, bool) {
	if cfg, ok := r.projectConfigs[id]; ok && trusted {
		return cfg, ScopeProject, true
	}
	if cfg, ok := r.globalConfigs[id]; ok {
		return cfg, ScopeGlobal, true
	}
	return Config{}, "", false
}

func (r *Registry) Stop(id, workspace string) error {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return err
	}
	key := instanceKey(workspace, strings.TrimSpace(id))
	r.mu.Lock()
	inst := r.instances[key]
	delete(r.instances, key)
	r.mu.Unlock()
	if inst == nil {
		return nil
	}
	return inst.client.Close()
}

func (r *Registry) StopWorkspace(workspace string) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return
	}
	prefix := workspace + "\x00"
	r.mu.Lock()
	instances := make([]*instance, 0)
	for key, inst := range r.instances {
		if strings.HasPrefix(key, prefix) {
			instances = append(instances, inst)
			delete(r.instances, key)
		}
	}
	r.mu.Unlock()
	for _, inst := range instances {
		_ = inst.client.Close()
	}
}

func (r *Registry) Statuses(workspace string) ([]Status, error) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	trusted := r.projectTrusted()
	prefix := workspace + "\x00"
	r.mu.RLock()
	statuses := make([]Status, 0)
	for key, inst := range r.instances {
		if strings.HasPrefix(key, prefix) {
			status := inst.client.Status()
			status.Scope = inst.scope
			status.BlockedByTrust = inst.scope == ScopeProject && !trusted
			statuses = append(statuses, status)
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
	trusted := r.projectTrusted()
	prefix := workspace + "\x00"
	r.mu.RLock()
	instances := make([]*instance, 0)
	for key, inst := range r.instances {
		if !strings.HasPrefix(key, prefix) || !inst.client.Status().Running {
			continue
		}
		// A project server keeps running when trust is withdrawn, but its tools stop being exposed
		// to runs (ADR-0013).
		if inst.scope == ScopeProject && !trusted {
			continue
		}
		instances = append(instances, inst)
	}
	r.mu.RUnlock()

	out := make([]AgentTool, 0)
	seen := map[string]bool{}
	for _, inst := range instances {
		status := inst.client.Status()
		for _, tool := range inst.client.Tools() {
			name := agentToolName(status.ID, tool.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			permission, source := effectivePermission(inst.cfg, inst.scope, trusted, tool)
			out = append(out, AgentTool{
				Name:             name,
				Description:      tool.Description,
				InputSchema:      tool.InputSchema,
				ServerID:         status.ID,
				ToolName:         tool.Name,
				ReadOnly:         permission == PermissionRead,
				Permission:       permission,
				PermissionSource: source,
				Scope:            inst.scope,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// effectivePermission is the permission a tool is exposed with. The server's readOnlyHint annotation
// decides the default; an entry in the server's tools map overrides it, but only when the entry is
// user-owned (global) or comes from a trusted project, so a project file cannot claim read-only
// access its server does not declare.
func effectivePermission(cfg Config, scope string, trusted bool, tool Tool) (permission, source string) {
	permission = PermissionExec
	if readOnly, _ := tool.Annotations["readOnlyHint"].(bool); readOnly {
		permission = PermissionRead
	}
	if scope == ScopeProject && !trusted {
		return permission, PermissionSourceAnnotation
	}
	if override, ok := cfg.Tools[tool.Name]; ok && override.Permission != "" {
		return override.Permission, PermissionSourceOverride
	}
	return permission, PermissionSourceAnnotation
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
	instances := r.instances
	r.instances = make(map[string]*instance)
	r.mu.Unlock()
	var firstErr error
	for _, inst := range instances {
		if err := inst.client.Close(); err != nil && firstErr == nil {
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
	trusted := r.projectTrusted()
	key := instanceKey(workspace, strings.TrimSpace(id))
	r.mu.RLock()
	inst := r.instances[key]
	r.mu.RUnlock()
	if inst == nil || !inst.client.Status().Running {
		return nil, errors.New("mcp server is not running in this runtime")
	}
	if inst.scope == ScopeProject && !trusted {
		return nil, fmt.Errorf("%w: %s", ErrUntrustedProject, id)
	}
	return inst.client, nil
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
		cfg.Scope = ""
		cfg.BlockedByTrust = false
		if !serverIDPattern.MatchString(cfg.ID) || strings.TrimSpace(cfg.Command) == "" {
			return nil, fmt.Errorf("invalid mcp server configuration: %s", id)
		}
		if cfg.ProtocolMode == "" {
			cfg.ProtocolMode = ModeLegacy
		}
		if err := validateTools(cfg); err != nil {
			return nil, err
		}
		configs[cfg.ID] = cfg
	}
	return configs, nil
}

// validateTools rejects a tools entry whose permission is not one this runtime can enforce, naming
// the server and the tool key so the configuration error points at the line to fix.
func validateTools(cfg Config) error {
	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("mcp server %q: tools keys must be non-empty tool names", cfg.ID)
		}
		switch cfg.Tools[name].Permission {
		case PermissionRead, PermissionWrite, PermissionExec:
		default:
			return fmt.Errorf("mcp server %q: tools[%q].permission must be one of read, write, exec", cfg.ID, name)
		}
	}
	return nil
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
		cfg.Scope = ""
		cfg.BlockedByTrust = false
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

func listingConfigs(configs map[string]Config, scope string, blocked bool) []Config {
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Config, 0, len(ids))
	for _, id := range ids {
		out = append(out, listingConfig(configs[id], scope, blocked))
	}
	return out
}

// listingConfig copies one configuration for a listing, without the fields that only describe the
// listing.
func listingConfig(cfg Config, scope string, blocked bool) Config {
	cfg.Workspace = ""
	cfg.Scope = scope
	cfg.BlockedByTrust = blocked
	return cfg
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
