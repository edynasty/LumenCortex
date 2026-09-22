package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryPersistsConfigWithoutAutostart(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, ".lumencortex", "mcp.json")
	registry, err := NewRegistry(root, configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Upsert(Config{
		ID: "helper",
		Name: "Helper",
		Command: os.Args[0],
		Args: []string{"-test.run=TestHelperMCPServer"},
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
	if err := registry.Upsert(Config{
		ID: "helper_server_with_a_long_name",
		Command: os.Args[0],
		Args: []string{"-test.run=TestHelperMCPServer"},
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

	cfg := Config{
		ID: "helper",
		Command: os.Args[0],
		Args: []string{"-test.run=TestHelperMCPServer"},
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

	global := Config{
		ID: "shared",
		Name: "Global helper",
		Command: os.Args[0],
		Args: []string{"-test.run=TestHelperMCPServer"},
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
