package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/protocol"
)

// The runtime tests drive a real language server process: the testdata server of internal/lsp, built
// once per test binary.
var (
	fakeLSPOnce sync.Once
	fakeLSPPath string
	fakeLSPErr  error
)

func fakeLSPServer(t *testing.T) string {
	t.Helper()
	fakeLSPOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lc-runtime-lsp-")
		if err != nil {
			fakeLSPErr = err
			return
		}
		_, file, _, _ := runtime.Caller(0)
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "fake-lsp"), "./internal/lsp/testdata/fakeserver")
		build.Dir = filepath.Dir(filepath.Dir(file))
		if out, err := build.CombinedOutput(); err != nil {
			fakeLSPErr = fmt.Errorf("build fake server: %w: %s", err, out)
			return
		}
		fakeLSPPath = filepath.Join(dir, "fake-lsp")
	})
	if fakeLSPErr != nil {
		t.Fatal(fakeLSPErr)
	}
	return fakeLSPPath
}

// lspConfig is a global configuration that names the fake server for Go, with the given extra lsp
// keys.
func lspConfig(command string, extra ...string) string {
	keys := fmt.Sprintf(`"lsp": {"servers": {"go": {"command": %q}}}`, command)
	for _, e := range extra {
		keys += ", " + e
	}
	return modelConfig(keys)
}

func lspWorkspace(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return workspace
}

func waitProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d still exists after %s", pid, timeout)
}

// LSPStatus reports what the workspace's language servers are, and starting nothing is part of the
// contract: a host asks for status at every window load.
func TestLSPStatusStartsNothing(t *testing.T) {
	server := fakeLSPServer(t)
	useGlobalConfig(t, lspConfig(server))
	workspace := lspWorkspace(t)
	engine := openEngineAt(t, workspace)
	statuses, err := engine.LSPStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 {
		t.Fatalf("statuses=%+v", statuses)
	}
	if status := statuses[0]; status.Language != "go" || status.State != lsp.StateStopped || status.PID != 0 ||
		status.Root != workspace || status.StartedAt != nil || status.LastUsedAt != nil {
		t.Fatalf("status=%+v", status)
	}
	raw, err := json.Marshal(statuses[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"language"`, `"root"`, `"state"`, `"command"`} {
		if !strings.Contains(string(raw), key) {
			t.Fatalf("status JSON %s lacks %s", raw, key)
		}
	}
	if strings.Contains(string(raw), `"Language"`) {
		t.Fatalf("status JSON is not camelCase: %s", raw)
	}
}

// A server starts on the first request, serves the next one from the same process, and is gone after
// LSPStop: the runtime owns no language server that a host did not ask for.
func TestLSPStartsOnDemandAndStops(t *testing.T) {
	server := fakeLSPServer(t)
	useGlobalConfig(t, lspConfig(server))
	workspace := lspWorkspace(t)
	engine := openEngineAt(t, workspace)
	// The status reports the pid of the process the manager started; no start log is needed.
	if _, err := engine.LSPHover(context.Background(), "", "main.go", 1, 1); err != nil {
		t.Fatal(err)
	}
	statuses, err := engine.LSPStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if statuses[0].State != lsp.StateIdle || statuses[0].PID == 0 {
		t.Fatalf("status after the first request=%+v", statuses[0])
	}
	pid := statuses[0].PID

	if _, err := engine.LSPHover(context.Background(), "", "main.go", 1, 1); err != nil {
		t.Fatal(err)
	}
	if statuses, err = engine.LSPStatus(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if statuses[0].PID != pid {
		t.Fatalf("the second request started another server: %d -> %d", pid, statuses[0].PID)
	}

	if err := engine.LSPStop(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, pid, 5*time.Second)
	if statuses, err = engine.LSPStatus(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if statuses[0].State != lsp.StateStopped || statuses[0].PID != 0 {
		t.Fatalf("status after LSPStop=%+v", statuses[0])
	}

	// A stopped manager starts again on the next request.
	if _, err := engine.LSPHover(context.Background(), "", "main.go", 1, 1); err != nil {
		t.Fatal(err)
	}
	if statuses, err = engine.LSPStatus(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if statuses[0].PID == 0 || statuses[0].PID == pid {
		t.Fatalf("status after the restart=%+v, old pid %d", statuses[0], pid)
	}

	// An explicit start is the host's path: it starts the server now, without a request.
	if err := engine.LSPStop(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	explicit, err := engine.LSPStart(context.Background(), "", LSPConfig{Command: server, LanguageID: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if explicit.PID == 0 || !explicit.Running {
		t.Fatalf("explicit start=%+v", explicit)
	}
	if err := engine.LSPStop(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, explicit.PID, 5*time.Second)
}

// A workspace whose server command does not exist reports it as unavailable with the reason, and asks
// nothing of the model: no server is started.
func TestLSPStatusReportsAMissingServer(t *testing.T) {
	useGlobalConfig(t, lspConfig("lcx-no-such-language-server"))
	engine := openEngineAt(t, lspWorkspace(t))
	statuses, err := engine.LSPStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].State != lsp.StateUnavailable ||
		!strings.Contains(statuses[0].LastError, "not found") {
		t.Fatalf("statuses=%+v", statuses)
	}
	if _, err := engine.LSPHover(context.Background(), "", "main.go", 1, 1); !errors.Is(err, lsp.ErrUnavailable) {
		t.Fatalf("err=%v, want ErrUnavailable", err)
	}
}

// Closing the Engine leaves no language server behind.
func TestEngineCloseStopsLanguageServers(t *testing.T) {
	server := fakeLSPServer(t)
	useGlobalConfig(t, lspConfig(server))
	workspace := lspWorkspace(t)
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.LSPHover(context.Background(), "", "main.go", 1, 1); err != nil {
		t.Fatal(err)
	}
	statuses, err := engine.LSPStatus(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	pid := statuses[0].PID
	if pid == 0 {
		t.Fatalf("status=%+v", statuses[0])
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, pid, 5*time.Second)
}

// lspRunOffers runs a read-only agent turn whose model calls lsp_hover on main.go, and returns whether
// the first request offered lsp_hover and the content of the tool result.
func lspRunOffers(t *testing.T, engine *Engine) (offered bool, result string) {
	t.Helper()
	ctx := context.Background()
	handle := newTestSession(t, engine, "hover main.go")
	provider := &notebookProvider{responses: []protocol.ProviderResponse{
		toolCallResponse("hover_1", "lsp_hover", `{"path":"main.go","line":1,"column":1}`),
		finalResponse("done"),
	}}
	run, err := engine.runAgentWith(ctx, handle.ID, provider, AgentOptions{Policy: "read-only", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("run = %#v", run)
	}
	provider.mu.Lock()
	first := provider.requests[0]
	provider.mu.Unlock()
	for _, spec := range first.Tools {
		if strings.HasPrefix(spec.Name, "lsp_") {
			offered = true
		}
	}
	for _, message := range provider.messages() {
		if message.Role == "tool" && message.ToolCallID == "hover_1" {
			result = message.Content
		}
	}
	return offered, result
}

// lspReports says whether the capability registry, the schema cost and the prompt report each count
// the lsp_* tools for a read-only run.
func lspReports(t *testing.T, engine *Engine) (capability, cost, prompt bool) {
	t.Helper()
	ctx := context.Background()
	capabilities, err := engine.Capabilities(ctx, CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	capability = capabilityByID(t, capabilities, "lsp_hover").Enabled
	schema, err := engine.ToolSchemaCost(ctx, CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	cost = schema.LSPTools > 0
	report, err := engine.EffectivePrompt(ctx, EffectivePromptRequest{Policy: "read-only"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range report.Tools {
		if tool.Name == "lsp_hover" && tool.Included {
			prompt = true
		}
	}
	return capability, cost, prompt
}

// ADR-0013: a run may start a language server only in a trusted workspace — a server such as
// typescript-language-server loads code from the repository it serves. In an untrusted workspace a
// run uses only a server the host started, and is offered no lsp_* tool otherwise; the capability
// registry, the schema cost and the prompt report say the same as the run.
func TestLSPToolsWithheldInUntrustedWorkspace(t *testing.T) {
	server := fakeLSPServer(t)
	workspace := lspWorkspace(t)
	if resolved, err := filepath.EvalSymlinks(workspace); err == nil {
		workspace = resolved
	}
	// The runs bind fakes to the fixture catalog's models, so the lsp keys go into that catalog.
	catalog := func(keys string) string {
		return strings.Replace(boundCatalog("test/b0"), `{"model"`, `{`+keys+`, "model"`, 1)
	}
	lspKeys := fmt.Sprintf(`"lsp": {"servers": {"go": {"command": %q}}}`, server)
	useGlobalConfig(t, catalog(lspKeys))
	engine := openEngineAt(t, workspace)
	ctx := context.Background()
	status := func() LSPServerStatus {
		t.Helper()
		statuses, err := engine.LSPStatus(ctx, "")
		if err != nil || len(statuses) != 1 {
			t.Fatalf("statuses=%+v err=%v", statuses, err)
		}
		return statuses[0]
	}

	// Untrusted, nothing running: no tool offered, and the model's call starts nothing.
	offered, result := lspRunOffers(t, engine)
	if offered {
		t.Fatal("an untrusted workspace's run was offered the lsp tools")
	}
	if strings.Contains(result, "hover-ok") {
		t.Fatalf("an untrusted run reached a language server: %s", result)
	}
	if got := status(); got.State != lsp.StateStopped || got.PID != 0 {
		t.Fatalf("an untrusted run started a language server: %+v", got)
	}
	if capability, cost, prompt := lspReports(t, engine); capability || cost || prompt {
		t.Fatalf("untrusted reports: capability=%v cost=%v prompt=%v, want none", capability, cost, prompt)
	}
	capabilities, err := engine.Capabilities(ctx, CapabilityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if hover := capabilityByID(t, capabilities, "lsp_hover"); hover.Availability.State != AvailabilityUntrusted || hover.DisabledReason != "trust" {
		t.Fatalf("untrusted lsp_hover capability = %+v, want untrusted/trust", hover)
	}

	// Untrusted, a server the host started: the run uses that process.
	started, err := engine.LSPStart(ctx, "", LSPConfig{Command: server, LanguageID: "go"})
	if err != nil {
		t.Fatal(err)
	}
	offered, result = lspRunOffers(t, engine)
	if !offered || !strings.Contains(result, "hover-ok") {
		t.Fatalf("a host-started server in an untrusted workspace: offered=%v result=%s", offered, result)
	}
	if got := status(); got.PID != started.PID {
		t.Fatalf("the run replaced the host's server: %d -> %+v", started.PID, got)
	}
	if capability, cost, prompt := lspReports(t, engine); !capability || !cost || !prompt {
		t.Fatalf("host-started reports: capability=%v cost=%v prompt=%v, want all", capability, cost, prompt)
	}
	if err := engine.LSPStop(ctx, ""); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, started.PID, 5*time.Second)

	// Trusted: offered as before, and the first call starts the server.
	useGlobalConfig(t, catalog(lspKeys+`, "trustedWorkspaces": [`+quoteJSON(workspace)+`]`))
	offered, result = lspRunOffers(t, engine)
	if !offered || !strings.Contains(result, "hover-ok") {
		t.Fatalf("trusted workspace: offered=%v result=%s", offered, result)
	}
	if got := status(); got.PID == 0 {
		t.Fatalf("a trusted run's call did not start the server: %+v", got)
	}
	if capability, cost, prompt := lspReports(t, engine); !capability || !cost || !prompt {
		t.Fatalf("trusted reports: capability=%v cost=%v prompt=%v, want all", capability, cost, prompt)
	}
}
