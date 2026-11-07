package evalruntime

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

func newPythonHarness(t *testing.T, mutate func(*Options)) (*harness, string) {
	t.Helper()
	exe := requirePython(t)
	h := newHarness(t, func(opts *Options) {
		opts.Python = exe
		if mutate != nil {
			mutate(opts)
		}
	})
	return h, exe
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func waitProcessGone(t *testing.T, pid int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// TestPythonPersistentKernel covers §37: cross-cell state, functions, modules,
// exceptions with partial effects, and a stable worker identity.
func TestPythonPersistentKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")

	first := h.expectCompleted(session, "owner", "import os\nimport json\npid = os.getpid()\ndef double(x):\n    return x * 2\ncounter = 1")
	second := h.expectCompleted(session, "owner", "counter += 1\ndouble(counter)")
	if len(second.Displays) != 1 || second.Displays[0].Text != "4" {
		t.Fatalf("function/variable did not survive: %+v", second.Displays)
	}
	_ = first
	pidText := strings.TrimSpace(h.expectCompleted(session, "owner", "print(os.getpid())").Stdout)
	if pidText == "" {
		t.Fatalf("pid output missing")
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("pid %q: %v", pidText, err)
	}
	if !processAlive(pid) {
		t.Fatalf("worker pid %d is not alive", pid)
	}
	again := strings.TrimSpace(h.expectCompleted(session, "owner", "print(os.getpid())").Stdout)
	if again != pidText {
		t.Fatalf("the worker restarted between cells: %s -> %s", pidText, again)
	}

	// A failure reports its traceback, does not roll back side effects, and
	// leaves the kernel usable.
	failure := h.exec(session, "owner", "counter = 99\n1/0", evalapi.Budget{})
	if failure.Status != evalapi.ResultStatusFailed {
		t.Fatalf("status = %q", failure.Status)
	}
	if failure.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("state after a handled exception = %q", failure.StateStatus)
	}
	if !strings.Contains(failure.Traceback, "ZeroDivisionError") {
		t.Fatalf("traceback = %q", failure.Traceback)
	}
	if !failure.PartialEffects {
		t.Fatalf("a failed cell must report partial effects")
	}
	after := h.expectCompleted(session, "owner", "counter")
	if len(after.Displays) == 0 || after.Displays[0].Text != "99" {
		t.Fatalf("side effect before the exception was lost: %+v", after.Displays)
	}
	if failure.Generation != after.Generation {
		t.Fatalf("generation changed after a handled exception")
	}
}

func TestPythonScientificStack(t *testing.T) {
	h, exe := newPythonHarness(t, nil)
	if !pythonModuleAvailable(t, exe, "numpy") || !pythonModuleAvailable(t, exe, "scipy") {
		t.Skip("numpy/scipy are not installed in the test interpreter")
	}
	session := h.create("python", "owner", "owner")
	result := h.expectCompleted(session, "owner", `
import numpy as np
from scipy import optimize
xs = np.linspace(0, 2, 11)
ys = xs ** 2
fit = np.polyfit(xs, ys, 2)
root = optimize.brentq(lambda x: x ** 2 - 4, 0, 5)
print(round(float(fit[0]), 6), round(float(root), 6))
`)
	if !strings.Contains(result.Stdout, "1.0") || !strings.Contains(result.Stdout, "2.0") {
		t.Fatalf("scientific output = %q", result.Stdout)
	}
	if pythonModuleAvailable(t, exe, "pandas") {
		pandas := h.expectCompleted(session, "owner", `
import pandas as pd
frame = pd.DataFrame({"a": [1, 2, 3]})
print(int(frame["a"].sum()))
`)
		if strings.TrimSpace(pandas.Stdout) != "6" {
			t.Fatalf("pandas output = %q", pandas.Stdout)
		}
	}
}

func TestPythonPlottingProducesPNGDisplay(t *testing.T) {
	h, exe := newPythonHarness(t, nil)
	if !pythonModuleAvailable(t, exe, "matplotlib") {
		t.Skip("matplotlib is not installed in the test interpreter")
	}
	session := h.create("python", "owner", "owner")
	result := h.expectCompleted(session, "owner", `
import matplotlib
matplotlib.use("Agg")
import matplotlib.pyplot as plt
plt.plot([1, 2, 3], [1, 4, 9])
`)
	var png []byte
	for _, display := range result.Displays {
		if display.MIME != "image/png" {
			continue
		}
		if display.Encoding != "base64" {
			t.Fatalf("png display encoding = %q", display.Encoding)
		}
		decoded, err := base64.StdEncoding.DecodeString(display.Text)
		if err != nil {
			t.Fatalf("decode display: %v", err)
		}
		png = decoded
	}
	if len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("plot display is not a PNG (%d bytes)", len(png))
	}
}

func TestPythonArtifactsAndReadBack(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	result := h.expectCompleted(session, "owner", `
import json, os
lc_artifact(json.dumps({"value": 7}), name="result.json", mime="application/json")
print(os.path.exists(os.path.join(os.environ["LC_EVAL_ARTIFACTS"], "result.json")))
`)
	if len(result.Artifacts) != 1 {
		t.Fatalf("artifacts = %+v", result.Artifacts)
	}
	artifact := result.Artifacts[0]
	if artifact.Name != "result.json" || artifact.MIME != "application/json" || artifact.Size == 0 || artifact.SHA256 == "" {
		t.Fatalf("artifact metadata = %+v", artifact)
	}
	meta, data, err := h.m.Artifact(context.Background(), session.ID, "owner", artifact.ID)
	if err != nil {
		t.Fatalf("Artifact: %v", err)
	}
	if meta.SHA256 != artifact.SHA256 {
		t.Fatalf("artifact digest mismatch")
	}
	if !strings.Contains(string(data), `"value": 7`) {
		t.Fatalf("artifact bytes = %q", data)
	}
	if _, _, err := h.m.Artifact(context.Background(), session.ID, "stranger", artifact.ID); !errors.Is(err, evalapi.ErrDenied) {
		t.Fatalf("cross-agent artifact read = %v", err)
	}
}

func TestPythonResetDiscardsState(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "value = 123")
	if err := h.m.Reset(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	result := h.exec(session, "owner", "value", evalapi.Budget{})
	if result.Succeeded() {
		t.Fatalf("value survived reset: %+v", result)
	}
	if !strings.Contains(result.Error, "NameError") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestPythonCrashIsNotReplayed(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "before = 1")
	crash := h.exec(session, "owner", "import os\nos._exit(7)", evalapi.Budget{})
	if crash.Status != evalapi.ResultStatusCrashed && crash.Status != evalapi.ResultStatusCancelled {
		t.Fatalf("crash status = %q", crash.Status)
	}
	if crash.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("crash state = %q", crash.StateStatus)
	}
	if !crash.PartialEffects {
		t.Fatalf("crash must report partial effects")
	}
	if crash.ExitCode != 7 {
		t.Fatalf("exit code = %d", crash.ExitCode)
	}
	// The next cell starts a new generation; nothing is replayed.
	fresh := h.exec(session, "owner", "before", evalapi.Budget{})
	if fresh.Succeeded() {
		t.Fatalf("a crashed cell was replayed: %+v", fresh)
	}
	if fresh.Generation <= crash.Generation {
		t.Fatalf("generation did not advance after crash: %d -> %d", crash.Generation, fresh.Generation)
	}
	if result := h.expectCompleted(session, "owner", "2 + 2"); len(result.Displays) == 0 {
		t.Fatalf("fresh kernel did not run")
	}
}

// TestPythonTimeoutForcesFreshKernel: a timeout spends the caller's budget, so the
// whole owned tree is terminated and the kernel state is gone (Owner §27).
func TestPythonTimeoutForcesFreshKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	before := h.expectCompleted(session, "owner", "kept = 5")
	start := time.Now()
	result := h.exec(session, "owner", "while True:\n    pass", evalapi.Budget{WallSeconds: 1})
	elapsed := time.Since(start)
	if result.Status != evalapi.ResultStatusTimeout {
		t.Fatalf("status = %q", result.Status)
	}
	if elapsed > 20*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	if result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("a timeout must report a lost kernel, got %q", result.StateStatus)
	}
	// The result reports the generation it ran in; the session advances after the
	// loss, so the *next* generation is what proves the kernel was replaced.
	after, err := h.m.Session(context.Background(), session.ID, "owner")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if after.Generation <= before.Generation {
		t.Fatalf("generation did not advance after a timeout: %d -> %d", before.Generation, after.Generation)
	}
	if result := h.exec(session, "owner", "kept", evalapi.Budget{}); result.Succeeded() {
		t.Fatalf("state survived a timeout: %+v", result)
	}
	h.expectCompleted(session, "owner", "1 + 1")
}

// TestExplicitInterruptRetainsAcknowledgedKernel: Interrupt asks the worker to
// abort. When it acknowledges, the kernel and its variables survive.
func TestExplicitInterruptRetainsAcknowledgedKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	before := h.expectCompleted(session, "owner", "kept = 5")
	done := make(chan evalapi.Result, 1)
	go func() {
		result, err := h.m.Execute(context.Background(), evalapi.Request{
			ID: "interrupt-me", SessionID: session.ID, CellID: "c", AgentID: "owner",
			Code: "import time\ntime.sleep(60)",
		}, nil)
		if err != nil {
			done <- evalapi.Result{Status: evalapi.ResultStatusCrashed, Error: err.Error()}
			return
		}
		done <- result
	}()
	time.Sleep(500 * time.Millisecond)
	if err := h.m.Interrupt(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	var result evalapi.Result
	select {
	case result = <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("interrupted execution did not return")
	}
	if result.Status != evalapi.ResultStatusCancelled {
		t.Fatalf("status = %q", result.Status)
	}
	if result.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("an acknowledged interrupt must retain the kernel, got %q", result.StateStatus)
	}
	if result.Generation != before.Generation {
		t.Fatalf("generation changed on a cooperative interrupt: %d -> %d", before.Generation, result.Generation)
	}
	kept := h.expectCompleted(session, "owner", "kept")
	if len(kept.Displays) == 0 || kept.Displays[0].Text != "5" {
		t.Fatalf("kernel state lost on a cooperative interrupt: %+v", kept.Displays)
	}
}

func TestPythonEscalatesWhenInterruptIsIgnored(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	start := time.Now()
	result := h.exec(session, "owner", `
import signal, time
signal.signal(signal.SIGINT, signal.SIG_IGN)
time.sleep(120)
`, evalapi.Budget{WallSeconds: 1})
	if result.Status != evalapi.ResultStatusTimeout {
		t.Fatalf("status = %q", result.Status)
	}
	if result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("state = %q", result.StateStatus)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("escalation took %s", elapsed)
	}
	// The next cell runs in a fresh kernel.
	h.expectCompleted(session, "owner", "1 + 1")
}

// TestPythonCancelCleansDetachedDescendants covers a host that can create
// descendants: Linux runs the worker in a private PID namespace, so killing the
// namespace is complete.
func TestPythonCancelCleansDetachedDescendants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS denies fork; Linux contains descendants in a PID namespace")
	}
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		result evalapi.Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := h.m.Execute(ctx, evalapi.Request{
			ID: "detach", SessionID: session.ID, CellID: "c", AgentID: "owner",
			Code: `
import subprocess, time
child = subprocess.Popen(["/bin/sleep", "120"], start_new_session=True)
open("child.pid", "w").write(str(child.pid))
time.sleep(120)
`,
		}, nil)
		done <- outcome{result: result, err: err}
	}()

	// The child pid is published in the kernel workspace, which the host can read.
	pidFile := filepath.Join(session.Workspace, "work", "child.pid")
	var childPID int
	deadline := time.Now().Add(20 * time.Second)
	for childPID == 0 && time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(pidFile); readErr == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(data))); convErr == nil && pid > 0 {
				childPID = pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if childPID == 0 {
		cancel()
		t.Fatalf("the cell did not publish its detached child pid")
	}
	if !processAlive(childPID) {
		t.Fatalf("detached child %d was not running", childPID)
	}
	cancel()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Execute: %v", got.err)
		}
		if got.result.Status != evalapi.ResultStatusCancelled {
			t.Fatalf("status = %q", got.result.Status)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("cancelled execution did not return")
	}
	if !waitProcessGone(t, childPID, 10*time.Second) {
		t.Fatalf("detached descendant %d survived cancellation", childPID)
	}
}

func TestPythonEnvAllowlistHidesHostSecrets(t *testing.T) {
	const secretName = "LCX_TEST_SYNTHETIC_SECRET"
	t.Setenv(secretName, "should-not-leak")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/definitely-not-a-socket")
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	result := h.expectCompleted(session, "owner", fmt.Sprintf(`
import os
print(repr(os.environ.get(%q)))
print(repr(os.environ.get("SSH_AUTH_SOCK")))
print(repr(os.environ.get("HOME")))
`, secretName))
	if strings.Contains(result.Stdout, "should-not-leak") {
		t.Fatalf("host secret leaked into the worker environment: %q", result.Stdout)
	}
	if strings.Contains(result.Stdout, "definitely-not-a-socket") {
		t.Fatalf("SSH_AUTH_SOCK leaked: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "None") {
		t.Fatalf("secret variable was defined: %q", result.Stdout)
	}
	// HOME points at the private kernel, not the host home.
	if strings.Contains(result.Stdout, os.Getenv("HOME")) {
		t.Fatalf("worker HOME is the host home: %q", result.Stdout)
	}
}

func TestPythonFileIsolationBetweenKernels(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	owner := h.create("python", "owner-a", "owner-a")
	other := h.create("python", "owner-b", "owner-b")
	canary := filepath.Join(other.Workspace, "work", "canary.txt")
	if err := os.WriteFile(canary, []byte("private-data"), 0o600); err != nil {
		t.Fatalf("write canary: %v", err)
	}
	result := h.exec(owner, "owner-a", fmt.Sprintf(`
try:
    print(open(%q).read())
except PermissionError:
    print("denied")
except OSError as exc:
    print("denied", type(exc).__name__)
`, canary), evalapi.Budget{})
	if !result.Succeeded() {
		t.Fatalf("read attempt failed unexpectedly: %+v", result)
	}
	if strings.Contains(result.Stdout, "private-data") {
		t.Fatalf("kernel A read kernel B's private file: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "denied") {
		t.Fatalf("expected a permission denial, got %q", result.Stdout)
	}
	// The kernel can still write inside its own workspace.
	own := h.expectCompleted(owner, "owner-a", `import os
open("own.txt", "w").write("ok")
print(os.path.getsize("own.txt"))`)
	if !strings.Contains(own.Stdout, "2") {
		t.Fatalf("own write = %q", own.Stdout)
	}
}

func TestPythonNetworkIsDenied(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	result := h.exec(session, "owner", `
import socket
try:
    s = socket.create_connection(("127.0.0.1", 9), timeout=1)
    print("connected")
except OSError as exc:
    print("denied", exc.errno)
`, evalapi.Budget{WallSeconds: 10})
	if !result.Succeeded() {
		t.Fatalf("network probe failed unexpectedly: %+v", result)
	}
	if strings.Contains(result.Stdout, "connected") {
		t.Fatalf("worker opened a network connection: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "denied") {
		t.Fatalf("expected a network denial, got %q", result.Stdout)
	}
}

func TestPythonDisposeCleansWorkers(t *testing.T) {
	exe := requirePython(t)
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Root: filepath.Join(root, "state"), Python: exe, JavaScript: requireNode(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	session, err := m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "python", OwnerID: "owner", AgentID: "owner", Workspace: workspace, Permission: "workspace",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	result, err := m.Execute(context.Background(), evalapi.Request{
		ID: "pid", SessionID: session.ID, CellID: "c", AgentID: "owner", Code: "import os\nprint(os.getpid())",
	}, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(result.Stdout))
	if convErr != nil {
		t.Fatalf("pid %q: %v", result.Stdout, convErr)
	}
	if err := m.Dispose(context.Background()); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if !waitProcessGone(t, pid, 10*time.Second) {
		t.Fatalf("worker %d survived Dispose", pid)
	}
}

// TestSubprocessIsDeniedByDefault proves the default containment: no fork, so no
// descendant can exist, and the runtime declares the capability unsupported.
func TestSubprocessIsDeniedByDefault(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Linux runs workers in a private PID namespace, so subprocess stays available")
	}
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	result := h.exec(session, "owner", `
import subprocess
try:
    child = subprocess.Popen(["/bin/sleep", "5"])
    print("spawned", child.pid)
except OSError as exc:
    print("denied", type(exc).__name__)
`, evalapi.Budget{})
	if !result.Succeeded() {
		t.Fatalf("probe cell failed: %+v", result)
	}
	if strings.Contains(result.Stdout, "spawned") {
		t.Fatalf("fork was allowed under the default sandbox: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "denied") {
		t.Fatalf("expected a denial, got %q", result.Stdout)
	}
	// The kernel survives the denied fork.
	h.expectCompleted(session, "owner", "1 + 1")
	manifests, err := h.m.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, manifest := range manifests {
		if manifest.ID == "python" && manifest.Supports(evalapi.CapSubprocess) {
			t.Fatalf("python must not claim subprocess support while fork is denied")
		}
	}
}
