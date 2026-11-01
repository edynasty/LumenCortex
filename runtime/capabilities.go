package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/mcp"
	"github.com/edynasty/LumenCortex/internal/mcptemplates"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// The capability registry (ADR-0021) is the one view of everything a run could be offered: Core's
// built-in tools, the optional language-server tools, and the tools of every configured MCP server
// (remote or local). It is computed from configuration, the environment and the tool catalogs a
// previous start recorded, so listing it never starts a process.
//
// Every entry goes through the same execution path as before — the permission gate, the tool
// deadline and the tool events — because the registry describes tools, it does not run them.

// Sources a capability can come from.
const (
	// SourceBuiltin tools are compiled into Core.
	SourceBuiltin = "builtin"
	// SourceCoreOptional tools need something attached: the language-server tools (LSP).
	SourceCoreOptional = "core-optional"
	// SourceMCP tools come from a remote MCP server.
	SourceMCP = "mcp"
	// SourceLocalAdapter tools come from a local process or a local socket (a stdio MCP server, a
	// Unix-socket daemon): the third-party code runs beside Core in its own process.
	SourceLocalAdapter = "local-adapter"
)

// Availability states. They are computed without starting anything.
const (
	// AvailabilityAvailable: the tool can be offered now.
	AvailabilityAvailable = "available"
	// AvailabilityDisabled: the user disabled the entry.
	AvailabilityDisabled = "disabled"
	// AvailabilityNeedsConfiguration: a referenced environment variable is unset, a binary is missing
	// or a local endpoint is not there.
	AvailabilityNeedsConfiguration = "needs-configuration"
	// AvailabilityUntrusted: the entry comes from the project configuration of an untrusted workspace.
	AvailabilityUntrusted = "untrusted"
	// AvailabilityError: the last start or call failed; LastError says why.
	AvailabilityError = "error"
)

// CapabilityAvailability says whether a tool can be offered now, and why not.
type CapabilityAvailability struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// CapabilityRequirements is what an entry needs before it can be used, from configuration and the
// environment only.
type CapabilityRequirements struct {
	MissingEnv       []string `json:"missingEnv,omitempty"`
	MissingBinary    string   `json:"missingBinary,omitempty"`
	MissingEndpoint  string   `json:"missingEndpoint,omitempty"`
	NeedsTrust       bool     `json:"needsTrust,omitempty"`
	NeedsLanguageSrv bool     `json:"needsLanguageServer,omitempty"`
}

// Empty reports whether nothing is missing.
func (r CapabilityRequirements) Empty() bool {
	return len(r.MissingEnv) == 0 && r.MissingBinary == "" && r.MissingEndpoint == "" && !r.NeedsTrust && !r.NeedsLanguageSrv
}

// CapabilityConstraints are the limits the tool runs under: the same ones the runtime enforces.
type CapabilityConstraints struct {
	// TimeoutSeconds bounds one call; 0 means the runtime default for the source.
	TimeoutSeconds int `json:"timeoutSeconds,omitempty"`
	// IdleTimeoutSeconds stops the providing process after that long without a call.
	IdleTimeoutSeconds int `json:"idleTimeoutSeconds,omitempty"`
	MaxConcurrentCalls int `json:"maxConcurrentCalls,omitempty"`
	MaxResultBytes     int `json:"maxResultBytes,omitempty"`
	// MutatesWorkspace reports that a call may change the workspace.
	MutatesWorkspace bool `json:"mutatesWorkspace"`
	// RequiresApproval reports that the call needs the permission gate to admit it (exec).
	RequiresApproval bool `json:"requiresApproval"`
}

// Capability is one tool a run could be offered, with where it comes from and whether it can be used
// now. Several capabilities may share one Capability value: the same job can have several sources.
type Capability struct {
	ToolID      string `json:"toolId"`
	DisplayName string `json:"displayName"`
	// Source is builtin, core-optional, mcp or local-adapter.
	Source string `json:"source"`
	// Provider names the server, template or subsystem the tool comes from; "" for built-in tools.
	Provider string `json:"provider,omitempty"`
	// Capability is the semantic job the tool does ("repo.read", "web.search", "browser.control", …).
	Capability   string                 `json:"capability"`
	Permission   string                 `json:"permission"`
	Enabled      bool                   `json:"enabled"`
	Availability CapabilityAvailability `json:"availability"`
	// ExecutionConstraints and ConfigRequirements say what a call costs and what is missing.
	ExecutionConstraints CapabilityConstraints  `json:"executionConstraints"`
	ConfigRequirements   CapabilityRequirements `json:"configRequirements"`
	LastError            string                 `json:"lastError,omitempty"`
	// DisabledReason names what switched the entry off: "user", "denylist" or "trust".
	DisabledReason string `json:"disabledReason,omitempty"`
	// Template is the MCP template the entry came from, when one was used.
	Template string `json:"template,omitempty"`
}

// CapabilityQuery narrows the registry view.
type CapabilityQuery struct {
	// SessionID resolves the workspace a session runs in; empty uses the Engine workspace.
	SessionID string
	// Denylist names tool ids or providers the caller does not want; a match reports Enabled=false
	// with DisabledReason "denylist" rather than removing the entry, so the view stays complete.
	Denylist []string
	// Capability keeps only entries of that semantic capability.
	Capability string
}

// Capabilities lists every tool a run could be offered in the query's workspace, without starting a
// server or a language server. It is the single view a host shows and a run is drawn from.
func (e *Engine) Capabilities(ctx context.Context, query CapabilityQuery) ([]Capability, error) {
	workspace, err := e.lspWorkspace(ctx, query.SessionID)
	if err != nil {
		return nil, err
	}
	// A host list may still name a tool by its previous name (shell); it means the tool that replaced it,
	// as it does for the run (protocol.CanonicalToolName).
	denied := map[string]bool{}
	for _, name := range query.Denylist {
		denied[protocol.CanonicalToolName(strings.TrimSpace(name))] = true
	}
	out := make([]Capability, 0, 32)
	out = append(out, e.builtinCapabilities(denied)...)
	out = append(out, e.lspCapabilities(workspace, denied)...)
	mcpCapabilities, err := e.mcpCapabilities(workspace, denied)
	if err != nil {
		return nil, err
	}
	out = append(out, mcpCapabilities...)
	if query.Capability != "" {
		filtered := out[:0]
		for _, capability := range out {
			if capability.Capability == query.Capability {
				filtered = append(filtered, capability)
			}
		}
		out = filtered
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source != out[j].Source {
			return out[i].Source < out[j].Source
		}
		return out[i].ToolID < out[j].ToolID
	})
	return out, nil
}

// builtinCapabilities lists the compiled-in tools. They are always available; the run's policy decides
// whether a step sees one.
func (e *Engine) builtinCapabilities(denied map[string]bool) []Capability {
	specs := e.builtinSpecs()
	out := make([]Capability, 0, len(specs))
	shellTimeout := 0
	if limits := e.executionLimits(); limits.ShellTimeoutSeconds > 0 {
		shellTimeout = limits.ShellTimeoutSeconds
	}
	for _, spec := range specs {
		constraints := CapabilityConstraints{
			MutatesWorkspace: spec.MutatesWorkspace,
			RequiresApproval: spec.Permission == "exec",
		}
		if spec.Name == protocol.ToolBash {
			constraints.TimeoutSeconds = shellTimeout
		}
		capability := Capability{
			ToolID:               spec.Name,
			DisplayName:          displayName(spec.Name),
			Source:               SourceBuiltin,
			Capability:           builtinCapability(spec.Name),
			Permission:           spec.Permission,
			Enabled:              true,
			Availability:         CapabilityAvailability{State: AvailabilityAvailable},
			ExecutionConstraints: constraints,
		}
		if denied[spec.Name] {
			capability.Enabled = false
			capability.DisabledReason = "denylist"
		}
		out = append(out, capability)
	}
	return out
}

// lspCapabilities lists the language-server tools. They are core-optional: the specs exist always, a
// run is offered them when a configured server can serve the workspace (its command is on PATH and the
// workspace holds a file or a marker of its language); the server itself starts on the first call.
func (e *Engine) lspCapabilities(workspace string, denied map[string]bool) []Capability {
	available := e.lspAvailable(workspace)
	specs := toolset.LSPSpecs()
	out := make([]Capability, 0, len(specs))
	for _, spec := range specs {
		capability := Capability{
			ToolID:      spec.Name,
			DisplayName: displayName(spec.Name),
			Source:      SourceCoreOptional,
			Provider:    "lsp",
			Capability:  "code.intelligence",
			Permission:  spec.Permission,
			Enabled:     available,
			Availability: CapabilityAvailability{
				State: AvailabilityNeedsConfiguration,
			},
			ExecutionConstraints: CapabilityConstraints{RequiresApproval: spec.Permission == "exec"},
			ConfigRequirements:   CapabilityRequirements{NeedsLanguageSrv: true},
		}
		if available {
			capability.Availability = CapabilityAvailability{State: AvailabilityAvailable}
			capability.ConfigRequirements = CapabilityRequirements{}
		} else {
			capability.Availability.Detail = "no configured language server can serve a file in this workspace (lsp.servers, PATH)"
			capability.DisabledReason = "unavailable"
		}
		if denied[spec.Name] {
			capability.Enabled = false
			capability.DisabledReason = "denylist"
		}
		out = append(out, capability)
	}
	return out
}

// mcpCapabilities lists the tools of every configured server, including a template's expected tools
// before the server was ever started. Nothing here starts a process.
func (e *Engine) mcpCapabilities(workspace string, denied map[string]bool) ([]Capability, error) {
	if e.mcpRegistry == nil {
		return nil, nil
	}
	states, err := e.mcpRegistry.States(workspace)
	if err != nil {
		return nil, err
	}
	configs := e.mcpRegistry.Configs()
	configByID := make(map[string]MCPConfig, len(configs))
	for _, cfg := range configs {
		configByID[cfg.ID] = cfg
	}
	tools, err := e.mcpRegistry.AgentTools(workspace)
	if err != nil {
		return nil, err
	}
	byServer := map[string][]MCPAgentTool{}
	for _, tool := range tools {
		byServer[tool.ServerID] = append(byServer[tool.ServerID], tool)
	}
	out := make([]Capability, 0, len(tools)+len(states))
	for _, state := range states {
		cfg := configByID[state.ID]
		source := mcpSource(cfg)
		capabilityName := mcpCapability(cfg)
		availability := mcpAvailability(state)
		constraints := CapabilityConstraints{
			TimeoutSeconds:     effectiveCallTimeout(cfg),
			IdleTimeoutSeconds: effectiveIdleTimeout(cfg),
			MaxConcurrentCalls: effectiveConcurrency(cfg),
			MaxResultBytes:     effectiveResultBytes(cfg),
		}
		requirements := CapabilityRequirements{
			MissingEnv:      state.Requirements.MissingEnv,
			MissingBinary:   state.Requirements.MissingBinary,
			MissingEndpoint: state.Requirements.MissingEndpoint,
			NeedsTrust:      state.Requirements.NeedsTrust && state.BlockedByTrust,
		}
		base := Capability{
			Source:               source,
			Provider:             state.ID,
			Capability:           capabilityName,
			Enabled:              state.Enabled && !state.BlockedByTrust,
			Availability:         availability,
			ExecutionConstraints: constraints,
			ConfigRequirements:   requirements,
			LastError:            state.LastError,
			Template:             cfg.Template,
		}
		if state.BlockedByTrust {
			base.DisabledReason = "trust"
		} else if !state.Enabled {
			base.DisabledReason = "user"
		}
		listed := byServer[state.ID]
		if len(listed) == 0 {
			// A server whose catalog was never discovered still contributes a row per configured tool,
			// so a host can show what enabling it would offer.
			for _, name := range cfg.Include {
				entry := base
				entry.ToolID = mcp.AgentToolName(state.ID, name)
				entry.DisplayName = name
				entry.Permission = mcp.PermissionExec
				if override, ok := cfg.Tools[name]; ok && override.Permission != "" {
					entry.Permission = override.Permission
				}
				// The name comes from the configured include list; the server has not been started, so
				// its own description and schema are not known yet. A host shows that, not a guess.
				entry.Availability = availability
				if availability.State == AvailabilityAvailable {
					entry.Availability.Detail = "tool list not yet discovered: the server has not been started"
				}
				entry.ExecutionConstraints.RequiresApproval = entry.Permission == "exec"
				entry = applyDenylist(entry, denied)
				out = append(out, entry)
			}
			continue
		}
		for _, tool := range listed {
			entry := base
			entry.ToolID = tool.Name
			entry.DisplayName = tool.ToolName
			entry.Permission = tool.Permission
			entry.ExecutionConstraints.RequiresApproval = tool.Permission == "exec"
			if entry.Availability.State == AvailabilityAvailable && tool.ToolSource == mcp.ToolSourceCache {
				entry.Availability.Detail = "tool list from the catalog an earlier start recorded"
			}
			entry = applyDenylist(entry, denied)
			out = append(out, entry)
		}
	}
	return out, nil
}

func applyDenylist(capability Capability, denied map[string]bool) Capability {
	if denied[capability.ToolID] || (capability.Provider != "" && denied[capability.Provider]) {
		capability.Enabled = false
		capability.DisabledReason = "denylist"
	}
	return capability
}

// mcpSource classifies a server: a local process or socket is a local adapter, a remote endpoint is an
// MCP server. The distinction matters for what a host tells the user about isolation.
func mcpSource(cfg MCPConfig) string {
	if cfg.Transport == "" || cfg.Transport == mcp.TransportStdio {
		return SourceLocalAdapter
	}
	if strings.HasPrefix(strings.TrimSpace(cfg.URL), "unix://") {
		return SourceLocalAdapter
	}
	return SourceMCP
}

// mcpCapability is the semantic capability a server provides: its template's, else a generic one.
func mcpCapability(cfg MCPConfig) string {
	if cfg.Template != "" {
		if template, ok := mcptemplates.Get(cfg.Template); ok && template.Capability != "" {
			return template.Capability
		}
	}
	return "mcp.tools"
}

func mcpAvailability(state mcp.ServerState) CapabilityAvailability {
	switch {
	case state.BlockedByTrust:
		return CapabilityAvailability{State: AvailabilityUntrusted, Detail: "the workspace is not trusted (ADR-0013)"}
	case !state.Enabled:
		return CapabilityAvailability{State: AvailabilityDisabled, Detail: "disabled in mcp.json"}
	case state.Requirements.MissingBinary != "":
		return CapabilityAvailability{State: AvailabilityNeedsConfiguration, Detail: "command not found: " + state.Requirements.MissingBinary}
	case state.Requirements.MissingEndpoint != "":
		return CapabilityAvailability{State: AvailabilityNeedsConfiguration, Detail: "endpoint is not there: " + state.Requirements.MissingEndpoint}
	case len(state.Requirements.MissingEnv) > 0:
		return CapabilityAvailability{State: AvailabilityNeedsConfiguration, Detail: "environment variable not set: " + strings.Join(state.Requirements.MissingEnv, ", ")}
	case state.LastError != "":
		return CapabilityAvailability{State: AvailabilityError, Detail: state.LastError}
	default:
		return CapabilityAvailability{State: AvailabilityAvailable}
	}
}

func effectiveCallTimeout(cfg MCPConfig) int {
	if cfg.CallTimeoutSeconds > 0 {
		return cfg.CallTimeoutSeconds
	}
	return int(mcp.DefaultCallTimeout.Seconds())
}

func effectiveIdleTimeout(cfg MCPConfig) int {
	if cfg.IdleTimeoutSeconds > 0 {
		return cfg.IdleTimeoutSeconds
	}
	return int(mcp.DefaultIdleTimeout.Seconds())
}

func effectiveConcurrency(cfg MCPConfig) int {
	if cfg.MaxConcurrentCalls > 0 {
		return cfg.MaxConcurrentCalls
	}
	return mcp.DefaultMaxConcurrentCalls
}

func effectiveResultBytes(cfg MCPConfig) int {
	if cfg.MaxResultBytes > 0 {
		return cfg.MaxResultBytes
	}
	return mcp.MaxToolResultBytes
}

// builtinCapability is the semantic job of a built-in tool, by name.
func builtinCapability(name string) string {
	switch name {
	case "search_text", "find_files", "read_file", "list_dir":
		return "repo.read"
	case "write_file", "replace_in_file", "apply_patch":
		return "repo.write"
	case protocol.ToolBash:
		return "process.exec"
	default:
		return "repo.read"
	}
}

// displayName turns a tool id into a label a UI can show without its own table.
func displayName(toolID string) string {
	words := strings.Split(strings.ReplaceAll(toolID, "-", "_"), "_")
	for index, word := range words {
		if word == "" {
			continue
		}
		words[index] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// SetCapabilityEnabled turns one capability on or off through the state that already owns it: an MCP
// tool edits its server's include list in the scope it came from, and nothing else is switchable —
// built-in tools are compiled in and the language-server tools follow the language server.
func (e *Engine) SetCapabilityEnabled(ctx context.Context, toolID string, enabled bool) error {
	if e.mcpRegistry == nil {
		return errors.New("capability is not switchable: no mcp registry")
	}
	workspace, err := e.lspWorkspace(ctx, "")
	if err != nil {
		return err
	}
	tools, err := e.mcpRegistry.AgentTools(workspace)
	if err != nil {
		return err
	}
	for _, tool := range tools {
		if tool.Name != toolID {
			continue
		}
		return e.setMCPToolIncluded(tool.ServerID, tool.Scope, tool.ToolName, enabled, workspace)
	}
	// A server that was never started has no live catalog: its include list names its tools.
	for _, cfg := range e.mcpRegistry.Configs() {
		for _, name := range cfg.Include {
			if mcp.AgentToolName(cfg.ID, name) == toolID {
				return e.setMCPToolIncluded(cfg.ID, cfg.Scope, name, enabled, workspace)
			}
		}
	}
	// A tool that was switched off is no longer in any list, so its name is recovered from the
	// namespaced id: mcp__<server>__<tool>.
	for _, cfg := range e.mcpRegistry.Configs() {
		if name, ok := strings.CutPrefix(toolID, mcp.AgentToolPrefix(cfg.ID)); ok && name != "" {
			return e.setMCPToolIncluded(cfg.ID, cfg.Scope, name, enabled, workspace)
		}
	}
	return fmt.Errorf("capability %q is not switchable: only an MCP server's tools can be enabled or disabled", toolID)
}

// setMCPToolIncluded adds or removes one tool from a server's include list. An empty include list means
// "every tool", so disabling one tool there first materialises the list from the server's catalog.
func (e *Engine) setMCPToolIncluded(serverID, scope, toolName string, enabled bool, workspace string) error {
	configs, err := e.mcpRegistry.ConfigsScope(scope)
	if err != nil {
		return err
	}
	var target *MCPConfig
	for index := range configs {
		if configs[index].ID == serverID {
			target = &configs[index]
			break
		}
	}
	if target == nil {
		return fmt.Errorf("mcp server %q is not configured in scope %s", serverID, scope)
	}
	include := append([]string(nil), target.Include...)
	if enabled {
		if len(include) == 0 {
			return nil
		}
		for _, name := range include {
			if name == toolName {
				return nil
			}
		}
		include = append(include, toolName)
		sort.Strings(include)
	} else {
		if len(include) == 0 {
			// Materialise "every tool" so one can be removed without changing the others.
			live, err := e.mcpRegistry.AgentTools(workspace)
			if err != nil {
				return err
			}
			for _, tool := range live {
				if tool.ServerID == serverID {
					include = append(include, tool.ToolName)
				}
			}
			sort.Strings(include)
		}
		kept := include[:0]
		for _, name := range include {
			if name != toolName {
				kept = append(kept, name)
			}
		}
		include = kept
		if len(include) == 0 {
			return fmt.Errorf("refusing to leave mcp server %q with no tools; disable the server instead", serverID)
		}
	}
	updated := *target
	updated.Include = include
	if err := e.mcpRegistry.UpsertScope(scope, updated); err != nil {
		return err
	}
	e.events.publish("capability.changed", "", map[string]any{
		"serverId": serverID,
		"tool":     toolName,
		"enabled":  enabled,
		"scope":    scope,
	})
	return nil
}

// SetMCPServerEnabled enables or disables one configured server in its scope. It is the coarse switch
// a Plugins page offers next to the per-tool one, and it keeps the server's configuration intact.
func (e *Engine) SetMCPServerEnabled(ctx context.Context, scope, serverID string, enabled bool) error {
	if e.mcpRegistry == nil {
		return errors.New("mcp registry is unavailable")
	}
	configs, err := e.mcpRegistry.ConfigsScope(scope)
	if err != nil {
		return err
	}
	for _, cfg := range configs {
		if cfg.ID != serverID {
			continue
		}
		cfg.Disabled = !enabled
		if err := e.mcpRegistry.UpsertScope(scope, cfg); err != nil {
			return err
		}
		e.events.publish("capability.changed", "", map[string]any{
			"serverId": serverID,
			"enabled":  enabled,
			"scope":    scope,
		})
		return nil
	}
	return fmt.Errorf("mcp server %q is not configured in scope %s", serverID, scope)
}

// ToolSchemaCost is the byte cost of the tool schemas a request carries, by source: the number a run
// pays per request for offering tools, before any model call.
type ToolSchemaCost struct {
	BuiltinBytes int            `json:"builtinBytes"`
	LSPBytes     int            `json:"lspBytes"`
	MCPBytes     map[string]int `json:"mcpBytes"`
	TotalBytes   int            `json:"totalBytes"`
	// Tools counts the specs each total is made of.
	BuiltinTools int            `json:"builtinTools"`
	LSPTools     int            `json:"lspTools"`
	MCPTools     map[string]int `json:"mcpTools"`
}

// ToolSchemaCost measures what a request pays for tool schemas in this workspace: built-in tools, the
// language-server tools when a server is attached, and each configured MCP server's exposed tools. It
// reads catalogs only; nothing is started.
func (e *Engine) ToolSchemaCost(ctx context.Context, query CapabilityQuery) (ToolSchemaCost, error) {
	workspace, err := e.lspWorkspace(ctx, query.SessionID)
	if err != nil {
		return ToolSchemaCost{}, err
	}
	cost := ToolSchemaCost{MCPBytes: map[string]int{}, MCPTools: map[string]int{}}
	builtin := e.builtinSpecs()
	cost.BuiltinBytes = specBytes(builtin)
	cost.BuiltinTools = len(builtin)
	if e.lspAvailable(workspace) {
		lspSpecs := toolset.LSPSpecs()
		cost.LSPBytes = specBytes(lspSpecs)
		cost.LSPTools = len(lspSpecs)
	}
	if e.mcpRegistry != nil {
		tools, err := e.mcpRegistry.AgentTools(workspace)
		if err != nil {
			return ToolSchemaCost{}, err
		}
		for _, tool := range tools {
			spec := protocol.ToolSpec{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema}
			cost.MCPBytes[tool.ServerID] += specBytes([]protocol.ToolSpec{spec})
			cost.MCPTools[tool.ServerID]++
		}
	}
	cost.TotalBytes = cost.BuiltinBytes + cost.LSPBytes
	for _, bytes := range cost.MCPBytes {
		cost.TotalBytes += bytes
	}
	return cost, nil
}

// lspAvailable reports whether a run in the workspace is offered the lsp_* tools. It starts nothing:
// the manager is an object until a request needs a server.
func (e *Engine) lspAvailable(workspace string) bool {
	manager, err := e.lspManagerFor(workspace)
	return err == nil && manager.Available()
}

// builtinSpecs is the built-in tool list a run of this Engine is offered: bash runs in the bash tool's
// interpreter and has its background mode when the job queue is open.
func (e *Engine) builtinSpecs() []protocol.ToolSpec {
	return toolset.BuiltinSpecs(shell.Bash(), e.jobs != nil)
}

// specBytes is the JSON size of a tool spec list, the shape a provider request carries.
func specBytes(specs []protocol.ToolSpec) int {
	raw, err := json.Marshal(specs)
	if err != nil {
		return 0
	}
	return len(raw)
}

// discoverMCPCatalogs starts the servers whose tool list is not known yet, so the run about to be
// prepared can offer their tools: a run that may use a server needs its tool list (ADR-0021 §3). It is
// a no-op for every server whose catalog a previous start recorded — the steady state — and a failure
// is recorded as that server's lastError: the server contributes no tools and the run continues. The
// wait is bounded in total, and the calls run concurrently so N cold servers cost one handshake, not N.
func (e *Engine) discoverMCPCatalogs(ctx context.Context, registry *mcp.Registry, workspace string) {
	missing := registry.MissingCatalogs(workspace)
	if len(missing) == 0 {
		return
	}
	discoverCtx, cancel := context.WithTimeout(ctx, mcpCatalogDiscoveryBudget)
	defer cancel()
	done := make(chan struct{}, len(missing))
	for _, serverID := range missing {
		serverID := serverID
		go func() {
			defer func() { done <- struct{}{} }()
			_, _ = registry.Start(discoverCtx, serverID, workspace)
		}()
	}
	for range missing {
		select {
		case <-done:
		case <-discoverCtx.Done():
			return
		}
	}
}

// mcpCatalogDiscoveryBudget bounds how long a run start waits for the catalogs it does not know yet.
const mcpCatalogDiscoveryBudget = 10 * time.Second
