//go:build unix

package runtime

// Cross-review (B2 process/eval/DAP): run end / revocation / Engine shutdown
// must clean every kernel it owns and none it does not.
//
//  2/4. Closing one session's kernel must not stop another session's kernel.
//  4. Engine.Close leaves no kernel process behind (checked by host pid).
//  3. The census reports no stale row after each step.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

func xreviewKernelPID(t *testing.T, e *Engine, sessionID string) int {
	t.Helper()
	rows, err := e.Processes(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("Processes(%s): %v", sessionID, err)
	}
	if len(rows) != 1 {
		t.Fatalf("census for %s = %+v", sessionID, rows)
	}
	if rows[0].Owner.Component != "eval" {
		t.Fatalf("census row is not an eval kernel: %+v", rows[0])
	}
	return rows[0].PID
}

func xreviewAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func TestXReviewRunEndLeavesForeignKernelAndShutdownCleansAll(t *testing.T) {
	if goruntime.GOOS != "darwin" && goruntime.GOOS != "linux" {
		t.Skip("no supported sandbox backend on this platform")
	}
	python := os.Getenv("LCX_TEST_PYTHON")
	if python == "" {
		python = "python3"
	}
	pythonPath, err := exec.LookPath(python)
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
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	useGlobalConfig(t, boundCatalog("test/b0"))
	engine, err := Open(Options{Workspace: workspace, Eval: &EvalOptions{Python: pythonPath, JavaScript: js, Budget: evalapi.Budget{MaxWorkers: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := engine.Close(); err != nil {
				t.Error(err)
			}
		}
	})

	first, err := engine.NewSession(context.Background(), SessionOptions{Goal: "first kernel"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := engine.NewSession(context.Background(), SessionOptions{Goal: "foreign kernel"})
	if err != nil {
		t.Fatal(err)
	}
	runtimeID := executionRuntimeID(t, engine, "python")
	firstKernel, err := engine.CreateEvalSession(context.Background(), first.ID, runtimeID)
	if err != nil {
		t.Fatalf("CreateEvalSession: %v", err)
	}
	secondKernel, err := engine.CreateEvalSession(context.Background(), second.ID, runtimeID)
	if err != nil {
		t.Fatalf("CreateEvalSession: %v", err)
	}
	executeHostCell(t, engine, first.ID, firstKernel.ID, "a", "print('a')")
	executeHostCell(t, engine, second.ID, secondKernel.ID, "b", "print('b')")
	firstPID := xreviewKernelPID(t, engine, first.ID)
	secondPID := xreviewKernelPID(t, engine, second.ID)
	if firstPID == secondPID {
		t.Fatalf("two kernels share pid %d", firstPID)
	}

	// Ending one session's kernel leaves the other one running and answering.
	if err := engine.EvalClose(context.Background(), first.ID, firstKernel.ID); err != nil {
		t.Fatalf("EvalClose: %v", err)
	}
	if rows, _ := engine.Processes(context.Background(), first.ID); len(rows) != 0 {
		t.Fatalf("closed session census = %+v", rows)
	}
	if rows, _ := engine.Processes(context.Background(), second.ID); len(rows) != 1 || rows[0].PID != secondPID {
		t.Fatalf("foreign census after close = %+v", rows)
	}
	if !xreviewAlive(secondPID) {
		t.Fatalf("foreign kernel %d died when the first session ended", secondPID)
	}
	kept := executeHostCell(t, engine, second.ID, secondKernel.ID, "b2", "print('still-here')")
	if kept.Generation == 0 {
		t.Fatalf("foreign kernel restarted: %+v", kept)
	}

	// A run end for the second session closes its own kernel only; Engine
	// shutdown then leaves no kernel process behind.
	if err := engine.closeEvalOwner(second.ID); err != nil {
		t.Fatalf("closeEvalOwner: %v", err)
	}
	if rows, _ := engine.Processes(context.Background(), second.ID); len(rows) != 0 {
		t.Fatalf("run-end census = %+v", rows)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Engine.Close: %v", err)
	}
	closed = true
	deadline := time.Now().Add(10 * time.Second)
	for xreviewAlive(firstPID) || xreviewAlive(secondPID) {
		if time.Now().After(deadline) {
			t.Fatalf("kernel orphan after run end/shutdown: first=%d alive=%v second=%d alive=%v",
				firstPID, xreviewAlive(firstPID), secondPID, xreviewAlive(secondPID))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
