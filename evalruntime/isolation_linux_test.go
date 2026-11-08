//go:build linux

package evalruntime

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/cgroupv2"
)

const allocate640MiB = `
import time
chunks = []
for _ in range(80):
    chunks.append(bytearray(8 * 1024 * 1024))
    time.sleep(0.02)
print("finished")
`

// workerCgroup returns the session's current worker cgroup directory.
func workerCgroup(t *testing.T, m *Manager, sessionID string) string {
	t.Helper()
	s, err := m.session(sessionID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	w := installedWorker(s)
	if w == nil || w.proc == nil || w.proc.cg == nil {
		t.Fatalf("session %s has no worker cgroup", sessionID)
	}
	return w.proc.cg.Dir()
}

func readCgroupValue(t *testing.T, dir, name string) string {
	t.Helper()
	value, err := cgroupv2.ReadFile(dir, name)
	if err != nil {
		t.Fatalf("read %s/%s: %v", dir, name, err)
	}
	return value
}

// The hard limit holds for the whole tree: memory.max is set before the worker
// runs, swap is closed, and the outer bwrap, the sandbox init and the
// interpreter are all members of the same leaf.
func TestLinuxWorkerCgroupBoundsTheWholeTree(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Budget.MemoryBytes = 128 << 20 })
	session := h.create("python", "owner", "owner")
	for _, manifest := range mustDiscover(t, h.m) {
		if manifest.Limits.MemoryEnforcement != evalapi.EnforcementHard {
			t.Fatalf("%s declares memory enforcement %q on Linux", manifest.ID, manifest.Limits.MemoryEnforcement)
		}
	}
	pidText := strings.TrimSpace(h.expectCompleted(session, "owner", "import os\nprint(os.getpid())").Stdout)
	pid, err := strconv.Atoi(pidText)
	if err != nil {
		t.Fatalf("pid %q: %v", pidText, err)
	}
	dir := workerCgroup(t, h.m, session.ID)
	if got := readCgroupValue(t, dir, "memory.max"); got != strconv.Itoa(128<<20) {
		t.Fatalf("memory.max = %s", got)
	}
	if got := readCgroupValue(t, dir, "memory.swap.max"); got != "0" {
		t.Fatalf("memory.swap.max = %s", got)
	}
	if got := readCgroupValue(t, dir, "pids.max"); got != strconv.Itoa(workerTasksMax) {
		t.Fatalf("pids.max = %s", got)
	}
	interpreter := hostProcessOf(t, h.m, session.ID, pid)
	s, _ := h.m.session(session.ID)
	w := installedWorker(s)
	members := map[int]bool{}
	for _, member := range w.proc.cg.procs() {
		members[member] = true
	}
	group, err := w.proc.workerGroup()
	if err != nil {
		t.Fatalf("workerGroup: %v", err)
	}
	for name, member := range map[string]int{"outer bwrap": w.proc.pid, "sandbox init": group.id.pid, "interpreter": interpreter.pid} {
		if !members[member] {
			t.Fatalf("%s (host pid %d) is not in the worker cgroup %v", name, member, w.proc.cg.procs())
		}
	}
	if group.id.pid == w.proc.pid {
		t.Fatalf("the worker group is the outer bwrap")
	}
}

func mustDiscover(t *testing.T, m *Manager) []evalapi.Manifest {
	t.Helper()
	manifests, err := m.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return manifests
}

// Below the limit a cell runs normally; above it the kernel kills the worker,
// the result names the limit, the peak is the kernel's figure, and the next
// worker is bounded again (a restart does not escape the limit).
func TestLinuxMemoryLimitIsHardAcrossRestarts(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Budget.MemoryBytes = 128 << 20 })
	session := h.create("python", "owner", "owner")
	under := h.expectCompleted(session, "owner", "block = bytearray(48 * 1024 * 1024)\nblock[::4096] = b'x' * len(block[::4096])\nlen(block)")
	if under.Usage.MemoryPeakBytes < 48<<20 || under.Usage.MemoryPeakBytes > 128<<20 {
		t.Fatalf("peak below the limit = %d", under.Usage.MemoryPeakBytes)
	}
	for round := 1; round <= 2; round++ {
		start := time.Now()
		result := h.exec(session, "owner", allocate640MiB, evalapi.Budget{WallSeconds: 30})
		if result.Succeeded() || strings.Contains(result.Stdout, "finished") {
			t.Fatalf("round %d: runaway allocation completed: %+v", round, result)
		}
		if result.StateStatus != evalapi.StateStatusLost {
			t.Fatalf("round %d: state = %q", round, result.StateStatus)
		}
		if !strings.Contains(result.Error, "memory limit exceeded") || !strings.Contains(result.Error, "oom_kill") {
			t.Fatalf("round %d: error does not name the kernel memory limit: %q", round, result.Error)
		}
		if result.Usage.MemoryPeakBytes > 128<<20 {
			t.Fatalf("round %d: peak %d exceeds memory.max", round, result.Usage.MemoryPeakBytes)
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("round %d: the limit took %s", round, elapsed)
		}
		if len(h.events.byType(evalapi.EventResourceLimit)) < round {
			t.Fatalf("round %d: no resource_limit event", round)
		}
		// The next cell starts a new, again-bounded worker.
		h.expectCompleted(session, "owner", "1 + 1")
		if got := readCgroupValue(t, workerCgroup(t, h.m, session.ID), "memory.max"); got != strconv.Itoa(128<<20) {
			t.Fatalf("round %d: restarted worker memory.max = %s", round, got)
		}
	}
	// Killed workers' cgroups are removed; only the live worker's remains.
	if leaves := h.m.cgroups.liveLeaves(); len(leaves) != 1 {
		t.Fatalf("worker cgroups after two OOM restarts = %v", leaves)
	}
}

// A descendant is bound by the same limit as the interpreter: spawning a child
// that allocates cannot escape the worker's budget.
func TestLinuxMemoryLimitBindsDescendants(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Budget.MemoryBytes = 128 << 20 })
	session := h.create("python", "owner", "owner")
	result := h.exec(session, "owner", `
import subprocess, sys
child = subprocess.run([sys.executable, "-c", '''
import time
chunks = []
for _ in range(80):
    chunks.append(bytearray(8 * 1024 * 1024))
    time.sleep(0.02)
print("child finished")
'''], start_new_session=True, capture_output=True, text=True)
print("child exit", child.returncode, child.stdout)
`, evalapi.Budget{WallSeconds: 30})
	if strings.Contains(result.Stdout, "child finished") {
		t.Fatalf("a descendant allocated past the worker limit: %+v", result)
	}
	if result.Succeeded() || result.StateStatus != evalapi.StateStatusLost {
		// memory.oom.group stops the whole worker, not only the child.
		t.Fatalf("a descendant OOM did not stop the worker: status=%q state=%q", result.Status, result.StateStatus)
	}
}

// One kernel's OOM does not disturb another kernel of the same manager.
func TestLinuxMemoryLimitIsPerWorker(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Budget.MemoryBytes = 128 << 20 })
	victim := h.create("python", "owner", "owner")
	bystander := h.create("python", "owner", "owner")
	h.expectCompleted(bystander, "owner", "kept = 41")
	bystanderPID, _ := strconv.Atoi(strings.TrimSpace(h.expectCompleted(bystander, "owner", "import os\nprint(os.getpid())").Stdout))
	before := hostProcessOf(t, h.m, bystander.ID, bystanderPID)
	if a, b := workerCgroup(t, h.m, victim.ID), workerCgroup(t, h.m, bystander.ID); a == b {
		t.Fatalf("two kernels share cgroup %s", a)
	}
	if result := h.exec(victim, "owner", allocate640MiB, evalapi.Budget{WallSeconds: 30}); result.Succeeded() {
		t.Fatalf("runaway allocation completed: %+v", result)
	}
	if !before.alive() {
		t.Fatalf("the bystander worker died with the victim")
	}
	kept := h.expectCompleted(bystander, "owner", "kept + 1")
	if len(kept.Displays) == 0 || kept.Displays[0].Text != "42" {
		t.Fatalf("bystander state lost: %+v", kept.Displays)
	}
}

// Without a usable cgroup no worker starts and no runtime claims a memory
// limit: an unreachable systemd user manager makes every runtime unavailable
// with the exact reason.
func TestLinuxWorkersFailClosedWithoutCgroup(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "no-runtime-dir"))
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "no-bus"))
	exe := requirePython(t)
	root := t.TempDir()
	m, err := New(Options{Root: filepath.Join(root, "state"), Python: exe})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.Dispose(context.Background())
	if m.cgroups != nil {
		t.Fatalf("a cgroup pool was created without a reachable user manager")
	}
	if manifests := mustDiscover(t, m); len(manifests) != 0 {
		t.Fatalf("runtimes offered without resource isolation: %+v", manifests)
	}
	workspace := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err = m.Create(context.Background(), evalapi.CreateOptions{RuntimeID: "python", OwnerID: "o", AgentID: "o", Workspace: workspace, Permission: "workspace"})
	if err == nil || !strings.Contains(err.Error(), "resource isolation unavailable") {
		t.Fatalf("Create without a cgroup = %v", err)
	}
}

// Cooperative interrupt reaches the interpreter, not the outer bwrap: the same
// host process keeps running, the kernel keeps its state, a second interrupt
// works the same way, and a running child of the cell stops with the cell.
func TestLinuxInterruptKeepsTheSandboxAndStopsChildren(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	pid, _ := strconv.Atoi(strings.TrimSpace(h.expectCompleted(session, "owner", "import os\nkept = 7\nprint(os.getpid())").Stdout))
	worker := hostProcessOf(t, h.m, session.ID, pid)
	s, _ := h.m.session(session.ID)
	outer := installedWorker(s).proc.pid
	for round := 1; round <= 2; round++ {
		done := make(chan evalapi.Result, 1)
		go func() {
			result, err := h.m.Execute(context.Background(), evalapi.Request{
				ID: "interrupt-" + strconv.Itoa(round), SessionID: session.ID, CellID: "c", AgentID: "owner",
				Code: "import subprocess, time\nchild = subprocess.Popen(['/bin/sleep', '60'])\nopen('child.pid', 'w').write(str(child.pid))\ntry:\n    time.sleep(60)\nfinally:\n    child.kill()\n    child.wait()",
			}, nil)
			if err != nil {
				done <- evalapi.Result{Status: "execute-error", Error: err.Error()}
				return
			}
			done <- result
		}()
		pidFile := filepath.Join(session.Workspace, "work", "child.pid")
		childPID, err := strconv.Atoi(waitForFile(t, pidFile, 20*time.Second))
		if err != nil {
			t.Fatalf("child pid: %v", err)
		}
		_ = os.Remove(pidFile)
		child := hostProcessOf(t, h.m, session.ID, childPID)
		if err := h.m.Interrupt(context.Background(), session.ID, "owner"); err != nil {
			t.Fatalf("round %d: Interrupt: %v", round, err)
		}
		var result evalapi.Result
		select {
		case result = <-done:
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: interrupted cell did not return", round)
		}
		if result.Status != evalapi.ResultStatusCancelled || result.StateStatus != evalapi.StateStatusOK {
			t.Fatalf("round %d: status=%q state=%q error=%q", round, result.Status, result.StateStatus, result.Error)
		}
		if !waitHostProcGone(child, 5*time.Second) {
			t.Fatalf("round %d: the cell's child %+v survived the interrupt", round, child)
		}
		if !worker.alive() || installedWorker(s).proc.pid != outer {
			t.Fatalf("round %d: the interrupt replaced the worker", round)
		}
	}
	kept := h.expectCompleted(session, "owner", "kept")
	if len(kept.Displays) == 0 || kept.Displays[0].Text != "7" {
		t.Fatalf("kernel state lost after interrupts: %+v", kept.Displays)
	}
}

// An interrupt for one kernel never reaches another kernel.
func TestLinuxInterruptIsScopedToItsKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	target := h.create("python", "owner", "owner")
	other := h.create("python", "owner", "owner")
	done := make(chan evalapi.Result, 2)
	run := func(session evalapi.Session, id string) {
		result, err := h.m.Execute(context.Background(), evalapi.Request{
			ID: id, SessionID: session.ID, CellID: "c", AgentID: "owner",
			Code: "import time\ntime.sleep(3)\nprint('slept')",
		}, nil)
		if err != nil {
			result = evalapi.Result{RequestID: id, Status: "execute-error", Error: err.Error()}
		}
		done <- result
	}
	go run(target, "target")
	go run(other, "other")
	time.Sleep(700 * time.Millisecond)
	if err := h.m.Interrupt(context.Background(), target.ID, "owner"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	results := map[string]evalapi.Result{}
	for range 2 {
		select {
		case result := <-done:
			results[result.RequestID] = result
		case <-time.After(30 * time.Second):
			t.Fatalf("cells did not return")
		}
	}
	if results["target"].Status != evalapi.ResultStatusCancelled {
		t.Fatalf("target = %+v", results["target"])
	}
	if got := results["other"]; got.Status != evalapi.ResultStatusCompleted || !strings.Contains(got.Stdout, "slept") {
		t.Fatalf("the other kernel was interrupted: %+v", got)
	}
}
