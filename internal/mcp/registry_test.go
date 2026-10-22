package mcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// trustedProject installs a project trust check and returns the switch that flips it, so a test can
// withdraw trust from a registry that already started a server.
func trustedProject(registry *Registry, want bool) *atomic.Bool {
	trusted := &atomic.Bool{}
	trusted.Store(want)
	registry.SetProjectTrust(trusted.Load)
	return trusted
}

func TestRegistryPersistsConfigWithoutAutostart(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".lumencortex", "mcp.json")
	registry, err := NewRegistry(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	trustedProject(registry, true)
	if err := registry.Upsert(Config{
		ID:           "helper",
		Name:         "Helper",
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestHelperMCPServer"},
		ProtocolMode: ModeLegacy,
	}); err != nil {
		t.Fatal(err)
	}
	if statuses, err := registry.Statuses(root); err != nil || len(statuses) != 0 {
		t.Fatalf("unexpected auto-start statuses=%#v err=%v", statuses, err)
	}
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewRegistry(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	trustedProject(reloaded, true)
	configs := reloaded.Configs()
	if len(configs) != 1 || configs[0].ID != "helper" || configs[0].Workspace != "" {
		t.Fatalf("configs=%#v", configs)
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\"helper\"") {
		t.Fatalf("config=%s", raw)
	}
}

func TestRegistryRuntimeIsolationAndAgentTools(t *testing.T) {
	root := t.TempDir()
	other := t.TempDir()
	registry, err := NewRegistry(root, filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trustedProject(registry, true)
	if err := registry.Upsert(Config{
		ID:           "helper_server_with_a_long_name",
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestHelperMCPServer"},
		ProtocolMode: ModeLegacy,
	}); err != nil {
		t.Fatal(err)
	}

	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")
	status, err := registry.Start(context.Background(), "helper_server_with_a_long_name", root)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.Workspace != root {
		t.Fatalf("status=%#v", status)
	}
	otherStatuses, err := registry.Statuses(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(otherStatuses) != 0 {
		t.Fatalf("other runtime statuses=%#v", otherStatuses)
	}

	tools, err := registry.AgentTools(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("tools=%#v", tools)
	}
	if len(tools[0].Name) > 64 || !strings.HasPrefix(tools[0].Name, "mcp__") {
		t.Fatalf("tool name=%q len=%d", tools[0].Name, len(tools[0].Name))
	}
	if tools[0].ReadOnly {
		t.Fatalf("helper tool should default to side-effect capable: %#v", tools[0])
	}
}

func TestRegistryRejectsInvalidServerID(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	if err := registry.Upsert(Config{ID: "bad id with spaces", Command: "server"}); err == nil {
		t.Fatal("expected invalid server id rejection")
	}
}

func TestAgentToolNameIsBoundedAndDeterministic(t *testing.T) {
	nameA := agentToolName(strings.Repeat("server!", 20), strings.Repeat("tool/name", 20))
	nameB := agentToolName(strings.Repeat("server!", 20), strings.Repeat("tool/name", 20))
	if nameA != nameB {
		t.Fatalf("names differ: %q %q", nameA, nameB)
	}
	if len(nameA) > 64 {
		t.Fatalf("tool name too long: %d %q", len(nameA), nameA)
	}
}

func TestRegistryDisabledServerCannotStartAndStopsRunningInstances(t *testing.T) {
	root := t.TempDir()
	registry, err := NewRegistry(root, filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trustedProject(registry, true)

	cfg := Config{
		ID:           "helper",
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestHelperMCPServer"},
		ProtocolMode: ModeLegacy,
	}
	if err := registry.Upsert(cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")
	if _, err := registry.Start(context.Background(), "helper", root); err != nil {
		t.Fatal(err)
	}
	statuses, err := registry.Statuses(root)
	if err != nil || len(statuses) != 1 || !statuses[0].Running {
		t.Fatalf("statuses=%#v err=%v", statuses, err)
	}

	cfg.Disabled = true
	if err := registry.Upsert(cfg); err != nil {
		t.Fatal(err)
	}
	statuses, err = registry.Statuses(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 0 {
		t.Fatalf("disabled server should be stopped: %#v", statuses)
	}
	if _, err := registry.Start(context.Background(), "helper", root); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("expected disabled start rejection, got %v", err)
	}

	configs := registry.Configs()
	if len(configs) != 1 || !configs[0].Disabled {
		t.Fatalf("configs=%#v", configs)
	}
}

func TestLayeredRegistryProjectOverridesGlobalAndFallsBack(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(t.TempDir(), "global", "mcp.json")
	projectPath := filepath.Join(root, ".lumencortex", "mcp.json")

	registry, err := NewLayeredRegistry(root, globalPath, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trustedProject(registry, true)

	global := Config{
		ID:           "shared",
		Name:         "Global helper",
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestHelperMCPServer"},
		ProtocolMode: ModeLegacy,
	}
	if err := registry.UpsertScope(ScopeGlobal, global); err != nil {
		t.Fatal(err)
	}
	effective := registry.Configs()
	if len(effective) != 1 || effective[0].Name != "Global helper" {
		t.Fatalf("effective after global=%#v", effective)
	}

	project := global
	project.Name = "Project helper"
	if err := registry.UpsertScope(ScopeProject, project); err != nil {
		t.Fatal(err)
	}
	effective = registry.Configs()
	if len(effective) != 1 || effective[0].Name != "Project helper" {
		t.Fatalf("project override not effective: %#v", effective)
	}
	globals, err := registry.ConfigsScope(ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := registry.ConfigsScope(ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(globals) != 1 || globals[0].Name != "Global helper" {
		t.Fatalf("globals=%#v", globals)
	}
	if len(projects) != 1 || projects[0].Name != "Project helper" {
		t.Fatalf("projects=%#v", projects)
	}

	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")
	status, err := registry.Start(context.Background(), "shared", root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "Project helper" {
		t.Fatalf("start did not use project override: %#v", status)
	}
	if err := registry.Stop("shared", root); err != nil {
		t.Fatal(err)
	}

	if err := registry.DeleteScope(ScopeProject, "shared"); err != nil {
		t.Fatal(err)
	}
	effective = registry.Configs()
	if len(effective) != 1 || effective[0].Name != "Global helper" {
		t.Fatalf("global fallback not restored: %#v", effective)
	}

	reloaded, err := NewLayeredRegistry(root, globalPath, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	trustedProject(reloaded, true)
	globals, err = reloaded.ConfigsScope(ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	projects, err = reloaded.ConfigsScope(ScopeProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(globals) != 1 || len(projects) != 0 {
		t.Fatalf("reloaded scopes globals=%#v projects=%#v", globals, projects)
	}
}

// helperConfig is the in-process test MCP server (TestHelperMCPServer) as a server entry.
func helperConfig(id, name string) Config {
	return Config{
		ID:           id,
		Name:         name,
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestHelperMCPServer"},
		ProtocolMode: ModeLegacy,
	}
}

func writeConfigFile(t *testing.T, path string, configs ...Config) {
	t.Helper()
	servers := make(map[string]Config, len(configs))
	for _, cfg := range configs {
		servers[cfg.ID] = cfg
	}
	if err := saveConfigFile(path, servers); err != nil {
		t.Fatal(err)
	}
}

// ADR-0013: a server defined by the project file of an untrusted workspace is refused before any
// process exists, and the listing says why instead of hiding it.
func TestUntrustedProjectServerIsRefusedWithoutSpawning(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(t.TempDir(), "spawned")
	projectPath := filepath.Join(root, ".lumencortex", "mcp.json")
	writeConfigFile(t, projectPath, Config{
		ID:      "project",
		Command: "/bin/sh",
		Args:    []string{"-c", "touch " + marker},
	})

	registry, err := NewLayeredRegistry(root, filepath.Join(t.TempDir(), "mcp.json"), projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trustedProject(registry, false)

	if _, err := registry.Start(context.Background(), "project", root); !errors.Is(err, ErrUntrustedProject) {
		t.Fatalf("start error = %v, want ErrUntrustedProject", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("project server was spawned: %v", err)
	}
	statuses, err := registry.Statuses(root)
	if err != nil || len(statuses) != 0 {
		t.Fatalf("statuses=%#v err=%v", statuses, err)
	}
	configs := registry.Configs()
	if len(configs) != 1 || configs[0].Scope != ScopeProject || !configs[0].BlockedByTrust {
		t.Fatalf("effective listing = %#v", configs)
	}
	projects, err := registry.ConfigsScope(ScopeProject)
	if err != nil || len(projects) != 1 || !projects[0].BlockedByTrust || projects[0].Scope != ScopeProject {
		t.Fatalf("project listing = %#v err=%v", projects, err)
	}
}

func TestGlobalServerStartsWhileProjectIsUntrusted(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(t.TempDir(), "mcp.json")
	projectPath := filepath.Join(root, ".lumencortex", "mcp.json")
	writeConfigFile(t, projectPath, Config{ID: "project", Command: "/nonexistent/mcp-server"})

	registry, err := NewLayeredRegistry(root, globalPath, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trustedProject(registry, false)
	if err := registry.UpsertScope(ScopeGlobal, helperConfig("global", "Global helper")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")

	status, err := registry.Start(context.Background(), "global", root)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.Scope != ScopeGlobal || status.BlockedByTrust {
		t.Fatalf("status = %#v", status)
	}
	tools, err := registry.AgentTools(root)
	if err != nil || len(tools) != 1 || tools[0].Scope != ScopeGlobal {
		t.Fatalf("tools = %#v err=%v", tools, err)
	}
	if _, err := registry.Start(context.Background(), "project", root); !errors.Is(err, ErrUntrustedProject) {
		t.Fatalf("project start error = %v", err)
	}
}

// While the project file is ignored, an id it shadows resolves to the global entry, and the listing
// shows that global entry, so the start is not silent. Trusting the workspace switches the same id
// to the project entry.
func TestUntrustedProjectShadowResolvesToGlobalEntry(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(t.TempDir(), "mcp.json")
	projectPath := filepath.Join(root, ".lumencortex", "mcp.json")
	writeConfigFile(t, projectPath, helperConfig("shared", "Project helper"))

	registry, err := NewLayeredRegistry(root, globalPath, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trusted := trustedProject(registry, false)
	if err := registry.UpsertScope(ScopeGlobal, helperConfig("shared", "Global helper")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")

	status, err := registry.Start(context.Background(), "shared", root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "Global helper" || status.Scope != ScopeGlobal || status.BlockedByTrust {
		t.Fatalf("untrusted status = %#v", status)
	}
	configs := registry.Configs()
	if len(configs) != 1 || configs[0].Scope != ScopeGlobal || configs[0].Name != "Global helper" || configs[0].BlockedByTrust {
		t.Fatalf("untrusted listing = %#v", configs)
	}

	trusted.Store(true)
	status, err = registry.Start(context.Background(), "shared", root)
	if err != nil {
		t.Fatal(err)
	}
	if status.Name != "Project helper" || status.Scope != ScopeProject || status.BlockedByTrust {
		t.Fatalf("trusted status = %#v", status)
	}
	configs = registry.Configs()
	if len(configs) != 1 || configs[0].Scope != ScopeProject || configs[0].Name != "Project helper" {
		t.Fatalf("trusted listing = %#v", configs)
	}
}

// Withdrawing trust stops the tools of a running project server from reaching a run, without
// killing the process, and trusting again restores them.
func TestAgentToolsDropProjectServerWhenTrustIsWithdrawn(t *testing.T) {
	root := t.TempDir()
	registry, err := NewRegistry(root, filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trusted := trustedProject(registry, true)
	if err := registry.Upsert(helperConfig("helper", "Helper")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")
	if _, err := registry.Start(context.Background(), "helper", root); err != nil {
		t.Fatal(err)
	}
	if tools, err := registry.AgentTools(root); err != nil || len(tools) != 1 {
		t.Fatalf("trusted tools = %#v err=%v", tools, err)
	}

	trusted.Store(false)
	tools, err := registry.AgentTools(root)
	if err != nil || len(tools) != 0 {
		t.Fatalf("untrusted tools = %#v err=%v", tools, err)
	}
	statuses, err := registry.Statuses(root)
	if err != nil || len(statuses) != 1 || !statuses[0].Running || !statuses[0].BlockedByTrust || statuses[0].Scope != ScopeProject {
		t.Fatalf("untrusted statuses = %#v err=%v", statuses, err)
	}
	if _, err := registry.CallTool(context.Background(), root, "helper", "echo", map[string]any{"text": "x"}); !errors.Is(err, ErrUntrustedProject) {
		t.Fatalf("call error = %v, want ErrUntrustedProject", err)
	}

	trusted.Store(true)
	if tools, err := registry.AgentTools(root); err != nil || len(tools) != 1 {
		t.Fatalf("restored tools = %#v err=%v", tools, err)
	}
}

// effectivePermission: the annotation decides the default; an override applies only from the global
// file or a trusted project.
func TestEffectivePermissionPrecedence(t *testing.T) {
	plain := Tool{Name: "echo"}
	annotated := Tool{Name: "echo", Annotations: map[string]any{"readOnlyHint": true}}
	readOverride := Config{ID: "s", Tools: map[string]ToolConfig{"echo": {Permission: PermissionRead}}}
	execOverride := Config{ID: "s", Tools: map[string]ToolConfig{"echo": {Permission: PermissionExec}}}

	cases := []struct {
		name    string
		cfg     Config
		scope   string
		trusted bool
		tool    Tool
		want    string
		source  string
	}{
		{"unannotated is exec", Config{ID: "s"}, ScopeGlobal, true, plain, PermissionExec, PermissionSourceAnnotation},
		{"readOnlyHint is read", Config{ID: "s"}, ScopeGlobal, true, annotated, PermissionRead, PermissionSourceAnnotation},
		{"global override", readOverride, ScopeGlobal, false, plain, PermissionRead, PermissionSourceOverride},
		{"trusted project override", readOverride, ScopeProject, true, plain, PermissionRead, PermissionSourceOverride},
		{"untrusted project override is ignored", readOverride, ScopeProject, false, plain, PermissionExec, PermissionSourceAnnotation},
		{"override can tighten", execOverride, ScopeGlobal, true, annotated, PermissionExec, PermissionSourceOverride},
	}
	for _, tc := range cases {
		permission, source := effectivePermission(tc.cfg, tc.scope, tc.trusted, tc.tool)
		if permission != tc.want || source != tc.source {
			t.Errorf("%s: got %s/%s, want %s/%s", tc.name, permission, source, tc.want, tc.source)
		}
	}
}

func TestToolPermissionOverrideReachesAgentTools(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(t.TempDir(), "mcp.json")
	projectPath := filepath.Join(root, ".lumencortex", "mcp.json")
	writeConfigFile(t, projectPath, Config{ID: "project", Command: "/nonexistent/mcp-server"})

	registry, err := NewLayeredRegistry(root, globalPath, projectPath)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trustedProject(registry, false)

	cfg := helperConfig("global", "Global helper")
	cfg.Tools = map[string]ToolConfig{"echo": {Permission: PermissionRead}}
	if err := registry.UpsertScope(ScopeGlobal, cfg); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")
	if _, err := registry.Start(context.Background(), "global", root); err != nil {
		t.Fatal(err)
	}
	tools, err := registry.AgentTools(root)
	if err != nil || len(tools) != 1 {
		t.Fatalf("tools = %#v err=%v", tools, err)
	}
	if tools[0].Permission != PermissionRead || !tools[0].ReadOnly || tools[0].PermissionSource != PermissionSourceOverride {
		t.Fatalf("tool = %#v", tools[0])
	}
}

func TestInvalidToolPermissionIsRejected(t *testing.T) {
	registry, err := NewRegistry(t.TempDir(), filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	err = registry.Upsert(Config{ID: "helper", Command: "/bin/true", Tools: map[string]ToolConfig{"echo": {Permission: "root"}}})
	if err == nil || !strings.Contains(err.Error(), `tools["echo"].permission`) {
		t.Fatalf("upsert error = %v", err)
	}
	if err := registry.Upsert(Config{ID: "helper", Command: "/bin/true", Tools: map[string]ToolConfig{"echo": {}}}); err == nil || !strings.Contains(err.Error(), `tools["echo"].permission`) {
		t.Fatalf("empty permission error = %v", err)
	}

	path := filepath.Join(t.TempDir(), "mcp.json")
	raw := `{"servers":{"helper":{"command":"/bin/true","tools":{"echo":{"permission":"root"}}}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRegistry(t.TempDir(), path); err == nil || !strings.Contains(err.Error(), `tools["echo"].permission`) {
		t.Fatalf("load error = %v", err)
	}
}

// The listing fields describe the listing; they never reach the file.
func TestListingFieldsAreNotPersisted(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".lumencortex", "mcp.json")
	registry, err := NewRegistry(root, path)
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trustedProject(registry, false)
	cfg := helperConfig("helper", "Helper")
	cfg.Scope = ScopeProject
	cfg.BlockedByTrust = true
	if err := registry.Upsert(cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "blockedByTrust") || strings.Contains(string(raw), `"scope"`) {
		t.Fatalf("listing fields persisted: %s", raw)
	}
}

// The trust check describes the Engine workspace, not the runtime workspace a run uses: a session
// worktree changes only the working directory.
func TestProjectTrustIsIndependentOfTheRuntimeWorkspace(t *testing.T) {
	root := t.TempDir()
	worktree := t.TempDir()
	registry, err := NewRegistry(root, filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	trusted := trustedProject(registry, false)
	if err := registry.Upsert(helperConfig("helper", "Helper")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")

	if _, err := registry.Start(context.Background(), "helper", worktree); !errors.Is(err, ErrUntrustedProject) {
		t.Fatalf("start error = %v, want ErrUntrustedProject", err)
	}
	trusted.Store(true)
	status, err := registry.Start(context.Background(), "helper", worktree)
	if err != nil {
		t.Fatal(err)
	}
	if status.Workspace != worktree || status.Scope != ScopeProject {
		t.Fatalf("status = %#v", status)
	}
	if tools, err := registry.AgentTools(worktree); err != nil || len(tools) != 1 {
		t.Fatalf("tools = %#v err=%v", tools, err)
	}
}
