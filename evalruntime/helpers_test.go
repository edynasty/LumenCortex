package evalruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

// requirePython returns an interpreter with a real standard library. The test
// environment may point at a scientific environment through LCX_TEST_PYTHON.
func requirePython(t *testing.T) string {
	t.Helper()
	if explicit := os.Getenv("LCX_TEST_PYTHON"); explicit != "" {
		return explicit
	}
	// Prefer the scientific environment: the capability tests are only meaningful
	// when NumPy/SciPy/Matplotlib are actually installed.
	candidates := []string{"/opt/miniconda3/envs/ai_prediction_env/bin/python"}
	if path, err := exec.LookPath("python3"); err == nil {
		candidates = append(candidates, path)
	}
	candidates = append(candidates, "/opt/miniconda3/bin/python")
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	t.Skip("no python3 interpreter is available")
	return ""
}

func requireNode(t *testing.T) string {
	t.Helper()
	if explicit := os.Getenv("LCX_TEST_NODE"); explicit != "" {
		return explicit
	}
	if path, err := exec.LookPath("node"); err == nil {
		return path
	}
	t.Skip("node is not available")
	return ""
}

// pythonModuleAvailable probes the interpreter outside any sandbox: a capability
// test may only assert what is actually installed.
func pythonModuleAvailable(t *testing.T, exe, module string) bool {
	t.Helper()
	script := fmt.Sprintf("import importlib.util as u, sys; sys.exit(0 if u.find_spec(%q) else 1)", module)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, exe, "-I", "-c", script).Run() == nil
}

var (
	fixtureOnce sync.Once
	fixturePath string
	fixtureDir  string
	fixtureErr  error
)

// TestMain owns the built fixture binary: the directory is created once and
// removed when the package's tests finish.
func TestMain(m *testing.M) {
	code := m.Run()
	if fixtureDir != "" {
		_ = os.RemoveAll(fixtureDir)
	}
	os.Exit(code)
}

// requireFixture builds the third-party runtime fixture from testdata. It is a
// separate executable that speaks only the public control protocol.
func requireFixture(t *testing.T) string {
	t.Helper()
	fixtureOnce.Do(func() {
		dir, err := os.MkdirTemp("", "lcx-fixture-")
		if err != nil {
			fixtureErr = err
			return
		}
		fixtureDir = dir
		out := filepath.Join(dir, "fixturelang")
		cmd := exec.Command("go", "build", "-o", out, "./testdata/fixturelang")
		cmd.Dir = packageDir()
		if output, err := cmd.CombinedOutput(); err != nil {
			fixtureErr = fmt.Errorf("go build fixture: %v: %s", err, output)
			return
		}
		fixturePath = out
	})
	if fixtureErr != nil {
		// The fixture is part of the acceptance surface: a build failure is a
		// defect, not an environment excuse.
		t.Fatalf("third-party fixture is unavailable: %v", fixtureErr)
	}
	return fixturePath
}

func packageDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

// harness is one manager plus a private host workspace, cleaned up per test.
type harness struct {
	t         *testing.T
	m         *Manager
	root      string
	workspace string
	events    *eventLog
}

type eventLog struct {
	mu     sync.Mutex
	events []evalapi.Event
}

func (l *eventLog) record(event evalapi.Event) {
	l.mu.Lock()
	l.events = append(l.events, event)
	l.mu.Unlock()
}

func (l *eventLog) byType(name string) []evalapi.Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []evalapi.Event
	for _, event := range l.events {
		if event.Type == name {
			out = append(out, event)
		}
	}
	return out
}

func newHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	events := &eventLog{}
	opts := Options{
		Root:    filepath.Join(root, "state"),
		Budget:  evalapi.Budget{WallSeconds: 30, CPUSeconds: 20, MemoryBytes: 1 << 30, OutputBytes: 256 << 10, ArtifactBytes: 4 << 20, DiskBytes: 32 << 20, MaxCallbacks: 16, MaxDepth: 2},
		OnEvent: events.record,
	}
	if mutate != nil {
		mutate(&opts)
	}
	m, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = m.Dispose(ctx)
	})
	return &harness{t: t, m: m, root: root, workspace: workspace, events: events}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func (h *harness) create(runtimeID, owner, agent string) evalapi.Session {
	h.t.Helper()
	session, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID:  runtimeID,
		OwnerID:    owner,
		AgentID:    agent,
		Workspace:  h.workspace,
		Permission: "workspace",
	})
	if err != nil {
		h.t.Fatalf("Create(%s): %v", runtimeID, err)
	}
	return session
}

func (h *harness) exec(session evalapi.Session, agent, code string, budget evalapi.Budget) evalapi.Result {
	h.t.Helper()
	return h.execCaller(session, agent, code, budget, nil)
}

func (h *harness) execCaller(session evalapi.Session, agent, code string, budget evalapi.Budget, caller evalapi.ToolCaller) evalapi.Result {
	h.t.Helper()
	return h.execRequest(session, agent, evalapi.Request{
		ID:      fmt.Sprintf("req-%d", time.Now().UnixNano()),
		CellID:  "cell",
		Code:    code,
		Budget:  budget,
		AgentID: agent,
	}, caller)
}

func (h *harness) execRequest(session evalapi.Session, agent string, req evalapi.Request, caller evalapi.ToolCaller) evalapi.Result {
	h.t.Helper()
	if req.AgentID == "" {
		req.AgentID = agent
	}
	req.SessionID = session.ID
	if req.ID == "" {
		req.ID = fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	if req.CellID == "" {
		req.CellID = "cell"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	result, err := h.m.Execute(ctx, req, caller)
	if err != nil {
		h.t.Fatalf("Execute: %v", err)
	}
	return result
}

func (h *harness) expectCompleted(session evalapi.Session, agent, code string) evalapi.Result {
	h.t.Helper()
	result := h.exec(session, agent, code, evalapi.Budget{})
	if !result.Succeeded() {
		h.t.Fatalf("expected completed, got status=%q state=%q error=%q stderr=%q", result.Status, result.StateStatus, result.Error, result.Stderr)
	}
	return result
}

// fakeCaller is a host-side tool surface for callback tests.
type fakeCaller struct {
	specs []protocol.ToolSpec
	mu    sync.Mutex
	calls []fakeCall
	fn    func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error)
}

type fakeCall struct {
	name string
	args map[string]any
}

func (f *fakeCaller) Tools(ctx context.Context) ([]protocol.ToolSpec, error) {
	return f.specs, nil
}

func (f *fakeCaller) Call(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{name: name, args: args})
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(ctx, name, args)
	}
	return protocol.ToolResult{OK: true, Content: "ok"}, nil
}

func (f *fakeCaller) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}
