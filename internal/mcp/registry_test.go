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
	if !strings.Contains(string(raw), ""helper"") {
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
