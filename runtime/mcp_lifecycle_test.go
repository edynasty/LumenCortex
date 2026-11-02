package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/mcp"
	"github.com/edynasty/LumenCortex/internal/mcptemplates"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// TestRuntimeMCPServer is the test MCP server: a real stdio JSON-RPC process the lifecycle tests start
// through the real Engine. It is the same binary as the test, re-executed with -test.run, so no
// network and no third-party dependency is involved.
//
// Knobs (environment):
//
//	LCX_TEST_MCP=1          run as the server
//	LCX_TEST_MCP_MODE       "" (normal), "crash" (exit on the first call), "hang" (never answer a call)
//	LCX_TEST_MCP_MARKER     file touched when the process starts
//	LCX_TEST_MCP_LOG        file each tool call name and its arguments are appended to
//	LCX_TEST_MCP_EXTRA_TOOL one more tool the server reports, by this name (it echoes like echo)
func TestRuntimeMCPServer(t *testing.T) {
	if os.Getenv("LCX_TEST_MCP") != "1" {
		return
	}
	mode := os.Getenv("LCX_TEST_MCP_MODE")
	if marker := os.Getenv("LCX_TEST_MCP_MARKER"); marker != "" {
		_ = os.WriteFile(marker, []byte("started"), 0o600)
	}
	logLine := func(line string) {
		path := os.Getenv("LCX_TEST_MCP_LOG")
		if path == "" {
			return
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		_, _ = file.WriteString(line + "\n")
		_ = file.Close()
	}
	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	write := func(value any) {
		raw, _ := json.Marshal(value)
		_, _ = writer.Write(raw)
		_, _ = writer.WriteString("\n")
		_ = writer.Flush()
	}
	tools := []map[string]any{
		{"name": "echo", "description": "Echo text back.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
			"annotations": map[string]any{"readOnlyHint": true}},
		{"name": "mutate", "description": "Pretend to change something.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}}},
		{"name": "computer_start", "description": "Open a session.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"session_id": map[string]any{"type": "string"}}}},
		{"name": "computer_stop", "description": "Close a session.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"computer_session_id": map[string]any{"type": "string"}}}},
	}
	if extra := os.Getenv("LCX_TEST_MCP_EXTRA_TOOL"); extra != "" {
		tools = append(tools, map[string]any{"name": extra, "description": "Echo text back.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}})
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		var msg struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal([]byte(line), &msg) != nil || msg.Method == "" || len(msg.ID) == 0 {
			continue
		}
		var id any
		_ = json.Unmarshal(msg.ID, &id)
		switch msg.Method {
		case "initialize":
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
				"protocolVersion": mcp.ProtocolLegacy,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "runtime-test-mcp", "version": "1"},
			}})
		case "tools/list":
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{"tools": tools}})
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(msg.Params, &params)
			logLine(params.Name + " " + mustJSON(params.Arguments))
			switch mode {
			case "crash":
				os.Exit(3)
			case "hang":
				time.Sleep(120 * time.Second)
				os.Exit(4)
			}
			switch params.Name {
			case "computer_start":
				session, _ := params.Arguments["session_id"].(string)
				write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"content":           []map[string]any{{"type": "text", "text": `{"computer_session_id":"cmp_test_1","state":"active"}`}},
					"structuredContent": map[string]any{"computer_session_id": "cmp_test_1", "state": "active", "lcSession": session},
				}})
			case "computer_stop":
				write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": `{"state":"stopped"}`}},
				}})
			default:
				text, _ := params.Arguments["text"].(string)
				write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				}})
			}
		default:
			write(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{}})
		}
	}
}

func mustJSON(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

// mcpTestServerConfig is the configuration of the test server, with the knobs the caller wants.
func mcpTestServerConfig(t *testing.T, id string, overrides map[string]any) map[string]any {
	t.Helper()
	server := map[string]any{
		"command": os.Args[0],
		"args":    []string{"-test.run=TestRuntimeMCPServer"},
	}
	for key, value := range overrides {
		server[key] = value
	}
	return server
}

// useGlobalMCP writes the global mcp.json (the user-owned file, never trust-gated) beside the fixture
// configuration, so a test needs no trusted workspace to start a server.
func useGlobalMCP(t *testing.T, servers map[string]any) {
	t.Helper()
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "lumencortex")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"servers": servers})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mcp.json")
	// The configuration directory is shared by every test of the package: put the file back as it was
	// so one test's servers are never another test's.
	previous, previousErr := os.ReadFile(path)
	t.Cleanup(func() {
		if previousErr == nil {
			_ = os.WriteFile(path, previous, 0o600)
			return
		}
		_ = os.Remove(path)
	})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// helperEnv makes the re-executed test binary act as the MCP server.
func helperEnv(t *testing.T, mode string) (marker, log string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "started")
	log = filepath.Join(dir, "calls.log")
	t.Setenv("LCX_TEST_MCP", "1")
	t.Setenv("LCX_TEST_MCP_MODE", mode)
	t.Setenv("LCX_TEST_MCP_MARKER", marker)
	t.Setenv("LCX_TEST_MCP_LOG", log)
	return marker, log
}

// childProcessCount counts the processes the test process owns, so "nothing started" is measured, not
// assumed.
func childProcessCount(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-o", "pid=", "-P", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return -1
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func openEngineWithGlobalConfig(t *testing.T, workspace string) *Engine {
	t.Helper()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	return engine
}

// capabilityByID finds one entry of the registry.
func capabilityByID(t *testing.T, capabilities []Capability, toolID string) Capability {
	t.Helper()
	for _, capability := range capabilities {
		if capability.ToolID == toolID {
			return capability
		}
	}
	t.Fatalf("capability %q not listed; have %s", toolID, capabilityIDs(capabilities))
	return Capability{}
}

func capabilityIDs(capabilities []Capability) string {
	ids := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		ids = append(ids, capability.ToolID)
	}
	return strings.Join(ids, ", ")
}

// ADR-0021 §2: no MCP server process exists when an Engine opens, even with every shipped template
// applied and enabled; the first call starts the server and the registry reports it running.
func TestMCPStartsOnlyOnFirstCall(t *testing.T) {
	marker, _ := helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", nil),
	})
	workspace := t.TempDir()
	before := childProcessCount(t)

	engine := openEngineWithGlobalConfig(t, workspace)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the server started at Engine open: %v", err)
	}
	if after := childProcessCount(t); after != before {
		t.Fatalf("child processes at open: before=%d after=%d", before, after)
	}
	states, err := engine.MCPServerStates(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Running || !states[0].Enabled {
		t.Fatalf("states after open = %#v, want one configured server, not running", states)
	}
	if statuses, err := engine.MCPStatuses(context.Background(), ""); err != nil || len(statuses) != 0 {
		t.Fatalf("statuses after open = %#v err=%v, want nothing running", statuses, err)
	}

	started := time.Now()
	result, err := engine.mcpRegistry.CallTool(context.Background(), workspace, "helper", "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	latency := time.Since(started)
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Fatalf("result = %#v", result)
	}
	t.Logf("stdio server start + first call latency: %s", latency)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the server did not start on the first call: %v", err)
	}
	statuses, err := engine.MCPStatuses(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || !statuses[0].Running || statuses[0].PID == 0 {
		t.Fatalf("statuses after the call = %#v", statuses)
	}
}

// ADR-0021 §2: a server that is not used is stopped after its idle timeout, and a later call starts a
// new one.
func TestMCPIdleTimeoutStopsServer(t *testing.T) {
	marker, _ := helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"idleTimeoutSeconds": 1}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	workspace := engine.workspace

	if _, err := engine.mcpRegistry.CallTool(context.Background(), workspace, "helper", "echo", map[string]any{"text": "one"}); err != nil {
		t.Fatal(err)
	}
	first, err := engine.MCPStatuses(context.Background(), "")
	if err != nil || len(first) != 1 || !first[0].Running {
		t.Fatalf("after first call statuses=%#v err=%v", first, err)
	}
	pid := first[0].PID

	deadline := time.Now().Add(15 * time.Second)
	for {
		states, err := engine.MCPServerStates(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		if len(states) == 1 && !states[0].Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the idle server was not stopped: %#v", states)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if statuses, err := engine.MCPStatuses(context.Background(), ""); err != nil || len(statuses) != 0 {
		t.Fatalf("statuses after the idle stop = %#v err=%v, want nothing running", statuses, err)
	}
	if processAlive(pid) {
		t.Fatalf("the idle server process %d is still alive", pid)
	}

	// The catalog the stopped server reported is still offered, so a run can plan without a start.
	capabilities, err := engine.Capabilities(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	echo := capabilityByID(t, capabilities, mcp.AgentToolName("helper", "echo"))
	if !echo.Enabled || echo.Availability.State != AvailabilityAvailable {
		t.Fatalf("echo after idle stop = %#v", echo)
	}

	if _, err := engine.mcpRegistry.CallTool(context.Background(), workspace, "helper", "echo", map[string]any{"text": "two"}); err != nil {
		t.Fatal(err)
	}
	second, err := engine.MCPStatuses(context.Background(), "")
	if err != nil || len(second) != 1 || !second[0].Running {
		t.Fatalf("after second call statuses=%#v err=%v", second, err)
	}
	if second[0].PID == pid {
		t.Fatalf("the restarted server reused pid %d", pid)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker: %v", err)
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	// Signal 0 probes existence without affecting the process.
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return process.Signal(nil) == nil
}

// ADR-0021 §2: a crashing server yields a tool result error and a lastError, and the run continues.
func TestMCPCrashIsAToolResultNotALoopFailure(t *testing.T) {
	helperEnv(t, "crash")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", nil),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	workspace := engine.workspace

	// The server starts (initialize and tools/list answer), then exits on the first call.
	_, err := engine.mcpRegistry.CallTool(context.Background(), workspace, "helper", "echo", map[string]any{"text": "boom"})
	if err == nil {
		t.Fatal("a crashed server must not report success")
	}
	states, err := engine.mcpRegistry.States(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].LastError == "" {
		t.Fatalf("states after crash = %#v, want a lastError", states)
	}
	if states[0].Running {
		t.Fatalf("a crashed server still reports running: %#v", states[0])
	}
	capabilities, err := engine.Capabilities(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	echo := capabilityByID(t, capabilities, mcp.AgentToolName("helper", "echo"))
	if echo.LastError == "" || echo.Availability.State != AvailabilityError {
		t.Fatalf("capability after crash = %#v", echo)
	}
	// The tool is still registered for a run: the failure is the tool's result, not a loop exit.
	if echo.Enabled != true {
		t.Fatalf("a crashed server's tool must stay configured: %#v", echo)
	}
}

// ADR-0021 §2: a hung call is cut by the server's call timeout, and a run's tool deadline cuts it
// earlier when it is sooner.
func TestMCPHungCallIsCutByTimeoutAndToolDeadline(t *testing.T) {
	helperEnv(t, "hang")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"callTimeoutSeconds": 1}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	workspace := engine.workspace

	started := time.Now()
	_, err := engine.mcpRegistry.CallTool(context.Background(), workspace, "helper", "echo", map[string]any{"text": "hang"})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a hung call must time out")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the call timeout did not cut the call: %s", elapsed)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("error = %v, want a timeout", err)
	}

	// The tool deadline is sooner than the server's timeout: the call stops at the deadline.
	deadlineCtx := protocol.WithToolDeadline(context.Background(), time.Now().Add(300*time.Millisecond))
	started = time.Now()
	_, err = engine.mcpRegistry.CallTool(deadlineCtx, workspace, "helper", "echo", map[string]any{"text": "hang"})
	elapsed = time.Since(started)
	if err == nil {
		t.Fatal("a call past the tool deadline must not report success")
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the tool deadline did not cut the call: %s", elapsed)
	}
}

// ADR-0021 §3: an enabled server exposes only its included tools; a disabled one exposes none; the
// names are namespaced by the server.
func TestMCPIncludeListAndDisableDecideExposure(t *testing.T) {
	helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"include": []string{"echo"}}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	workspace := engine.workspace

	if _, err := engine.mcpRegistry.CallTool(context.Background(), workspace, "helper", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	tools, err := engine.MCPTools(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].ToolName != "echo" || !strings.HasPrefix(tools[0].Name, "mcp__helper__") {
		t.Fatalf("tools = %#v, want only the included echo tool", tools)
	}

	if err := engine.SetMCPServerEnabled(context.Background(), mcp.ScopeGlobal, "helper", false); err != nil {
		t.Fatal(err)
	}
	tools, err = engine.MCPTools(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Fatalf("a disabled server still exposes tools: %#v", tools)
	}
	capabilities, err := engine.Capabilities(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	echo := capabilityByID(t, capabilities, mcp.AgentToolName("helper", "echo"))
	if echo.Enabled || echo.Availability.State != AvailabilityDisabled {
		t.Fatalf("disabled capability = %#v", echo)
	}

	if err := engine.SetMCPServerEnabled(context.Background(), mcp.ScopeGlobal, "helper", true); err != nil {
		t.Fatal(err)
	}
	tools, err = engine.MCPTools(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 {
		t.Fatalf("re-enabled server exposes %d tools", len(tools))
	}
}

// ADR-0021 §1: the registry lists built-in, optional and MCP capabilities with the permission and the
// source each one runs under.
func TestCapabilityRegistryListsEverySource(t *testing.T) {
	helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", nil),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	// The server's catalog is discovered by a call; until then its tools are unknown and a run is
	// offered none of them.
	if _, err := engine.mcpRegistry.CallTool(context.Background(), engine.workspace, "helper", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}

	capabilities, err := engine.Capabilities(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	readFile := capabilityByID(t, capabilities, "read_file")
	if readFile.Source != SourceBuiltin || readFile.Permission != "read" || !readFile.Enabled ||
		readFile.Availability.State != AvailabilityAvailable {
		t.Fatalf("read_file = %#v", readFile)
	}
	if readFile.Capability != "repo.read" || readFile.DisplayName != "Read File" {
		t.Fatalf("read_file labels = %#v", readFile)
	}
	bash := capabilityByID(t, capabilities, "bash")
	if bash.Permission != "exec" || !bash.ExecutionConstraints.RequiresApproval || !bash.ExecutionConstraints.MutatesWorkspace {
		t.Fatalf("bash = %#v", bash)
	}
	lsp := capabilityByID(t, capabilities, "lsp_definition")
	if lsp.Source != SourceCoreOptional || lsp.Enabled || lsp.Availability.State != AvailabilityNeedsConfiguration {
		t.Fatalf("lsp_definition = %#v", lsp)
	}
	echo := capabilityByID(t, capabilities, mcp.AgentToolName("helper", "echo"))
	if echo.Source != SourceLocalAdapter || echo.Provider != "helper" {
		t.Fatalf("echo source = %#v", echo)
	}
	if echo.Permission != mcp.PermissionRead || echo.ExecutionConstraints.RequiresApproval {
		t.Fatalf("echo permission = %#v", echo)
	}
	// A tool the server does not annotate read-only is exec, so a read-only run does not see it.
	mutate := capabilityByID(t, capabilities, mcp.AgentToolName("helper", "mutate"))
	if mutate.Permission != mcp.PermissionExec || !mutate.ExecutionConstraints.RequiresApproval {
		t.Fatalf("mutate = %#v", mutate)
	}
}

// ADR-0021 §1: the denylist is reported as Enabled=false with the reason, and the entry stays visible.
// A denylist saved before the rename names the command tool shell; it still denies bash.
func TestCapabilityDenylist(t *testing.T) {
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	capabilities, err := engine.Capabilities(context.Background(), CapabilityQuery{Denylist: []string{"shell", "read_file"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"bash", "read_file"} {
		capability := capabilityByID(t, capabilities, id)
		if capability.Enabled || capability.DisabledReason != "denylist" {
			t.Fatalf("%s = %#v", id, capability)
		}
	}
	if write := capabilityByID(t, capabilities, "write_file"); !write.Enabled {
		t.Fatalf("write_file = %#v, want enabled", write)
	}
}

// ADR-0021 §2-3: the catalog a previous start recorded is offered without a start, across a restart.
func TestMCPCatalogSurvivesRestart(t *testing.T) {
	helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", nil),
	})
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.mcpRegistry.CallTool(context.Background(), workspace, "helper", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	catalogDir := filepath.Join(engine.state.Path, "mcp-catalog")
	if entries, err := os.ReadDir(catalogDir); err != nil || len(entries) == 0 {
		t.Fatalf("no persisted catalog under %s: %v", catalogDir, err)
	}

	reopened := openEngineWithGlobalConfig(t, workspace)
	tools, err := reopened.MCPTools(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) == 0 {
		t.Fatal("the reopened Engine offers no tools from the recorded catalog")
	}
	for _, tool := range tools {
		if tool.ToolSource != mcp.ToolSourceCache {
			t.Fatalf("tool %s source = %q, want cache", tool.Name, tool.ToolSource)
		}
	}
}

// ADR-0021 §5: a session-bound server receives the runtime's session id and the server-side session it
// opened is closed when the server stops.
func TestMCPSessionBindingInjectsAndCloses(t *testing.T) {
	_, logPath := helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{
			"include": []string{"computer_start", "computer_stop"},
			"session": map[string]any{
				"argument":       "session_id",
				"startTool":      "computer_start",
				"sessionIdField": "computer_session_id",
				"stopTool":       "computer_stop",
				"stopArgument":   "computer_session_id",
			},
		}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	workspace := engine.workspace

	if _, err := engine.mcpRegistry.CallToolForSession(context.Background(), workspace, "helper", "computer_start", "session_lc_1",
		map[string]any{"session_id": "model_supplied"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"session_id":"session_lc_1"`) {
		t.Fatalf("the runtime session id was not injected: %s", raw)
	}
	if strings.Contains(string(raw), "model_supplied") {
		t.Fatalf("a model-supplied session id was kept: %s", raw)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "computer_stop") || !strings.Contains(string(raw), "cmp_test_1") {
		t.Fatalf("the server-side session was not closed on close: %s", raw)
	}
}

// ADR-0021 D5: the runtime closes a bound session only through a tool the server exposes. A stop tool
// outside the include list is never called, even by the runtime itself when the server stops.
func TestMCPBoundSessionStopToolMustBeIncluded(t *testing.T) {
	_, logPath := helperEnv(t, "")
	useGlobalMCP(t, map[string]any{"helper": sessionBoundHelper(t, map[string]any{"include": []string{"computer_start"}})})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	if _, err := engine.mcpRegistry.CallToolForSession(context.Background(), engine.workspace, "helper", "computer_start", "session_lc_1",
		map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "computer_start") {
		t.Fatalf("the start call did not reach the server: %s", raw)
	}
	if strings.Contains(string(raw), "computer_stop") {
		t.Fatalf("a stop tool outside the include list was called: %s", raw)
	}
}

// sessionBoundHelper is the helper server with the Computer Use session binding.
func sessionBoundHelper(t *testing.T, overrides map[string]any) map[string]any {
	t.Helper()
	settings := map[string]any{
		"include": []string{"computer_start", "computer_stop"},
		"session": map[string]any{
			"argument":       "session_id",
			"startTool":      "computer_start",
			"sessionIdField": "computer_session_id",
			"stopTool":       "computer_stop",
			"stopArgument":   "computer_session_id",
		},
	}
	for key, value := range overrides {
		settings[key] = value
	}
	return mcpTestServerConfig(t, "helper", settings)
}

// ADR-0021 D5: every path that stops a session-bound server closes the server-side sessions it
// opened — the idle stop, a disable, a reconfiguration and a delete, not only an explicit stop.
func TestMCPBoundSessionsCloseOnEveryStop(t *testing.T) {
	globalConfig := func(t *testing.T, engine *Engine) MCPConfig {
		t.Helper()
		configs, err := engine.MCPConfigsScope(mcp.ScopeGlobal)
		if err != nil || len(configs) != 1 {
			t.Fatalf("global configs = %#v err=%v", configs, err)
		}
		return configs[0]
	}
	paths := []struct {
		name      string
		overrides map[string]any
		stop      func(t *testing.T, engine *Engine)
	}{
		{name: "idle", overrides: map[string]any{"idleTimeoutSeconds": 1}, stop: func(*testing.T, *Engine) {}},
		{name: "disable", stop: func(t *testing.T, engine *Engine) {
			if err := engine.SetMCPServerEnabled(context.Background(), mcp.ScopeGlobal, "helper", false); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "reconfigure", stop: func(t *testing.T, engine *Engine) {
			cfg := globalConfig(t, engine)
			cfg.CallTimeoutSeconds = 30
			if err := engine.MCPUpsertConfigScope(mcp.ScopeGlobal, cfg); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "delete", stop: func(t *testing.T, engine *Engine) {
			if err := engine.MCPDeleteConfigScope(mcp.ScopeGlobal, "helper"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			_, logPath := helperEnv(t, "")
			useGlobalMCP(t, map[string]any{"helper": sessionBoundHelper(t, path.overrides)})
			engine := openEngineWithGlobalConfig(t, t.TempDir())
			if _, err := engine.mcpRegistry.CallToolForSession(context.Background(), engine.workspace, "helper",
				"computer_start", "session_lc_1", map[string]any{}); err != nil {
				t.Fatal(err)
			}
			path.stop(t, engine)
			// The stop call is made before the process is closed; poll the server's log rather than
			// guess when the idle timer fired.
			deadline := time.Now().Add(10 * time.Second)
			for {
				raw, _ := os.ReadFile(logPath)
				if strings.Contains(string(raw), `computer_stop {"computer_session_id":"cmp_test_1"}`) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("%s left the server-side session open: %s", path.name, raw)
				}
				time.Sleep(50 * time.Millisecond)
			}
			if statuses, err := engine.MCPStatuses(context.Background(), ""); err != nil || len(statuses) != 0 {
				t.Fatalf("after %s statuses=%#v err=%v, want the server stopped", path.name, statuses, err)
			}
		})
	}
}

// ADR-0021 D3: the idle timer never stops a server while one of its calls is in flight; the call ends
// at its own call timeout.
func TestMCPIdleTimerSparesInFlightCall(t *testing.T) {
	helperEnv(t, "hang")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"idleTimeoutSeconds": 1, "callTimeoutSeconds": 6}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	started := time.Now()
	_, err := engine.mcpRegistry.CallTool(context.Background(), engine.workspace, "helper", "echo", map[string]any{"text": "slow"})
	elapsed := time.Since(started)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("after %s the call ended with %v, want its call timeout (the idle timer stopped the server)", elapsed, err)
	}
	if elapsed < 5*time.Second {
		t.Fatalf("the call ended after %s, before its 6 s call timeout", elapsed)
	}
}

// ADR-0013 / ADR-0021 §3: a catalog recorded for a project server is keyed by everything that decides
// the process, its environment included. After an edit of the entry's env alone the recorded catalog is
// withdrawn: no tool is offered from it and nothing is started until the host starts the server again.
func TestMCPProjectEnvEditWithdrawsCachedCatalog(t *testing.T) {
	marker, _ := helperEnv(t, "")
	workspace := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	writeEngineMCP(t, workspace, map[string]any{"proj": mcpTestServerConfig(t, "proj", nil)})
	useGlobalConfig(t, mcpGlobalConfig(workspace))
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := engine.MCPStart(ctx, "", "proj"); err != nil {
		t.Fatal(err)
	}
	if tools, err := engine.MCPTools(ctx, ""); err != nil || len(tools) == 0 {
		t.Fatalf("tools after the host start = %#v err=%v", tools, err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	injected := filepath.Join(t.TempDir(), "injected")
	writeEngineMCP(t, workspace, map[string]any{
		"proj": mcpTestServerConfig(t, "proj", map[string]any{"env": map[string]string{"LCX_TEST_MCP_MARKER": injected}}),
	})
	reopened := openEngineWithGlobalConfig(t, workspace)
	if tools, err := reopened.MCPTools(ctx, ""); err != nil || len(tools) != 0 {
		t.Fatalf("after an env-only edit tools = %#v err=%v, want the recorded catalog withdrawn", tools, err)
	}
	handle := newTestSession(t, reopened, "use the project server")
	provider := &notebookProvider{}
	if _, err := reopened.runAgentWith(ctx, handle.ID, provider, AgentOptions{Policy: "read-only", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	for _, spec := range provider.requests[0].Tools {
		if strings.HasPrefix(spec.Name, mcp.AgentToolPrefix("proj")) {
			t.Fatalf("a run was offered %s from the withdrawn catalog", spec.Name)
		}
	}
	for _, path := range []string{marker, injected} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the edited project server started without a host start (%s): %v", path, err)
		}
	}
}

// hostStartedProjectServer opens an Engine on a trusted workspace whose project mcp.json names the
// helper as "proj" with include [echo], starts it as the host does, runs host, and closes the Engine,
// so the catalog the start recorded is all a later Engine has. It returns the workspace, the helper's
// start marker (removed) and its call log.
func hostStartedProjectServer(t *testing.T, host func(engine *Engine)) (workspace, marker, logPath string) {
	t.Helper()
	marker, logPath = helperEnv(t, "")
	workspace = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	writeEngineMCP(t, workspace, map[string]any{"proj": mcpTestServerConfig(t, "proj", map[string]any{"include": []string{"echo"}})})
	useGlobalConfig(t, mcpGlobalConfig(workspace))
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.MCPStart(context.Background(), "", "proj"); err != nil {
		t.Fatal(err)
	}
	host(engine)
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	return workspace, marker, logPath
}

// ADR-0013 / ADR-0021 §3: the catalog a host start recorded for a project server is keyed by the
// entry's exposure too — include, tools overrides, disabled and the session binding. An edit of the
// project mcp.json that did not go through the Engine (a run's write_file) withdraws it: a later
// read-only run is offered nothing from it, its calls start no server, and no tool outside the include
// list runs — neither the relabelled one nor a stop tool the edit bound, when the server stops.
func TestMCPProjectExposureEditWithdrawsCachedCatalog(t *testing.T) {
	cases := map[string]struct {
		host   func(t *testing.T, engine *Engine)
		edited map[string]any
	}{
		"include and permission": {
			host:   func(*testing.T, *Engine) {},
			edited: map[string]any{"include": []string{"echo", "mutate"}, "tools": map[string]any{"mutate": map[string]any{"permission": "read"}}},
		},
		"disabled by the host, enabled by the file": {
			host: func(t *testing.T, engine *Engine) {
				if err := engine.SetMCPServerEnabled(context.Background(), mcp.ScopeProject, "proj", false); err != nil {
					t.Fatal(err)
				}
			},
			edited: map[string]any{"include": []string{"echo"}},
		},
		"session binding with an excluded stop tool": {
			host: func(*testing.T, *Engine) {},
			edited: map[string]any{"include": []string{"echo"}, "session": map[string]any{
				"argument": "session_id", "startTool": "echo", "sessionIdField": "sid",
				"stopTool": "mutate", "stopArgument": "value",
			}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			workspace, marker, logPath := hostStartedProjectServer(t, func(engine *Engine) { tc.host(t, engine) })
			writeEngineMCP(t, workspace, map[string]any{"proj": mcpTestServerConfig(t, "proj", tc.edited)})
			reopened := openEngineWithGlobalConfig(t, workspace)
			ctx := context.Background()
			if tools, err := reopened.MCPTools(ctx, ""); err != nil || len(tools) != 0 {
				t.Fatalf("after an exposure edit outside the Engine tools = %#v err=%v, want the recorded catalog withdrawn", tools, err)
			}
			handle := newTestSession(t, reopened, "use the project server")
			provider := &notebookProvider{responses: []protocol.ProviderResponse{
				toolCallResponse("echo_1", mcp.AgentToolName("proj", "echo"), `{"text":"{\"sid\":\"PAYLOAD-from-read-only-run\"}"}`),
				toolCallResponse("mutate_1", mcp.AgentToolName("proj", "mutate"), `{"value":"x"}`),
			}}
			if _, err := reopened.runAgentWith(ctx, handle.ID, provider, AgentOptions{Policy: "read-only", MaxSteps: 4}); err != nil {
				t.Fatal(err)
			}
			for _, spec := range provider.requests[0].Tools {
				if strings.HasPrefix(spec.Name, mcp.AgentToolPrefix("proj")) {
					t.Fatalf("a read-only run was offered %s (%s) from the withdrawn catalog", spec.Name, spec.Permission)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("the edited project server started without a host start: %v", err)
			}
			// A server that did start is stopped here, which is when a bound stop tool would run.
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if raw, _ := os.ReadFile(logPath); strings.Contains(string(raw), "mutate") {
				t.Fatalf("the relabelled tool ran: %s", raw)
			}
		})
	}
}

// ADR-0021 §3: an exposure edit the host makes through the Engine — a tool switched on, a permission
// override, the server disabled and enabled again — keeps the recorded catalog: the next Engine offers
// the edited exposure from it without starting the server.
func TestMCPHostExposureEditKeepsCachedCatalog(t *testing.T) {
	workspace, marker, _ := hostStartedProjectServer(t, func(engine *Engine) {
		ctx := context.Background()
		if err := engine.SetCapabilityEnabled(ctx, mcp.AgentToolName("proj", "mutate"), true); err != nil {
			t.Fatal(err)
		}
		configs, err := engine.MCPConfigsScope(mcp.ScopeProject)
		if err != nil || len(configs) != 1 {
			t.Fatalf("project configs = %#v err=%v", configs, err)
		}
		cfg := configs[0]
		cfg.Tools = map[string]mcp.ToolConfig{"mutate": {Permission: mcp.PermissionRead}}
		if err := engine.MCPUpsertConfigScope(mcp.ScopeProject, cfg); err != nil {
			t.Fatal(err)
		}
		for _, enabled := range []bool{false, true} {
			if err := engine.SetMCPServerEnabled(ctx, mcp.ScopeProject, "proj", enabled); err != nil {
				t.Fatal(err)
			}
		}
	})
	reopened := openEngineWithGlobalConfig(t, workspace)
	tools, err := reopened.MCPTools(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var mutate *MCPAgentTool
	for index := range tools {
		if tools[index].Name == mcp.AgentToolName("proj", "mutate") {
			mutate = &tools[index]
		}
	}
	if mutate == nil || mutate.Permission != mcp.PermissionRead || mutate.ToolSource != mcp.ToolSourceCache {
		t.Fatalf("after host edits tools = %#v, want mutate offered read from the recorded catalog", tools)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("listing the tools started the server: %v", err)
	}
}

// ADR-0021 §3: a run discovers the catalog only of a server whose tools it could be offered. A server
// whose every included tool the run's denylist names, or that the run's allowlist does not reach, is
// not started; a run that may use the server still discovers it.
func TestMCPRunSkipsDiscoveryOfAServerItCannotUse(t *testing.T) {
	marker, _ := helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"include": []string{"echo", "mutate"}}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	ctx := context.Background()
	run := func(opts AgentOptions) {
		t.Helper()
		handle := newTestSession(t, engine, "no server needed")
		result, err := engine.runAgentWith(ctx, handle.ID, &notebookProvider{}, opts)
		if err != nil || result.Status != "completed" {
			t.Fatalf("run = %#v err=%v", result, err)
		}
	}
	for name, opts := range map[string]AgentOptions{
		"denylist names every included tool": {Policy: "read-only", MaxSteps: 2, ToolDenylist: []string{
			mcp.AgentToolName("helper", "echo"), mcp.AgentToolName("helper", "mutate"),
		}},
		"allowlist names no tool of the server": {Policy: "read-only", MaxSteps: 2, ToolAllowlist: []string{"read_file"}},
	} {
		run(opts)
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("%s: the run started a server it cannot use: %v", name, err)
		}
	}
	run(AgentOptions{Policy: "read-only", MaxSteps: 2})
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("a run that may use the server did not discover it: %v", err)
	}
}

// The shipped templates are data the loader accepts, they are disabled, and they carry no credential.
func TestMCPTemplatesAreValidDisabledEntries(t *testing.T) {
	templates := mcptemplates.All()
	if len(templates) < 5 {
		t.Fatalf("templates = %d, want the five shipped ones", len(templates))
	}
	seen := map[string]bool{}
	for _, template := range templates {
		if seen[template.ID] {
			t.Fatalf("duplicate template id %s", template.ID)
		}
		seen[template.ID] = true
		if err := mcptemplates.Validate(template); err != nil {
			t.Fatalf("template %s: %v", template.ID, err)
		}
		if !template.Config.Disabled {
			t.Fatalf("template %s is enabled by default", template.ID)
		}
	}
	for _, id := range []string{"exa", "github", "browser", "computer-use", "codegraph"} {
		if !seen[id] {
			t.Fatalf("template %s is missing", id)
		}
	}
}

// Applying a template writes a disabled entry the loader reads back, and never overwrites an entry.
func TestMCPTemplateAppliesAsDisabled(t *testing.T) {
	useGlobalMCP(t, map[string]any{})
	engine := openEngineWithGlobalConfig(t, t.TempDir())

	cfg, err := engine.ApplyMCPTemplate(context.Background(), mcp.ScopeGlobal, "browser")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Disabled || cfg.Template != "browser" {
		t.Fatalf("applied config = %#v", cfg)
	}
	if _, err := engine.ApplyMCPTemplate(context.Background(), mcp.ScopeGlobal, "browser"); err == nil {
		t.Fatal("applying the same template twice must be refused")
	}
	if _, err := engine.ApplyMCPTemplate(context.Background(), mcp.ScopeGlobal, "nope"); err == nil {
		t.Fatal("an unknown template must be refused")
	}
	configs, err := engine.MCPConfigsScope(mcp.ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 || configs[0].ID != "browser" || !configs[0].Disabled {
		t.Fatalf("global configs = %#v", configs)
	}
	// The entry the template wrote is one the real loader reads back unchanged.
	reloaded, err := mcp.NewLayeredRegistry(engine.workspace, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "lumencortex", "mcp.json"), "")
	if err != nil {
		t.Fatalf("the loader refused the applied template: %v", err)
	}
	defer reloaded.Close()
	loaded := reloaded.Configs()
	if len(loaded) != 1 || loaded[0].Template != "browser" || !loaded[0].Disabled {
		t.Fatalf("reloaded configs = %#v", loaded)
	}
	// The diagnostic for a template whose binary is absent names the binary, without starting anything.
	states, err := engine.mcpRegistry.States(engine.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].Enabled || states[0].Running {
		t.Fatalf("states = %#v", states)
	}
}

// One tool of an MCP server can be switched off without touching the others, and the switch is written
// to the scope the entry came from.
func TestSetCapabilityEnabledEditsTheIncludeList(t *testing.T) {
	helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", nil),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	ctx := context.Background()
	if _, err := engine.mcpRegistry.CallTool(ctx, engine.workspace, "helper", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}

	if err := engine.SetCapabilityEnabled(ctx, mcp.AgentToolName("helper", "mutate"), false); err != nil {
		t.Fatal(err)
	}
	configs, err := engine.MCPConfigsScope(mcp.ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("configs = %#v", configs)
	}
	include := strings.Join(configs[0].Include, ",")
	if strings.Contains(include, "mutate") || !strings.Contains(include, "echo") {
		t.Fatalf("include after disabling one tool = %v", configs[0].Include)
	}
	capabilities, err := engine.Capabilities(ctx, CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found := capabilityLookup(capabilities, mcp.AgentToolName("helper", "mutate")); found {
		t.Fatal("a tool removed from the include list is still offered")
	}
	if capability, found := capabilityLookup(capabilities, mcp.AgentToolName("helper", "echo")); !found || !capability.Enabled {
		t.Fatalf("echo = %#v found=%v", capability, found)
	}

	if err := engine.SetCapabilityEnabled(ctx, mcp.AgentToolName("helper", "mutate"), true); err != nil {
		t.Fatal(err)
	}
	configs, err = engine.MCPConfigsScope(mcp.ScopeGlobal)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(configs[0].Include, ","), "mutate") {
		t.Fatalf("include after re-enabling = %v", configs[0].Include)
	}
	if err := engine.SetCapabilityEnabled(ctx, "read_file", false); err == nil {
		t.Fatal("a built-in tool must not be switchable through an MCP server's list")
	}
}

// A tool whose name the model-visible id sanitises (page.read -> mcp__helper__page_read) is switched
// back on under the name the server reports, found in the server's catalog; an id that names no tool
// of a server whose catalog is known is refused rather than written as a guessed include entry.
func TestSetCapabilityEnabledReenablesSanitizedToolName(t *testing.T) {
	helperEnv(t, "")
	t.Setenv("LCX_TEST_MCP_EXTRA_TOOL", "page.read")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", nil),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	ctx := context.Background()
	if _, err := engine.mcpRegistry.CallTool(ctx, engine.workspace, "helper", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	pageRead := mcp.AgentToolName("helper", "page.read")
	if err := engine.SetCapabilityEnabled(ctx, pageRead, false); err != nil {
		t.Fatal(err)
	}
	if err := engine.SetCapabilityEnabled(ctx, pageRead, true); err != nil {
		t.Fatal(err)
	}
	configs, err := engine.MCPConfigsScope(mcp.ScopeGlobal)
	if err != nil || len(configs) != 1 {
		t.Fatalf("configs = %#v err=%v", configs, err)
	}
	if !slices.Contains(configs[0].Include, "page.read") || slices.Contains(configs[0].Include, "page_read") {
		t.Fatalf("include after re-enabling %s = %v, want the server's name page.read", pageRead, configs[0].Include)
	}
	tools, err := engine.MCPTools(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(tools, func(tool MCPAgentTool) bool { return tool.Name == pageRead }) {
		t.Fatalf("after re-enabling tools = %#v, want %s offered", tools, pageRead)
	}
	before := configs[0].Include
	if err := engine.SetCapabilityEnabled(ctx, mcp.AgentToolName("helper", "no_such_tool"), true); err == nil {
		t.Fatal("re-enabling a tool the server's catalog does not list must fail")
	}
	configs, err = engine.MCPConfigsScope(mcp.ScopeGlobal)
	if err != nil || !slices.Equal(configs[0].Include, before) {
		t.Fatalf("a refused re-enable changed the include list: %v -> %v (err=%v)", before, configs[0].Include, err)
	}
}

// capabilityLookup finds one entry, reporting whether it is there.
func capabilityLookup(capabilities []Capability, toolID string) (Capability, bool) {
	for _, capability := range capabilities {
		if capability.ToolID == toolID {
			return capability, true
		}
	}
	return Capability{}, false
}

// The byte cost of tool schemas: built-ins only, then each configured server with its include list.
func TestToolSchemaCost(t *testing.T) {
	helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"include": []string{"echo"}}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())

	builtinOnly, err := engine.ToolSchemaCost(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if builtinOnly.BuiltinBytes <= 0 || builtinOnly.LSPBytes != 0 {
		t.Fatalf("built-in cost = %#v", builtinOnly)
	}
	if builtinOnly.MCPBytes["helper"] != 0 {
		t.Fatalf("a server that was never started contributes %d bytes", builtinOnly.MCPBytes["helper"])
	}
	if _, err := engine.mcpRegistry.CallTool(context.Background(), engine.workspace, "helper", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}
	withServer, err := engine.ToolSchemaCost(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if withServer.MCPBytes["helper"] <= 0 || withServer.TotalBytes <= builtinOnly.TotalBytes {
		t.Fatalf("cost with the server = %#v (built-in only %#v)", withServer, builtinOnly)
	}
	t.Logf("tool schema bytes: built-ins %d (%d tools), + helper %d (%d tools), total %d",
		withServer.BuiltinBytes, withServer.BuiltinTools, withServer.MCPBytes["helper"], withServer.MCPTools["helper"], withServer.TotalBytes)
}

// A server named by an untrusted project file still spawns nothing and is reported as untrusted.
func TestMCPCapabilityUntrustedProjectEntry(t *testing.T) {
	helperEnv(t, "")
	workspace := t.TempDir()
	marker := os.Getenv("LCX_TEST_MCP_MARKER")
	writeEngineMCP(t, workspace, map[string]any{
		"project": mcpTestServerConfig(t, "project", map[string]any{"include": []string{"echo"}}),
	})
	useGlobalConfig(t, mcpGlobalConfig())
	engine := openEngineWithGlobalConfig(t, workspace)

	capabilities, err := engine.Capabilities(context.Background(), CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, capability := range capabilities {
		if capability.Provider != "project" {
			continue
		}
		if capability.Enabled || capability.Availability.State != AvailabilityUntrusted {
			t.Fatalf("untrusted capability = %#v", capability)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("an untrusted project server started: %v", err)
	}
}

var _ = fmt.Sprintf

// ADR-0021 D3: a crashing server inside a real run is a tool result; the run continues and answers.
func TestMCPCrashInsideARunContinuesTheLoop(t *testing.T) {
	helperEnv(t, "crash")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", nil),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	ctx := context.Background()
	handle := newTestSession(t, engine, "call the helper")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{{
			ID:        "call_1",
			Name:      mcp.AgentToolName("helper", "echo"),
			Arguments: json.RawMessage(`{"text":"boom"}`),
		}}}},
		{Message: protocol.Message{Content: "the helper failed, moving on"}},
	}}
	result, err := engine.runAgentWith(ctx, handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 4})
	if err != nil {
		t.Fatalf("a crashing MCP server ended the run: %v", err)
	}
	if result.Status != "completed" || result.Final != "the helper failed, moving on" {
		t.Fatalf("result = %#v", result)
	}
	messages, err := handle.RecentMessages(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		raw := string(message.JSON)
		// The tool row carries the failure as a JSON object inside the content string, so the quotes are
		// escaped in the row.
		if strings.Contains(raw, "mcp tool helper/echo failed") && strings.Contains(raw, `serverId`) &&
			strings.Contains(raw, "helper") {
			found = true
		}
	}
	if !found {
		t.Fatal("the tool result does not carry the failure")
	}
	states, err := engine.mcpRegistry.States(engine.workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states[0].LastError == "" {
		t.Fatalf("states after the run = %#v, want a lastError", states)
	}
}

// ADR-0021 D4: a disabled server's tools are absent from a step's tool list, and an enabled one's are
// present with the namespaced name and the effective permission.
func TestMCPDisabledServerIsAbsentFromTheStepToolList(t *testing.T) {
	helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"include": []string{"echo"}}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	ctx := context.Background()
	if _, err := engine.mcpRegistry.CallTool(ctx, engine.workspace, "helper", "echo", map[string]any{"text": "x"}); err != nil {
		t.Fatal(err)
	}

	stepTools := func() map[string]bool {
		tools, err := toolset.New(toolset.Options{
			Workspace: engine.workspace,
			Policy:    "full",
			MCP:       engine.mcpRegistry,
		})
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, spec := range tools.Specs(nil) {
			names[spec.Name] = true
		}
		return names
	}
	enabled := stepTools()
	if !enabled[mcp.AgentToolName("helper", "echo")] {
		t.Fatalf("an enabled server's tool is not in the step list: %v", enabled)
	}
	if enabled[mcp.AgentToolName("helper", "mutate")] {
		t.Fatal("a tool outside the include list reached the step list")
	}

	if err := engine.SetMCPServerEnabled(ctx, mcp.ScopeGlobal, "helper", false); err != nil {
		t.Fatal(err)
	}
	disabled := stepTools()
	if disabled[mcp.AgentToolName("helper", "echo")] {
		t.Fatalf("a disabled server's tool is in the step list: %v", disabled)
	}
	if !disabled["read_file"] {
		t.Fatal("disabling a server removed a built-in tool")
	}
}

// ADR-0021 §3: a run that may use MCP offers a server's tools on its first run — the catalog is
// discovered then, not at Engine open — and the model's call reaches the server.
func TestMCPRunDiscoversTheCatalogAndCallsTheTool(t *testing.T) {
	helperEnv(t, "")
	useGlobalMCP(t, map[string]any{
		"helper": mcpTestServerConfig(t, "helper", map[string]any{"include": []string{"echo"}}),
	})
	engine := openEngineWithGlobalConfig(t, t.TempDir())
	ctx := context.Background()
	handle := newTestSession(t, engine, "call the helper")
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{{
			ID:        "call_1",
			Name:      mcp.AgentToolName("helper", "echo"),
			Arguments: json.RawMessage(`{"text":"through the run"}`),
		}}}},
		{Message: protocol.Message{Content: "done"}},
	}}
	result, err := engine.runAgentWith(ctx, handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("result = %#v", result)
	}
	messages, err := handle.RecentMessages(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range messages {
		if strings.Contains(string(message.JSON), "through the run") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the server's answer is not in the transcript: %#v", messages)
	}
}
