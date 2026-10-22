package toolset

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/mcp"
)

// TestToolsetMCPServer is a minimal stdio MCP server with two tools that carry no readOnlyHint
// annotation, so only a configuration override can make one of them read-only.
func TestToolsetMCPServer(t *testing.T) {
	if os.Getenv("LCX_TOOLSET_MCP_HELPER") != "1" {
		return
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	write := func(value any) {
		raw, _ := json.Marshal(value)
		_, _ = writer.Write(raw)
		_, _ = writer.WriteString("\n")
		_ = writer.Flush()
	}
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			os.Exit(0)
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil || len(msg.ID) == 0 {
			continue
		}
		var id any
		_ = json.Unmarshal(msg.ID, &id)
		switch msg.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": mcp.ProtocolLegacy,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "toolset-helper", "version": "1"},
			}})
		case "tools/list":
			tool := func(name string) map[string]any {
				return map[string]any{"name": name, "description": name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"tools": []map[string]any{tool("inspect"), tool("mutate")},
			}})
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"content": []map[string]any{{"type": "text", "text": params.Name}},
			}})
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}
}

// ADR-0013: an override from the project file of a trusted workspace makes an unannotated tool
// read-only, so a read-only run sees and can call it, while the tools it does not cover stay exec
// and are denied.
func TestMCPToolOverrideDecidesWhatAReadOnlyRunSees(t *testing.T) {
	root := t.TempDir()
	registry, err := mcp.NewLayeredRegistry(root, "", filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	registry.SetProjectTrust(func() bool { return true })
	t.Setenv("LCX_TOOLSET_MCP_HELPER", "1")

	if err := registry.Upsert(mcp.Config{
		ID:           "helper",
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestToolsetMCPServer"},
		ProtocolMode: mcp.ModeLegacy,
		Tools:        map[string]mcp.ToolConfig{"inspect": {Permission: mcp.PermissionRead}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Start(context.Background(), "helper", root); err != nil {
		t.Fatal(err)
	}

	set, err := New(Options{Workspace: root, Policy: PolicyReadOnly, MCP: registry})
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]string{}
	for _, spec := range set.Specs(nil) {
		specs[spec.Name] = spec.Permission
	}
	if specs["mcp__helper__inspect"] != "read" {
		t.Fatalf("overridden tool not exposed to a read-only run: %#v", specs)
	}
	if _, ok := specs["mcp__helper__mutate"]; ok {
		t.Fatalf("exec tool exposed to a read-only run: %#v", specs)
	}

	denied, err := set.Execute(context.Background(), "mcp__helper__mutate", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !denied.Denied || denied.Permission != "exec" {
		t.Fatalf("exec tool was not denied: %#v", denied)
	}
	allowed, err := set.Execute(context.Background(), "mcp__helper__inspect", map[string]any{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed.OK || allowed.Denied {
		t.Fatalf("overridden tool was not callable: %#v", allowed)
	}
}
