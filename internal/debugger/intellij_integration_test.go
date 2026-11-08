package debugger

import (
	"context"
	"encoding/json"
	"github.com/edynasty/LumenCortex/internal/mcp"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealIntelliJManagedSession(t *testing.T) {
	root := os.Getenv("LC_TEST_INTELLIJ_PROJECT")
	endpoint := os.Getenv("LC_TEST_INTELLIJ_URL")
	if root == "" || endpoint == "" {
		t.Skip("set isolated IntelliJ project and endpoint")
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcp.json")
	cfg := mcp.Config{ID: "ide", Transport: mcp.TransportHTTP, URL: endpoint, HostOnly: true, Headers: map[string]string{"IJ_MCP_SERVER_PROJECT_PATH": os.Getenv("LC_TEST_INTELLIJ_PROJECT")}, CallTimeoutSeconds: 30, MaxConcurrentCalls: 1}
	data, _ := json.Marshal(mcp.ConfigFile{Servers: map[string]mcp.Config{"ide": cfg}})
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	registry, err := mcp.NewLayeredRegistry(root, path, filepath.Join(t.TempDir(), "unused.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer registry.Close()
	manager, err := New(Options{Adapters: []Adapter{{ID: "java", Language: "Java", MCPServer: "ide"}}, MCP: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	snapshot, err := manager.Start(ctx, StartOptions{SessionID: "owner", Actor: "human", Workspace: root, AdapterID: "java", Program: "Main.java"})
	if err != nil {
		t.Fatal(err)
	}
	c := Control{SessionID: "owner", DebugID: snapshot.ID, Actor: "human", Epoch: snapshot.Epoch}
	if tools, err := registry.AgentTools(root); err != nil || len(tools) != 0 {
		t.Fatalf("raw IDE tools exposed: %v %+v", err, tools)
	}
	if _, err = registry.CallToolForSession(ctx, root, "ide", "xdebug_get_debugger_status", "foreign", nil); err == nil {
		t.Fatal("raw IDE call accepted")
	}
	if _, err = manager.Start(ctx, StartOptions{SessionID: "foreign", Actor: "human", Workspace: root, AdapterID: "java", Program: "Main.java"}); err == nil {
		t.Fatal("second IDE owner accepted")
	}
	if _, err = manager.Breakpoints(ctx, c, "Main.java", []Breakpoint{{Line: 5}}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	if _, err = manager.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	for {
		state, err := manager.Snapshot("owner", snapshot.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.State == "stopped" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
	stack, err := manager.Inspect(ctx, c, "stackTrace", 1, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	var view struct {
		Frames []struct {
			ID   int `json:"id"`
			Line int `json:"line"`
		} `json:"stackFrames"`
	}
	if err = json.Unmarshal(stack, &view); err != nil {
		t.Fatal(err)
	}
	if len(view.Frames) == 0 || view.Frames[0].Line != 5 {
		t.Fatalf("unexpected real stack %s", stack)
	}
	value, err := manager.Evaluate(ctx, c, view.Frames[0].ID, "value", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(value), `"result":"42"`) {
		t.Fatalf("uninitialized/wrong value %s", value)
	}
	c.Action = "stop"
	if _, err = manager.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	t.Log("INTELLIJ_CORE_REAL: Main.java:5 value=42; raw tool access denied; duplicate owner denied; explicit stop")
}
