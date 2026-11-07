package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/session"
)

func executionTestEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	if goruntime.GOOS != "darwin" {
		t.Skip("native seatbelt integration requires macOS; platform worker tests cover other backends")
	}
	python := os.Getenv("LCX_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	python, err := exec.LookPath(python)
	if err != nil {
		t.Skip("test Python not installed")
	}
	js, err := exec.LookPath("node")
	if err != nil {
		js, err = exec.LookPath("bun")
	}
	if err != nil {
		t.Skip("JavaScript runtime not installed")
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	useGlobalConfig(t, boundCatalog("test/b0"))
	engine, err := Open(Options{Workspace: workspace, Eval: &EvalOptions{Python: python, JavaScript: js, Budget: evalapi.Budget{MaxWorkers: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := engine.Close(); err != nil {
			t.Error(err)
		}
	})
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "synthetic multilingual verification"})
	if err != nil {
		t.Fatal(err)
	}
	return engine, handle.ID
}

func executionRuntimeID(t *testing.T, e *Engine, language string) string {
	t.Helper()
	list, err := e.EvalRuntimes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list {
		if item.Language == language {
			return item.ID
		}
	}
	t.Fatalf("no %s runtime discovered", language)
	return ""
}

func executeHostCell(t *testing.T, e *Engine, owner, kernel, cell, code string) evalapi.Result {
	t.Helper()
	result, err := e.ExecuteEval(context.Background(), owner, evalapi.Request{SessionID: kernel, CellID: cell, Code: code, Deadline: time.Now().Add(20 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Succeeded() {
		t.Fatalf("cell %s: %#v", cell, result)
	}
	return result
}

func TestEvalHostPersistsStateAndTransfersAcrossLanguages(t *testing.T) {
	engine, owner := executionTestEngine(t)
	ctx := context.Background()
	py, err := engine.CreateEvalSession(ctx, owner, executionRuntimeID(t, engine, "python"))
	if err != nil {
		t.Fatal(err)
	}
	js, err := engine.CreateEvalSession(ctx, owner, executionRuntimeID(t, engine, "javascript"))
	if err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"values":[2,4,6]}`)
	if err := os.WriteFile(filepath.Join(engine.workspace, "input.json"), input, 0600); err != nil {
		t.Fatal(err)
	}
	artifact, err := engine.TransferEvalArtifact(ctx, owner, "import", py.ID, "", "input.json", "")
	if err != nil {
		t.Fatal(err)
	}
	first := executeHostCell(t, engine, owner, py.ID, "py-load", "import json, os\nvalues = json.load(open('input.json'))['values']\nprint(sum(values))\nprint(os.getpid())")
	if !strings.Contains(first.Stdout, "12") {
		t.Fatalf("actual data sum missing: %s", first.Stdout)
	}
	second := executeHostCell(t, engine, owner, py.ID, "py-reuse", "print(sum(values) * 2)")
	if !strings.Contains(second.Stdout, "24") || first.Generation != second.Generation {
		t.Fatalf("state not retained: %#v %#v", first, second)
	}
	copied, err := engine.TransferEvalArtifact(ctx, owner, "copy", py.ID, artifact.ID, "input.json", js.ID)
	if err != nil {
		t.Fatal(err)
	}
	if copied.SHA256 != artifact.SHA256 {
		t.Fatal("cross-language bytes changed")
	}
	result := executeHostCell(t, engine, owner, js.ID, "js-read", "var fs = await import('node:fs/promises'); var input = JSON.parse(await fs.readFile('input.json', 'utf8')); console.log(JSON.stringify({sum:input.values.reduce((a,b)=>a+b,0)}));")
	if !strings.Contains(result.Stdout, `"sum":12`) {
		t.Fatalf("JS did not process transferred bytes: %s", result.Stdout)
	}
	if err := engine.EvalReset(ctx, owner, py.ID); err != nil {
		t.Fatal(err)
	}
	reset := executeHostCell(t, engine, owner, py.ID, "py-reset", "print('values' in globals())")
	if !strings.Contains(reset.Stdout, "False") || reset.Generation == first.Generation {
		t.Fatalf("reset retained state: %#v", reset)
	}
}

func TestEvalHostIdentityIsolationAndRevocation(t *testing.T) {
	engine, owner := executionTestEngine(t)
	ctx := context.Background()
	other, err := engine.NewSession(ctx, SessionOptions{Goal: "other owner"})
	if err != nil {
		t.Fatal(err)
	}
	py, err := engine.CreateEvalSession(ctx, owner, executionRuntimeID(t, engine, "python"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ExecuteEval(ctx, other.ID, evalapi.Request{SessionID: py.ID, Code: "print('unauthorized')", CellID: "denied"}); err == nil {
		t.Fatal("other owner executed private kernel")
	}
	if _, err := engine.ExecuteEval(ctx, owner, evalapi.Request{SessionID: py.ID, AgentID: other.ID, Code: "print('impersonation')", CellID: "denied"}); err == nil {
		t.Fatal("request impersonated another agent")
	}
	if err := engine.GrantEvalSession(ctx, owner, py.ID, evalapi.Grant{AgentID: other.ID, Role: evalapi.RoleObserver, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.EvalResults(ctx, other.ID, py.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ExecuteEval(ctx, other.ID, evalapi.Request{SessionID: py.ID, Code: "print('observer cannot execute')", CellID: "observer"}); err == nil {
		t.Fatal("observer executed")
	}
	if err := engine.RevokeEvalSession(ctx, owner, py.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.EvalResults(ctx, other.ID, py.ID); err == nil {
		t.Fatal("revoked observer retained results")
	}
	if _, err := engine.RevokeSource(ctx, SourceSession, owner, "synthetic_revocation"); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.ExecuteEval(ctx, owner, evalapi.Request{SessionID: py.ID, Code: "print('withdrawn')", CellID: "withdrawn"}); !errors.Is(err, ErrNotebookWithdrawn) {
		t.Fatalf("withdrawn owner allowed: %v", err)
	}
}

func TestEvalHostDisabledDoesNotCreateRuntimeState(t *testing.T) {
	root := t.TempDir()
	e, err := Open(Options{Workspace: root, RepoDir: filepath.Join(root, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if _, err := e.EvalRuntimes(context.Background()); !errors.Is(err, ErrEvalDisabled) {
		t.Fatalf("expected disabled, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "state", "eval")); !os.IsNotExist(err) {
		t.Fatalf("disabled host created runtime state: %v", err)
	}
}

// A child's observer lease must not outlive a Start that never became a run:
// the grant is revoked on the failed Start instead of surviving until its lease expires.
func TestFailedChildStartRevokesTheSharedKernelGrant(t *testing.T) {
	useGlobalConfig(t, catalogWithAgents(`{"worker": {"models": [{"model": "test/b0"}]}}`))
	engine := openEvalEngine(t)
	ctx := context.Background()
	parent, err := engine.NewSession(ctx, SessionOptions{Goal: "grant owner"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := engine.NewSession(ctx, SessionOptions{Goal: "grantee"})
	if err != nil {
		t.Fatal(err)
	}
	kernel, err := engine.CreateEvalSession(ctx, parent.ID, pythonRuntime(t, engine).ID)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := engine.evalHost(ctx)
	if err != nil {
		t.Fatal(err)
	}
	granted := false
	testHookBeforeBeginRun = func() {
		if _, grantErr := manager.Session(context.Background(), kernel.ID, child.ID); grantErr == nil {
			granted = true
		}
		if _, revokeErr := engine.store.RevokeSession(context.Background(), child.ID, "test"); revokeErr != nil {
			t.Error(revokeErr)
		}
	}
	defer func() { testHookBeforeBeginRun = nil }()
	_, err = engine.prepareRun(ctx, child.ID, AgentOptions{parentSessionID: parent.ID, EvalEnabled: true,
		sharedEvalSession: kernel.ID, sharedEvalLease: time.Minute})
	if !errors.Is(err, session.ErrFenced) {
		t.Fatalf("failed Start: %v", err)
	}
	if !granted {
		t.Fatal("the child never held the grant: the cleanup is unproven")
	}
	if _, grantErr := manager.Session(ctx, kernel.ID, child.ID); !errors.Is(grantErr, evalapi.ErrDenied) {
		t.Fatalf("a failed Start left the shared kernel grant alive: %v", grantErr)
	}
}
