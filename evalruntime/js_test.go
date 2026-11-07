package evalruntime

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

func newJavaScriptHarness(t *testing.T, mutate func(*Options)) *harness {
	t.Helper()
	exe := requireNode(t)
	return newHarness(t, func(opts *Options) {
		opts.JavaScript = exe
		if mutate != nil {
			mutate(opts)
		}
	})
}

// TestJavaScriptPersistentKernel covers §38: cross-cell state, functions,
// modules, JSON and file work in the private workspace.
func TestJavaScriptPersistentKernel(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")

	h.expectCompleted(session, "owner", "const base = 7\nfunction triple(x) { return x * 3 }\nconst { a, b: renamed } = { a: 1, b: 2 }")
	result := h.expectCompleted(session, "owner", "triple(base) + a + renamed")
	if len(result.Displays) != 1 || result.Displays[0].Text != "24" {
		t.Fatalf("displays = %+v", result.Displays)
	}

	// Modules and files: the form the host integration test uses.
	fileResult := h.expectCompleted(session, "owner", `
var fs = await import('node:fs/promises');
await fs.writeFile('data.json', JSON.stringify({ answer: 42 }));
const parsed = JSON.parse(await fs.readFile('data.json', 'utf8'));
parsed.answer
`)
	if len(fileResult.Displays) != 1 || fileResult.Displays[0].Text != "42" {
		t.Fatalf("module/file result = %+v", fileResult.Displays)
	}

	// A thrown error reports a stack and leaves the kernel usable.
	failure := h.exec(session, "owner", "throw new Error('boom')", evalapi.Budget{})
	if failure.Status != evalapi.ResultStatusFailed {
		t.Fatalf("status = %q", failure.Status)
	}
	if failure.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("state = %q", failure.StateStatus)
	}
	if !strings.Contains(failure.Traceback, "boom") {
		t.Fatalf("traceback = %q", failure.Traceback)
	}
	if !failure.PartialEffects {
		t.Fatalf("failed cell must report partial effects")
	}
	if result := h.expectCompleted(session, "owner", "base + 1"); len(result.Displays) == 0 || result.Displays[0].Text != "8" {
		t.Fatalf("state lost after an exception: %+v", result.Displays)
	}
}

func TestJavaScriptReset(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")
	h.expectCompleted(session, "owner", "const keep = 1")
	if err := h.m.Reset(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	result := h.exec(session, "owner", "keep", evalapi.Budget{})
	if result.Succeeded() {
		t.Fatalf("variable survived reset: %+v", result)
	}
	if !strings.Contains(result.Error, "keep") {
		t.Fatalf("error = %q", result.Error)
	}
}

func TestJavaScriptTimeout(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")
	start := time.Now()
	result := h.exec(session, "owner", "while (true) {}", evalapi.Budget{WallSeconds: 1})
	if result.Status != evalapi.ResultStatusTimeout {
		t.Fatalf("status = %q", result.Status)
	}
	if elapsed := time.Since(start); elapsed > 25*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
	h.expectCompleted(session, "owner", "1 + 1")
}

// TestJavaScriptCancelCleansDetachedDescendants covers Linux, where the worker's
// PID namespace contains every descendant.
func TestJavaScriptCancelCleansDetachedDescendants(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("macOS denies fork; Linux contains descendants in a PID namespace")
	}
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")
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
const { spawn } = await import('node:child_process');
const fsp = await import('node:fs/promises');
const child = spawn('/bin/sleep', ['120'], { detached: true, stdio: 'ignore' });
child.unref();
await fsp.writeFile('child.pid', String(child.pid));
await new Promise((resolve) => setTimeout(resolve, 120000));
`,
		}, nil)
		done <- outcome{result: result, err: err}
	}()

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
	cancel()
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Execute: %v", got.err)
		}
		if got.result.Status != evalapi.ResultStatusCancelled && got.result.Status != evalapi.ResultStatusTimeout {
			t.Fatalf("status = %q", got.result.Status)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("cancelled execution did not return")
	}
	if !waitProcessGone(t, childPID, 10*time.Second) {
		t.Fatalf("detached descendant %d survived cancellation", childPID)
	}
}

func TestJavaScriptDisposeCleansWorker(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")
	result := h.expectCompleted(session, "owner", "process.pid")
	if len(result.Displays) != 1 {
		t.Fatalf("displays = %+v", result.Displays)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(result.Displays[0].Text))
	if err != nil {
		t.Fatalf("pid %q: %v", result.Displays[0].Text, err)
	}
	if err := h.m.Dispose(context.Background()); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if !waitProcessGone(t, pid, 10*time.Second) {
		t.Fatalf("node worker %d survived Dispose", pid)
	}
}

func TestJavaScriptUnsupportedInterruptIsDeclared(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	manifests, err := h.m.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for _, manifest := range manifests {
		if manifest.ID != "javascript" {
			continue
		}
		if manifest.Supports(evalapi.CapInterrupt) {
			t.Fatalf("javascript must declare interrupt unsupported")
		}
		if !manifest.Supports(evalapi.CapPersistent) {
			t.Fatalf("javascript must declare persistent execution")
		}
		return
	}
	t.Fatalf("javascript runtime is not discoverable")
}
