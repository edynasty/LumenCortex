package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mcpGlobalConfig is the fixture catalog with an optional trustedWorkspaces list.
func mcpGlobalConfig(trusted ...string) string {
	trust := ""
	if len(trusted) > 0 {
		quoted := make([]string, 0, len(trusted))
		for _, workspace := range trusted {
			quoted = append(quoted, fmt.Sprintf("%q", workspace))
		}
		trust = `"trustedWorkspaces": [` + strings.Join(quoted, ", ") + `], `
	}
	return fmt.Sprintf(`{%s"model": "test/b0", "providers": {"test": {"settings": {"baseURL": "http://127.0.0.1:9/v1"}, "models": {"b0": {"modelID": "b0"}}}}}`, trust)
}

// writeEngineMCP writes the project mcp.json the Engine reads from its state directory.
func writeEngineMCP(t *testing.T, workspace string, servers map[string]any) {
	t.Helper()
	dir := filepath.Join(workspace, ".lumencortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"servers": servers})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// ADR-0013: a server defined by the project mcp.json of an untrusted workspace is refused before
// its command runs, and the listing says why. Trusting the workspace lets the same entry spawn.
func TestMCPProjectServerNeedsTrustedWorkspace(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "spawned")
	// The command records that it ran and then exits, so a spawn is observable and the handshake
	// fails without a live server.
	writeEngineMCP(t, workspace, map[string]any{
		"project": map[string]any{"command": "/bin/sh", "args": []string{"-c", "touch " + marker + " && exec /bin/true"}},
	})
	useGlobalConfig(t, mcpGlobalConfig())

	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	if _, err := engine.MCPStart(context.Background(), "", "project"); !errors.Is(err, ErrUntrustedWorkspace) {
		t.Fatalf("untrusted start error = %v, want ErrUntrustedWorkspace", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("untrusted start spawned the project command: %v", err)
	}
	configs := engine.MCPConfigs()
	if len(configs) != 1 || configs[0].Scope != "project" || !configs[0].BlockedByTrust {
		t.Fatalf("untrusted listing = %#v", configs)
	}
	if statuses, err := engine.MCPStatuses(context.Background(), ""); err != nil || len(statuses) != 0 {
		t.Fatalf("untrusted statuses = %#v err=%v", statuses, err)
	}
	if tools, err := engine.MCPTools(context.Background(), ""); err != nil || len(tools) != 0 {
		t.Fatalf("untrusted tools = %#v err=%v", tools, err)
	}

	useGlobalConfig(t, mcpGlobalConfig(workspace))
	if _, err := engine.MCPStart(context.Background(), "", "project"); err == nil {
		t.Fatal("the placeholder command should fail the MCP handshake")
	} else if errors.Is(err, ErrUntrustedWorkspace) {
		t.Fatalf("trusted start was still blocked: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("trusted start did not spawn the project command: %v", err)
	}
	configs = engine.MCPConfigs()
	if len(configs) != 1 || configs[0].Scope != "project" || configs[0].BlockedByTrust {
		t.Fatalf("trusted listing = %#v", configs)
	}
}
