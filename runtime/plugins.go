package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/pluginapi"
)

// Plugin platform (ADR-0012). Core owns registration, lifecycle, enablement, capability lookup and
// diagnostics. A plugin reaches Core only through pluginapi.Host; Core recovers a panic in any
// plugin call, marks that plugin failed and keeps running without it.

var (
	ErrPluginRegistryStarted = errors.New("plugin registry has already started")
	ErrPluginAPIVersion      = errors.New("unsupported plugin API version")
	ErrPluginCapability      = errors.New("unsupported plugin capability")
	ErrPluginNotFound        = errors.New("plugin not found")
	ErrPluginStateInvalid    = errors.New("plugin state file is invalid")
)

// PluginStateFileName is the global file that records disabled plugins.
const PluginStateFileName = "plugins.json"

// maxPluginStateBytes bounds the plugin state file.
const maxPluginStateBytes = 64 << 10

var pluginIDPattern = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,63}$`)

// knownCapabilities are the capabilities this Core understands. A manifest that declares another
// one was built for a different contract and is refused.
var knownCapabilities = map[string]bool{
	pluginapi.CapabilitySessionImport:       true,
	pluginapi.CapabilityEvents:              true,
	pluginapi.CapabilityBeforeSessionImport: true,
}

// PluginState is a plugin's lifecycle state.
type PluginState string

const (
	PluginRegistered PluginState = "registered"
	PluginRunning    PluginState = "running"
	PluginDisabled   PluginState = "disabled"
	PluginStopped    PluginState = "stopped"
	PluginFailed     PluginState = "failed"
)

// PluginInfo is one plugin as diagnostics show it.
type PluginInfo struct {
	Manifest pluginapi.Manifest `json:"manifest"`
	// Builtin plugins ship with Core; the others were supplied by the host through Options.Plugins.
	Builtin bool        `json:"builtin"`
	Enabled bool        `json:"enabled"`
	State   PluginState `json:"state"`
	// Error is the last start, stop, hook or panic failure; Failures counts them since Open.
	Error         string     `json:"error,omitempty"`
	Failures      int        `json:"failures,omitempty"`
	LastFailureAt *time.Time `json:"lastFailureAt,omitempty"`
	// ImportSource describes the source a session-import plugin provides.
	ImportSource *pluginapi.ImportSourceInfo `json:"importSource,omitempty"`
}

// PluginCatalog is every registered plugin, by id, with the enablement file they were read with.
type PluginCatalog struct {
	APIVersion int          `json:"apiVersion"`
	Plugins    []PluginInfo `json:"plugins"`
	StatePath  string       `json:"statePath,omitempty"`
	// StateError is set when the enablement file cannot be read; every plugin then counts as enabled
	// and changing enablement is refused until the file is fixed or removed.
	StateError string `json:"stateError,omitempty"`
}

type pluginEntry struct {
	plugin        pluginapi.Plugin
	manifest      pluginapi.Manifest
	builtin       bool
	source        *pluginapi.ImportSourceInfo
	state         PluginState
	lastErr       string
	failures      int
	lastFailureAt time.Time
	host          *pluginHost
}

// PluginRegistry owns deterministic registration and lifecycle of in-process extensions.
type PluginRegistry struct {
	mu          sync.RWMutex
	lifecycleMu sync.Mutex
	plugins     map[string]*pluginEntry
	started     bool
	statePath   string
	disabled    map[string]bool
	stateErr    error
	events      *eventBus
	hooks       *hookDispatcher
}

func newPluginRegistry(events *eventBus, hooks *hookDispatcher, statePath string) *PluginRegistry {
	r := &PluginRegistry{plugins: map[string]*pluginEntry{}, events: events, hooks: hooks, statePath: statePath}
	hooks.onFailure = func(pluginID string, err error, panicked bool) {
		if panicked {
			r.fail(pluginID, err)
			return
		}
		r.noteFailure(pluginID, err)
	}
	return r
}

func validatePluginManifest(m pluginapi.Manifest) (pluginapi.Manifest, error) {
	m.ID = strings.TrimSpace(m.ID)
	m.Name = strings.TrimSpace(m.Name)
	m.Version = strings.TrimSpace(m.Version)
	m.Description = strings.TrimSpace(m.Description)
	m.Capabilities = slices.Clone(m.Capabilities)
	if !pluginIDPattern.MatchString(m.ID) {
		return pluginapi.Manifest{}, fmt.Errorf("plugin id %q must match [a-z][a-z0-9._-] and be at most 64 bytes", m.ID)
	}
	if m.Name == "" || m.Version == "" {
		return pluginapi.Manifest{}, fmt.Errorf("plugin %s: name and version are required", m.ID)
	}
	if m.APIVersion != pluginapi.APIVersion {
		return pluginapi.Manifest{}, fmt.Errorf("%w: plugin %s requires %d, Core implements %d", ErrPluginAPIVersion, m.ID, m.APIVersion, pluginapi.APIVersion)
	}
	seen := make(map[string]bool, len(m.Capabilities))
	for i, capability := range m.Capabilities {
		capability = strings.TrimSpace(capability)
		m.Capabilities[i] = capability
		if !knownCapabilities[capability] {
			return pluginapi.Manifest{}, fmt.Errorf("%w %q in plugin %s", ErrPluginCapability, capability, m.ID)
		}
		if seen[capability] {
			return pluginapi.Manifest{}, fmt.Errorf("duplicate capability %q in plugin %s", capability, m.ID)
		}
		seen[capability] = true
	}
	sort.Strings(m.Capabilities)
	return m, nil
}

// Register adds a plugin before the registry starts. The manifest must be valid, the id unused,
// and every declared capability implemented by the value.
func (r *PluginRegistry) Register(plugin pluginapi.Plugin, builtin bool) error {
	if plugin == nil {
		return errors.New("plugin is required")
	}
	var manifest pluginapi.Manifest
	if err := recoverCall(func() error { manifest = plugin.Manifest(); return nil }); err != nil {
		return fmt.Errorf("plugin manifest: %w", err)
	}
	manifest, err := validatePluginManifest(manifest)
	if err != nil {
		return err
	}
	entry := &pluginEntry{plugin: plugin, manifest: manifest, builtin: builtin, state: PluginRegistered}
	if slices.Contains(manifest.Capabilities, pluginapi.CapabilitySessionImport) {
		source, ok := plugin.(pluginapi.SessionImportSource)
		if !ok {
			return fmt.Errorf("plugin %s declares %s but does not implement pluginapi.SessionImportSource", manifest.ID, pluginapi.CapabilitySessionImport)
		}
		var info pluginapi.ImportSourceInfo
		if err := recoverCall(func() error { info = source.ImportSource(); return nil }); err != nil {
			return fmt.Errorf("plugin %s import source: %w", manifest.ID, err)
		}
		if !pluginIDPattern.MatchString(info.ID) || strings.TrimSpace(info.Name) == "" || info.Converter < 1 {
			return fmt.Errorf("plugin %s: import source needs an id matching [a-z][a-z0-9._-], a name and a converter version >= 1", manifest.ID)
		}
		entry.source = &info
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return ErrPluginRegistryStarted
	}
	if _, exists := r.plugins[manifest.ID]; exists {
		return fmt.Errorf("plugin already registered: %s", manifest.ID)
	}
	if entry.source != nil {
		for _, other := range r.plugins {
			if other.source != nil && other.source.ID == entry.source.ID {
				return fmt.Errorf("plugin %s: import source %q is already provided by %s", manifest.ID, entry.source.ID, other.manifest.ID)
			}
		}
	}
	r.plugins[manifest.ID] = entry
	return nil
}

// StartAll reads the enablement file and starts every enabled plugin in id order. A plugin that
// fails to start is marked failed and the others still start: one broken extension never stops
// Core from opening.
func (r *PluginRegistry) StartAll(ctx context.Context) {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return
	}
	r.started = true
	r.disabled, r.stateErr = readPluginState(r.statePath)
	ids := r.sortedIDsLocked()
	r.mu.Unlock()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		r.mu.RLock()
		disabled := r.disabled[id]
		r.mu.RUnlock()
		if disabled {
			r.setState(id, PluginDisabled)
			continue
		}
		_ = r.start(ctx, id)
	}
}

// start runs one plugin's Start with a fresh host. The caller holds lifecycleMu.
func (r *PluginRegistry) start(ctx context.Context, id string) error {
	r.mu.RLock()
	entry := r.plugins[id]
	r.mu.RUnlock()
	capabilities := make(map[string]bool, len(entry.manifest.Capabilities))
	for _, capability := range entry.manifest.Capabilities {
		capabilities[capability] = true
	}
	host := &pluginHost{pluginID: id, capabilities: capabilities, events: r.events, hooks: r.hooks}
	if err := recoverCall(func() error { return entry.plugin.Start(ctx, host) }); err != nil {
		host.close()
		r.hooks.removePlugin(id)
		r.fail(id, fmt.Errorf("start: %w", err))
		return fmt.Errorf("start plugin %s: %w", id, err)
	}
	r.mu.Lock()
	entry.host = host
	entry.state = PluginRunning
	r.mu.Unlock()
	return nil
}

// stop runs one plugin's Stop and removes what its host registered. The caller holds lifecycleMu.
func (r *PluginRegistry) stop(ctx context.Context, id string, next PluginState) error {
	r.mu.Lock()
	entry := r.plugins[id]
	host := entry.host
	entry.host = nil
	entry.state = next
	r.mu.Unlock()
	if host != nil {
		host.close()
	}
	r.hooks.removePlugin(id)
	if err := recoverCall(func() error { return entry.plugin.Stop(ctx) }); err != nil {
		r.fail(id, fmt.Errorf("stop: %w", err))
		return fmt.Errorf("stop plugin %s: %w", id, err)
	}
	return nil
}

// StopAll stops running plugins in reverse id order.
func (r *PluginRegistry) StopAll(ctx context.Context) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.mu.RLock()
	ids := r.sortedIDsLocked()
	r.mu.RUnlock()
	var joined error
	for i := len(ids) - 1; i >= 0; i-- {
		r.mu.RLock()
		running := r.plugins[ids[i]].state == PluginRunning
		r.mu.RUnlock()
		if running {
			joined = errors.Join(joined, r.stop(ctx, ids[i], PluginStopped))
		}
	}
	return joined
}

// SetEnabled records the plugin's enablement in the global state file and applies it now: an
// enabled plugin is started (a start failure is returned and the plugin stays enabled but failed,
// so the next Open retries it), a disabled one is stopped.
func (r *PluginRegistry) SetEnabled(ctx context.Context, id string, enabled bool) error {
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	r.mu.Lock()
	entry := r.plugins[id]
	if entry == nil {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrPluginNotFound, id)
	}
	if r.stateErr != nil {
		r.mu.Unlock()
		return fmt.Errorf("%w: %v", ErrPluginStateInvalid, r.stateErr)
	}
	disabled := make(map[string]bool, len(r.disabled)+1)
	for k, v := range r.disabled {
		disabled[k] = v
	}
	if enabled {
		delete(disabled, id)
	} else {
		disabled[id] = true
	}
	if err := writePluginState(r.statePath, disabled); err != nil {
		r.mu.Unlock()
		return err
	}
	r.disabled = disabled
	state := entry.state
	r.mu.Unlock()

	switch {
	case enabled && state != PluginRunning:
		return r.start(ctx, id)
	case !enabled && state == PluginRunning:
		return r.stop(ctx, id, PluginDisabled)
	case !enabled:
		r.setState(id, PluginDisabled)
	}
	return nil
}

func (r *PluginRegistry) setState(id string, state PluginState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry := r.plugins[id]; entry != nil {
		entry.state = state
	}
}

// fail records a plugin failure, marks the plugin failed, drops what its host registered and
// publishes plugin.failed. Stop is not called: the plugin is assumed broken.
func (r *PluginRegistry) fail(id string, err error) {
	r.mu.Lock()
	entry := r.plugins[id]
	if entry == nil {
		r.mu.Unlock()
		return
	}
	entry.state = PluginFailed
	entry.lastErr = err.Error()
	entry.failures++
	entry.lastFailureAt = time.Now().UTC()
	host := entry.host
	entry.host = nil
	r.mu.Unlock()
	// A failed plugin keeps nothing registered: its subscriptions close and its hooks stop running.
	if host != nil {
		host.close()
	}
	r.hooks.removePlugin(id)
	r.publishFailure(id, err)
}

// noteFailure records a failure that leaves the plugin running (a fail-open hook error).
func (r *PluginRegistry) noteFailure(id string, err error) {
	r.mu.Lock()
	if entry := r.plugins[id]; entry != nil {
		entry.lastErr = err.Error()
		entry.failures++
		entry.lastFailureAt = time.Now().UTC()
	}
	r.mu.Unlock()
}

func (r *PluginRegistry) publishFailure(id string, err error) {
	if r.events != nil {
		r.events.publish("plugin.failed", "", map[string]any{"pluginId": id, "error": err.Error()})
	}
}

func (r *PluginRegistry) sortedIDsLocked() []string {
	ids := make([]string, 0, len(r.plugins))
	for id := range r.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Catalog lists every registered plugin by id.
func (r *PluginRegistry) Catalog() PluginCatalog {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := PluginCatalog{APIVersion: pluginapi.APIVersion, Plugins: []PluginInfo{}, StatePath: r.statePath}
	if r.stateErr != nil {
		out.StateError = r.stateErr.Error()
	}
	for _, id := range r.sortedIDsLocked() {
		entry := r.plugins[id]
		manifest := entry.manifest
		manifest.Capabilities = slices.Clone(manifest.Capabilities)
		info := PluginInfo{
			Manifest: manifest, Builtin: entry.builtin, Enabled: !r.disabled[id], State: entry.state,
			Error: entry.lastErr, Failures: entry.failures,
		}
		if !entry.lastFailureAt.IsZero() {
			at := entry.lastFailureAt
			info.LastFailureAt = &at
		}
		if entry.source != nil {
			source := *entry.source
			info.ImportSource = &source
		}
		out.Plugins = append(out.Plugins, info)
	}
	return out
}

// importSources returns the running session import sources by source id order.
func (r *PluginRegistry) importSources() []registeredSource {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []registeredSource
	for _, id := range r.sortedIDsLocked() {
		entry := r.plugins[id]
		if entry.state != PluginRunning || entry.source == nil {
			continue
		}
		out = append(out, registeredSource{pluginID: id, info: *entry.source, source: entry.plugin.(pluginapi.SessionImportSource)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].info.ID < out[j].info.ID })
	return out
}

type registeredSource struct {
	pluginID string
	info     pluginapi.ImportSourceInfo
	source   pluginapi.SessionImportSource
}

// call runs fn, a call into plugin code, turning a panic into an error that marks the plugin failed.
func (r *PluginRegistry) call(pluginID string, fn func() error) error {
	var panicked bool
	err := recoverCallReport(fn, &panicked)
	if panicked {
		r.fail(pluginID, err)
	}
	return err
}

func recoverCall(fn func() error) error {
	var panicked bool
	return recoverCallReport(fn, &panicked)
}

func recoverCallReport(fn func() error, panicked *bool) (err error) {
	defer func() {
		if v := recover(); v != nil {
			*panicked = true
			err = fmt.Errorf("plugin panic: %v", v)
		}
	}()
	return fn()
}

type pluginStateFile struct {
	Version  int      `json:"version"`
	Disabled []string `json:"disabled"`
}

func readPluginState(path string) (map[string]bool, error) {
	disabled := map[string]bool{}
	if path == "" {
		return disabled, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return disabled, nil
	}
	if err != nil {
		return disabled, err
	}
	if len(raw) > maxPluginStateBytes {
		return disabled, fmt.Errorf("%s exceeds %d bytes", path, maxPluginStateBytes)
	}
	var file pluginStateFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return disabled, fmt.Errorf("%s: %w", path, err)
	}
	if file.Version != 1 {
		return disabled, fmt.Errorf("%s: unsupported version %d", path, file.Version)
	}
	for _, id := range file.Disabled {
		disabled[id] = true
	}
	return disabled, nil
}

func writePluginState(path string, disabled map[string]bool) error {
	if path == "" {
		return errors.New("plugin state path is unavailable")
	}
	file := pluginStateFile{Version: 1, Disabled: []string{}}
	for id, off := range disabled {
		if off {
			file.Disabled = append(file.Disabled, id)
		}
	}
	sort.Strings(file.Disabled)
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pluginHost is the Host one started plugin receives; it is closed when the plugin stops.
type pluginHost struct {
	pluginID     string
	capabilities map[string]bool
	events       *eventBus
	hooks        *hookDispatcher
	mu           sync.Mutex
	closed       bool
	cleanups     []func()
}

func (h *pluginHost) SubscribeEvents(buffer int) (<-chan Event, func(), error) {
	if !h.capabilities[pluginapi.CapabilityEvents] {
		return nil, func() {}, fmt.Errorf("plugin %s did not declare capability %s", h.pluginID, pluginapi.CapabilityEvents)
	}
	closedStream := func() (<-chan Event, func(), error) {
		ch := make(chan Event)
		close(ch)
		return ch, func() {}, nil
	}
	if h.events == nil {
		return closedStream()
	}
	ch, cancel := h.events.subscribe(buffer)
	var once sync.Once
	stop := func() { once.Do(cancel) }
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		stop()
		return closedStream()
	}
	h.cleanups = append(h.cleanups, stop)
	return ch, stop, nil
}

func (h *pluginHost) RegisterBeforeSessionImportHook(hook pluginapi.BeforeSessionImportHook) error {
	if !h.capabilities[pluginapi.CapabilityBeforeSessionImport] {
		return fmt.Errorf("plugin %s did not declare capability %s", h.pluginID, pluginapi.CapabilityBeforeSessionImport)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("plugin host is closed")
	}
	return h.hooks.registerBeforeSessionImport(h.pluginID, hook)
}

func (h *pluginHost) close() {
	h.mu.Lock()
	h.closed = true
	cleanups := h.cleanups
	h.cleanups = nil
	h.mu.Unlock()
	for _, cleanup := range cleanups {
		cleanup()
	}
}

type beforeSessionImportEntry struct {
	pluginID string
	sequence uint64
	hook     pluginapi.BeforeSessionImportHook
}

// hookDispatcher runs the explicit hook points. Failures are reported to onFailure (the registry)
// and published as plugin.hook.failed with no error text from the request.
type hookDispatcher struct {
	mu        sync.RWMutex
	nextSeq   uint64
	hooks     []beforeSessionImportEntry
	events    *eventBus
	onFailure func(pluginID string, err error, panicked bool)
}

func newHookDispatcher(events *eventBus) *hookDispatcher {
	return &hookDispatcher{events: events}
}

func (d *hookDispatcher) registerBeforeSessionImport(pluginID string, hook pluginapi.BeforeSessionImportHook) error {
	if hook.Handle == nil {
		return errors.New("hook handler is required")
	}
	if hook.Timeout <= 0 {
		hook.Timeout = pluginapi.DefaultHookTimeout
	}
	if hook.Timeout > pluginapi.MaxHookTimeout {
		return fmt.Errorf("hook timeout cannot exceed %s", pluginapi.MaxHookTimeout)
	}
	if hook.Policy != pluginapi.HookFailClosed && hook.Policy != pluginapi.HookFailOpen {
		return errors.New("hook failure policy must be fail-closed or fail-open")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextSeq++
	d.hooks = append(d.hooks, beforeSessionImportEntry{pluginID: pluginID, sequence: d.nextSeq, hook: hook})
	return nil
}

func (d *hookDispatcher) removePlugin(pluginID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hooks = slices.DeleteFunc(d.hooks, func(h beforeSessionImportEntry) bool { return h.pluginID == pluginID })
}

// beforeSessionImport runs the hooks in order. A fail-closed failure refuses the import; a
// fail-open one is recorded and the next hook runs. Caller cancellation always aborts. A hook that
// ignores its context keeps running in its goroutine after the timeout: in-process code cannot be
// killed.
func (d *hookDispatcher) beforeSessionImport(ctx context.Context, request pluginapi.BeforeSessionImportRequest) error {
	d.mu.RLock()
	hooks := slices.Clone(d.hooks)
	d.mu.RUnlock()
	sort.SliceStable(hooks, func(i, j int) bool {
		a, b := hooks[i], hooks[j]
		if a.hook.Priority != b.hook.Priority {
			return a.hook.Priority < b.hook.Priority
		}
		if a.pluginID != b.pluginID {
			return a.pluginID < b.pluginID
		}
		return a.sequence < b.sequence
	})
	for _, entry := range hooks {
		if err := ctx.Err(); err != nil {
			return err
		}
		hookCtx, cancel := context.WithTimeout(ctx, entry.hook.Timeout)
		type outcome struct {
			err      error
			panicked bool
		}
		done := make(chan outcome, 1)
		go func(handle func(context.Context, pluginapi.BeforeSessionImportRequest) error) {
			var o outcome
			o.err = recoverCallReport(func() error { return handle(hookCtx, request) }, &o.panicked)
			done <- o
		}(entry.hook.Handle)
		var result outcome
		select {
		case result = <-done:
		case <-hookCtx.Done():
			result.err = hookCtx.Err()
		}
		cancel()
		if result.err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		timedOut := errors.Is(result.err, context.DeadlineExceeded)
		// An error a fail-closed hook returns in time is its decision, not a failure.
		refused := entry.hook.Policy == pluginapi.HookFailClosed && !timedOut && !result.panicked
		if !refused {
			if d.onFailure != nil {
				d.onFailure(entry.pluginID, fmt.Errorf("before-session-import hook: %w", result.err), result.panicked)
			}
			if d.events != nil {
				d.events.publish("plugin.hook.failed", "", map[string]any{
					"pluginId": entry.pluginID, "hook": "before_session_import", "policy": entry.hook.Policy,
					"timedOut": timedOut, "panicked": result.panicked,
				})
			}
		}
		if entry.hook.Policy == pluginapi.HookFailClosed {
			return fmt.Errorf("plugin %s refused the import: %w", entry.pluginID, result.err)
		}
	}
	return nil
}
