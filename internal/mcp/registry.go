package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
)

const (
	MaxServers     = 64
	MaxConfigBytes = 1 << 20
	// MaxCatalogBytes bounds one persisted tool catalog.
	MaxCatalogBytes = 1 << 20
	// maxTrackedServers bounds the per-workspace lifecycle table; over it, stopped servers are
	// forgotten oldest-first.
	maxTrackedServers = MaxServers * 4
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
	// ToolSource is "live" when the server answered this listing and "cache" when the tool list comes
	// from the catalog a previous start recorded.
	ToolSource string `json:"toolSource,omitempty"`
}

// ConfigRequirements is what a server needs before it can start, computed from configuration and
// the environment without starting it.
type ConfigRequirements struct {
	// MissingEnv names environment variables a "{env:NAME}" reference points at that are unset.
	MissingEnv []string `json:"missingEnv,omitempty"`
	// MissingBinary is the command a stdio server needs that does not resolve on PATH.
	MissingBinary string `json:"missingBinary,omitempty"`
	// NeedsTrust reports that the entry comes from the project file, so the workspace must be trusted.
	NeedsTrust bool `json:"needsTrust,omitempty"`
	// MissingEndpoint is the Unix socket a streamable-http server points at that does not exist.
	MissingEndpoint string `json:"missingEndpoint,omitempty"`
}

// Empty reports whether nothing is missing.
func (r ConfigRequirements) Empty() bool {
	return len(r.MissingEnv) == 0 && r.MissingBinary == "" && r.MissingEndpoint == "" && !r.NeedsTrust
}

// ServerState is one configured server as the registry sees it, without starting anything.
type ServerState struct {
	ID             string             `json:"id"`
	Name           string             `json:"name,omitempty"`
	Transport      Transport          `json:"transport"`
	Scope          string             `json:"scope,omitempty"`
	Enabled        bool               `json:"enabled"`
	Running        bool               `json:"running"`
	BlockedByTrust bool               `json:"blockedByTrust,omitempty"`
	Tools          int                `json:"tools"`
	LastError      string             `json:"lastError,omitempty"`
	Requirements   ConfigRequirements `json:"configRequirements"`
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

	// onStop is told when the registry stops a server on its own (idle timeout or a crash), so the
	// host can publish the event. It is called without the registry lock held.
	onStop func(serverID, workspace, reason string)

	stateMu sync.RWMutex
	// stateDir is where tool catalogs are persisted; empty keeps them in memory only.
	stateDir string

	mu             sync.Mutex
	globalConfigs  map[string]Config
	projectConfigs map[string]Config
	servers        map[string]*server
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
		servers:           make(map[string]*server),
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

// SetStateDir makes tool catalogs survive a restart: a run can offer a server's tools without
// starting it, from the list a previous start recorded. An empty directory keeps them in memory.
func (r *Registry) SetStateDir(dir string) {
	r.stateMu.Lock()
	r.stateDir = dir
	r.stateMu.Unlock()
}

// SetStopHook installs the callback that reports a server the registry stopped by itself.
func (r *Registry) SetStopHook(hook func(serverID, workspace, reason string)) {
	r.mu.Lock()
	r.onStop = hook
	r.mu.Unlock()
}

func (r *Registry) publishStopped(s *server, reason string) {
	r.mu.Lock()
	hook := r.onStop
	r.mu.Unlock()
	if hook != nil {
		cfg, _ := s.config()
		hook(cfg.ID, s.workspace, reason)
	}
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	cfg.URL = strings.TrimSpace(cfg.URL)
	cfg.Workspace = ""
	cfg.Scope = ""
	cfg.BlockedByTrust = false
	if !serverIDPattern.MatchString(cfg.ID) {
		return errors.New("mcp server id must match [A-Za-z0-9._-] and be at most 64 characters")
	}
	if cfg.ProtocolMode == "" {
		cfg.ProtocolMode = ModeLegacy
	}
	if cfg.ProtocolMode != ModeLegacy && cfg.ProtocolMode != ModeModern {
		return errors.New("unsupported mcp protocol mode")
	}
	if err := validateConfig(cfg); err != nil {
		return err
	}
	if err := validateTools(cfg); err != nil {
		return err
	}

	r.mu.Lock()
	target, path, err := r.scopeLocked(scope)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	if _, exists := target[cfg.ID]; !exists && len(target) >= MaxServers {
		r.mu.Unlock()
		return errors.New("mcp server configuration limit exceeded")
	}
	previous, existed := target[cfg.ID]
	target[cfg.ID] = cfg
	if existed {
		// The host's own edit: a catalog the previous definition recorded serves the new one when only
		// the exposure changed.
		r.rerecordCatalogs(previous, cfg, scope)
	}
	stopped := r.reconfigureLocked(cfg.ID)
	saveErr := saveConfigFile(path, target)
	r.mu.Unlock()
	// A process the old entry started is closed outside the registry lock, after the sessions it opened:
	// waiting for a server to exit must not block every other registry operation.
	closeReleases(stopped)
	return saveErr
}

func (r *Registry) Delete(id string) error {
	return r.DeleteScope(ScopeProject, id)
}

func (r *Registry) DeleteScope(scope, id string) error {
	id = strings.TrimSpace(id)
	r.mu.Lock()
	target, path, err := r.scopeLocked(scope)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	delete(target, id)
	stopped := r.stopServersLocked(id)
	saveErr := saveConfigFile(path, target)
	r.mu.Unlock()
	closeReleases(stopped)
	return saveErr
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

// reconfigureLocked re-reads the effective configuration of one id into the lifecycle table, so a
// changed entry stops the process the old one started. It returns the clients the caller must close
// after releasing the registry lock.
func (r *Registry) reconfigureLocked(id string) []release {
	trusted := r.projectTrusted()
	cfg, scope, ok := r.resolveLocked(id, trusted)
	var stopped []release
	for _, srv := range r.servers {
		if !strings.HasSuffix(srv.key, "\x00"+id) {
			continue
		}
		if !ok {
			cfg = Config{ID: id, Disabled: true}
		}
		stopped = append(stopped, srv.configure(cfg, scope))
	}
	return stopped
}

// stopServersLocked forgets the lifecycle entries of one id, returning the clients to close after the
// registry lock is released.
func (r *Registry) stopServersLocked(id string) []release {
	var stopped []release
	for key, srv := range r.servers {
		if strings.HasSuffix(key, "\x00"+id) {
			stopped = append(stopped, srv.detach())
			delete(r.servers, key)
		}
	}
	return stopped
}

// serverLocked returns the lifecycle entry of a configured server in one workspace, creating it
// without starting anything.
func (r *Registry) serverLocked(cfg Config, scope, workspace string) *server {
	key := instanceKey(workspace, cfg.ID)
	if existing := r.servers[key]; existing != nil {
		// The effective entry can change without an edit through this registry (trust withdrawn moves
		// an id to its global entry); the client the old entry started is closed outside the lock.
		if stale := existing.configure(cfg, scope); stale.client != nil {
			go stale.close()
		}
		return existing
	}
	if len(r.servers) >= maxTrackedServers {
		r.pruneServersLocked()
	}
	created := newServer(r, key, workspace, cfg, scope)
	r.servers[key] = created
	return created
}

// pruneServersLocked forgets stopped servers, oldest use first, when the table is over its bound.
func (r *Registry) pruneServersLocked() {
	type candidate struct {
		key  string
		used int64
	}
	candidates := make([]candidate, 0, len(r.servers))
	for key, srv := range r.servers {
		srv.mu.Lock()
		running := srv.current != nil
		used := srv.lastUsed.UnixNano()
		srv.mu.Unlock()
		if running {
			continue
		}
		candidates = append(candidates, candidate{key: key, used: used})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].used < candidates[j].used })
	for _, entry := range candidates {
		if len(r.servers) < maxTrackedServers {
			return
		}
		delete(r.servers, entry.key)
	}
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

// server resolves the lifecycle entry for an explicit operation, refusing a project-only server of an
// untrusted workspace before anything is created.
func (r *Registry) server(id, workspace string) (*server, error) {
	id = strings.TrimSpace(id)
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	trusted := r.projectTrusted()
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, scope, ok := r.resolveLocked(id, trusted)
	_, projectDefined := r.projectConfigs[id]
	if !ok {
		if projectDefined && !trusted {
			return nil, fmt.Errorf("%w: %s", ErrUntrustedProject, id)
		}
		return nil, fmt.Errorf("unknown mcp server: %s", id)
	}
	if cfg.Disabled {
		return nil, fmt.Errorf("%w: %s", ErrServerDisabled, id)
	}
	return r.serverLocked(cfg, scope, workspace), nil
}

// Start starts one server for workspace. A server defined only by the project file of an untrusted
// workspace is refused with ErrUntrustedProject before any process is created; a global server, or
// one the project file shadows while untrusted, starts from its global entry as if the project file
// did not exist.
func (r *Registry) Start(ctx context.Context, id, workspace string) (Status, error) {
	srv, err := r.server(id, workspace)
	if err != nil {
		return Status{}, err
	}
	if _, err := srv.ensure(ctx); err != nil {
		return Status{}, err
	}
	return srv.status(r.projectTrusted()), nil
}

func (r *Registry) Stop(id, workspace string) error {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return err
	}
	key := instanceKey(workspace, strings.TrimSpace(id))
	r.mu.Lock()
	srv := r.servers[key]
	r.mu.Unlock()
	if srv == nil {
		return nil
	}
	return srv.stop()
}

func (r *Registry) StopWorkspace(workspace string) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return
	}
	prefix := workspace + "\x00"
	r.mu.Lock()
	servers := make([]*server, 0)
	for key, srv := range r.servers {
		if strings.HasPrefix(key, prefix) {
			servers = append(servers, srv)
			delete(r.servers, key)
		}
	}
	r.mu.Unlock()
	for _, srv := range servers {
		_ = srv.stop()
	}
}

// Statuses lists the servers of the workspace that are running. It starts nothing; States is the full
// configured view, running or not.
func (r *Registry) Statuses(workspace string) ([]Status, error) {
	states, err := r.States(workspace)
	if err != nil {
		return nil, err
	}
	trusted := r.projectTrusted()
	out := make([]Status, 0, len(states))
	for _, state := range states {
		if !state.Running {
			continue
		}
		srv := r.tracked(workspace, state.ID)
		if srv == nil {
			continue
		}
		out = append(out, srv.status(trusted))
	}
	return out, nil
}

// States lists the configured servers of a workspace with what they would need to start. It creates
// no process and no lifecycle entry that outlives the call.
func (r *Registry) States(workspace string) ([]ServerState, error) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	trusted := r.projectTrusted()
	// Snapshot under the lock; the requirements do filesystem and environment lookups (PATH, sockets)
	// and must not run while every other registry operation waits.
	type snapshot struct {
		cfg     Config
		srv     *server
		running bool
		tools   int
		lastErr string
	}
	r.mu.Lock()
	configs := r.effectiveConfigsLocked(trusted)
	snapshots := make([]snapshot, 0, len(configs))
	for _, cfg := range configs {
		entry := snapshot{cfg: cfg, srv: r.servers[instanceKey(workspace, cfg.ID)]}
		if entry.srv != nil {
			entry.srv.mu.Lock()
			entry.running = entry.srv.current != nil
			entry.tools = len(entry.srv.catalog)
			entry.lastErr = entry.srv.lastErr
			entry.srv.mu.Unlock()
		}
		snapshots = append(snapshots, entry)
	}
	r.mu.Unlock()

	out := make([]ServerState, 0, len(snapshots))
	for _, entry := range snapshots {
		cfg := entry.cfg
		state := ServerState{
			ID:             cfg.ID,
			Name:           cfg.Name,
			Transport:      cfg.transport(),
			Scope:          cfg.Scope,
			Enabled:        !cfg.Disabled,
			Running:        entry.running,
			BlockedByTrust: cfg.BlockedByTrust,
			Tools:          entry.tools,
			LastError:      entry.lastErr,
		}
		if entry.srv != nil {
			state.Requirements = entry.srv.configRequirements()
		} else {
			state.Requirements = requirementsFor(cfg, cfg.Scope, cfg.BlockedByTrust)
		}
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// requirementsFor computes the static requirements of a configuration that has no lifecycle entry.
func requirementsFor(cfg Config, scope string, blocked bool) ConfigRequirements {
	requirements := ConfigRequirements{NeedsTrust: scope == ScopeProject}
	requirements.MissingEnv = cfg.MissingEnv()
	switch cfg.transport() {
	case TransportStdio:
		if _, err := exec.LookPath(cfg.Command); err != nil {
			requirements.MissingBinary = cfg.Command
		}
	case TransportHTTP:
		// A Unix socket endpoint is a local service: its absence is reported without connecting.
		if raw, err := expandEnv(cfg.URL); err == nil && strings.HasPrefix(raw, "unix://") {
			socket := strings.TrimPrefix(raw, "unix://")
			if _, statErr := os.Stat(socket); statErr != nil {
				requirements.MissingEndpoint = socket
			}
		}
	}
	return requirements
}

// tracked returns the lifecycle entry of a server when one exists.
func (r *Registry) tracked(workspace, id string) *server {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.servers[instanceKey(workspace, id)]
}

func (r *Registry) RefreshTools(ctx context.Context, id, workspace string) ([]Tool, error) {
	srv, err := r.server(id, workspace)
	if err != nil {
		return nil, err
	}
	return srv.refreshTools(ctx)
}

// AgentTools lists the tools runs may use, without starting any server: a running server reports its
// live catalog, a stopped one the catalog a previous start recorded. A server whose configuration
// lists Include exposes only those tools.
func (r *Registry) AgentTools(workspace string) ([]AgentTool, error) {
	workspace, err := r.runtimeWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	trusted := r.projectTrusted()
	r.mu.Lock()
	configs := r.effectiveConfigsLocked(trusted)
	servers := make([]*server, 0, len(configs))
	resolved := make([]Config, 0, len(configs))
	for _, cfg := range configs {
		if cfg.Disabled || (cfg.Scope == ScopeProject && !trusted) {
			continue
		}
		servers = append(servers, r.serverLocked(cfg, cfg.Scope, workspace))
		resolved = append(resolved, cfg)
	}
	r.mu.Unlock()

	out := make([]AgentTool, 0)
	seen := map[string]bool{}
	for index, srv := range servers {
		cfg := resolved[index]
		tools, source := srv.tools()
		for _, tool := range tools {
			if !cfg.included(tool.Name) {
				continue
			}
			name := agentToolName(cfg.ID, tool.Name)
			if seen[name] {
				continue
			}
			seen[name] = true
			permission, permissionSource := effectivePermission(cfg, cfg.Scope, trusted, tool)
			out = append(out, AgentTool{
				Name:             name,
				Description:      tool.Description,
				InputSchema:      tool.InputSchema,
				ServerID:         cfg.ID,
				ToolName:         tool.Name,
				ReadOnly:         permission == PermissionRead,
				Permission:       permission,
				PermissionSource: permissionSource,
				Scope:            cfg.Scope,
				ToolSource:       source,
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

// CallTool calls one tool of one server, starting the server on demand. A failure is returned as an
// error the caller turns into a tool result; it never takes the registry down.
func (r *Registry) CallTool(ctx context.Context, workspace, serverID, toolName string, args map[string]any) (CallToolResult, error) {
	return r.CallToolForSession(ctx, workspace, serverID, toolName, "", args)
}

// CallToolForSession is CallTool with the LumenCortex session that drives the call, which a server
// configured with a Session binding receives instead of a model-supplied id.
func (r *Registry) CallToolForSession(ctx context.Context, workspace, serverID, toolName, sessionID string, args map[string]any) (CallToolResult, error) {
	srv, err := r.server(serverID, workspace)
	if err != nil {
		return CallToolResult{}, err
	}
	return srv.callTool(ctx, toolName, sessionID, args)
}

func (r *Registry) Close() error {
	r.mu.Lock()
	servers := r.servers
	r.servers = make(map[string]*server)
	r.mu.Unlock()
	var firstErr error
	for _, srv := range servers {
		if err := srv.stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
		if !serverIDPattern.MatchString(cfg.ID) {
			return nil, fmt.Errorf("invalid mcp server configuration: %s", id)
		}
		if cfg.ProtocolMode == "" {
			cfg.ProtocolMode = ModeLegacy
		}
		if err := validateConfig(cfg); err != nil {
			return nil, err
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

// AgentToolPrefix is the namespace a server's tool names share: "mcp__<server>__". It lets a host turn a
// model-visible name back into a server and a tool name.
func AgentToolPrefix(serverID string) string {
	return "mcp__" + sanitizeToolPart(serverID, 16) + "__"
}

// AgentToolName is the model-visible name of a server tool, exported so a registry view can report the
// name a run would see without duplicating the rule.
func AgentToolName(serverID, toolName string) string { return agentToolName(serverID, toolName) }

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

// catalogFingerprint identifies the definition a recorded catalog may serve, so a catalog recorded for
// one server definition is not reused for another. For every scope it covers the spawn- and
// connection-relevant part — what sameSpawnConfig compares to decide that a running process no longer
// describes its entry, minus the limits — with env and headers hashed as configured: an {env:NAME}
// reference, never the value it resolves to. For a project entry it also covers the exposure — the
// include list, the tools overrides and disabled — because the project mcp.json is a file a run can
// write: an exposure edit that did not come through the registry withdraws the catalog until the host
// starts the server again, and one that did is re-recorded by rerecordCatalogs (ADR-0013, ADR-0021 D3).
// A global definition without env or headers keeps the fingerprint it had before they were covered.
func catalogFingerprint(cfg Config, scope string) string {
	digest := sha256.New()
	fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%v\x00%v", cfg.transport(), cfg.Command, cfg.URL, cfg.Args, cfg.ProtocolMode)
	for _, section := range []struct {
		name   string
		values map[string]string
	}{{"env", cfg.Env}, {"headers", cfg.Headers}} {
		for _, key := range slices.Sorted(maps.Keys(section.values)) {
			fmt.Fprintf(digest, "\x00%s %q=%q", section.name, key, section.values[key])
		}
	}
	if scope == ScopeProject {
		fmt.Fprintf(digest, "\x00disabled %t", cfg.Disabled)
		for _, name := range slices.Sorted(slices.Values(cfg.Include)) {
			fmt.Fprintf(digest, "\x00include %q", name)
		}
		for _, name := range slices.Sorted(maps.Keys(cfg.Tools)) {
			fmt.Fprintf(digest, "\x00tool %q=%q", name, cfg.Tools[name].Permission)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))[:16]
}

// MissingCatalogs lists the enabled servers of a workspace whose tool catalog was never discovered, so
// a run can start exactly those and no other. A server that is disabled, blocked by trust or missing a
// requirement is not listed: it could not start anyway. It starts nothing.
func (r *Registry) MissingCatalogs(workspace string) []string {
	states, err := r.States(workspace)
	if err != nil {
		return nil
	}
	var missing []string
	for _, state := range states {
		if !state.Enabled || state.BlockedByTrust || !state.Requirements.Empty() {
			continue
		}
		if srv := r.tracked(workspace, state.ID); srv != nil {
			if tools, _ := srv.tools(); len(tools) > 0 {
				continue
			}
			if srv.recentlyFailed() {
				continue
			}
		}
		missing = append(missing, state.ID)
	}
	sort.Strings(missing)
	return missing
}
