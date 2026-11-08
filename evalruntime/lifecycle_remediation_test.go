package evalruntime

// Permanent regressions for the execution-lifecycle and callback-authority
// findings of the unified-execution-runtime review. Every test drives a real
// worker process through the public Manager API: no /tmp paths, no mocked
// processes, and no assertions on log text alone.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

var lifecycleSeq atomic.Int64

// lifecycleExec runs one cell and returns the raw error, so an admission refusal
// can be asserted instead of failing the test inside the harness helper.
func lifecycleExec(t *testing.T, h *harness, session evalapi.Session, agent, code string, mod func(*evalapi.Request), caller evalapi.ToolCaller) (evalapi.Result, error, time.Duration) {
	t.Helper()
	req := evalapi.Request{
		ID:        fmt.Sprintf("lc-%d", lifecycleSeq.Add(1)),
		SessionID: session.ID,
		CellID:    "cell",
		AgentID:   agent,
		Code:      code,
	}
	if mod != nil {
		mod(&req)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	start := time.Now()
	result, err := h.m.Execute(ctx, req, caller)
	return result, err, time.Since(start)
}

func echoCaller(fn func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error)) *fakeCaller {
	return &fakeCaller{specs: []protocol.ToolSpec{echoSpec()}, fn: fn}
}

// installedWorker reads the session's current worker under its own lock.
func installedWorker(s *session) *worker {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.worker
}

// waitForFile polls a file inside the kernel workspace; a cell uses it to
// publish its pid before it starts ignoring interrupts.
func waitForFile(t *testing.T, path string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return strings.TrimSpace(string(data))
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

// SPI-1: a worker numbers its callbacks from its own counter, which restarts
// with the process. A reset or a crash must not turn the next cell's "c1" into a
// duplicate-id denial.
func TestCallbacksSurviveResetAndCrash(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	caller := echoCaller(func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		return protocol.ToolResult{OK: true, Content: "echo:" + fmt.Sprint(args["text"])}, nil
	})
	allow := func(req *evalapi.Request) { req.AllowedCapabilities = []string{"echo"} }

	first, err, _ := lifecycleExec(t, h, session, "owner", `print(lc_tool("echo", text="gen1"))`, allow, caller)
	if err != nil || !first.Succeeded() || !strings.Contains(first.Stdout, "echo:gen1") {
		t.Fatalf("first generation callback = %+v err=%v", first, err)
	}

	if err := h.m.Reset(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	second, err, _ := lifecycleExec(t, h, session, "owner", `print(lc_tool("echo", text="gen2"))`, allow, caller)
	if err != nil || !second.Succeeded() || !strings.Contains(second.Stdout, "echo:gen2") {
		t.Fatalf("callback after reset = %+v err=%v", second, err)
	}
	if second.Generation <= first.Generation {
		t.Fatalf("reset did not start a new generation: %d -> %d", first.Generation, second.Generation)
	}

	// The crash is the setup; the next cell restarts the kernel.
	_, _, _ = lifecycleExec(t, h, session, "owner", "import os\nos._exit(1)", nil, nil)
	third, err, _ := lifecycleExec(t, h, session, "owner", `print(lc_tool("echo", text="gen3"))`, allow, caller)
	if err != nil || !third.Succeeded() || !strings.Contains(third.Stdout, "echo:gen3") {
		t.Fatalf("callback after crash = %+v err=%v", third, err)
	}
	if caller.callCount() != 3 {
		t.Fatalf("host tool executions = %d, want 3", caller.callCount())
	}
}

// SPI-2: Reset must not return while a worker it did not stop is still alive.
func TestResetLeavesNoWorkerBehind(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")

	before := h.expectCompleted(session, "owner", "import os\nos.getpid()")
	if len(before.Displays) == 0 {
		t.Fatalf("no pid display: %+v", before.Displays)
	}
	firstPID, convErr := strconv.Atoi(strings.TrimSpace(before.Displays[0].Text))
	if convErr != nil {
		t.Fatalf("pid display %q: %v", before.Displays[0].Text, convErr)
	}
	// The printed pid is a sandbox pid; the host identity is what must vanish.
	first := hostProcessOf(t, h.m, session.ID, firstPID)

	// A cell holds the kernel while another is queued behind it, then Reset runs.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _, _ = lifecycleExec(t, h, session, "owner", "import time\ntime.sleep(2)", nil, nil)
	}()
	time.Sleep(250 * time.Millisecond)
	go func() {
		defer wg.Done()
		_, _, _ = lifecycleExec(t, h, session, "owner", "import os\nos.getpid()", nil, nil)
	}()
	time.Sleep(250 * time.Millisecond)
	if err := h.m.Reset(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	wg.Wait()

	if first.alive() {
		t.Fatalf("worker %+v survived Reset", first)
	}
	// Exactly one worker is installed, and it is a new process.
	s, err := h.m.session(session.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	w := installedWorker(s)
	if w == nil || !w.alive() {
		t.Fatalf("Reset left the kernel without a live worker")
	}
	after := h.expectCompleted(session, "owner", "import os\nos.getpid()")
	if len(after.Displays) == 0 {
		t.Fatalf("no pid display after reset: %+v", after.Displays)
	}
	secondPID, convErr := strconv.Atoi(strings.TrimSpace(after.Displays[0].Text))
	if convErr != nil {
		t.Fatalf("pid display %q: %v", after.Displays[0].Text, convErr)
	}
	// A new PID namespace may hand out the same sandbox pid again; the host
	// process must still be a different, live one.
	second := hostProcessOf(t, h.m, session.ID, secondPID)
	if second.sameProcess(first) || !second.alive() {
		t.Fatalf("reset reused worker %+v (now %+v)", first, second)
	}
}

// SPI-2: Close must not return while a restart it raced is still starting a
// worker. A closed session never keeps a worker.
func TestCloseDuringRestartLeavesNoWorker(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")

	// The crash is the setup: the next Execute restarts the kernel.
	_, _, _ = lifecycleExec(t, h, session, "owner", "import os\nos._exit(1)", nil, nil)
	done := make(chan struct{})
	var observed atomic.Int64
	go func() {
		defer close(done)
		result, err, _ := lifecycleExec(t, h, session, "owner", "import os, time\ntime.sleep(1)\nos.getpid()", nil, nil)
		if err == nil && len(result.Displays) > 0 {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(result.Displays[0].Text)); convErr == nil {
				observed.Store(int64(pid))
			}
		}
	}()
	time.Sleep(30 * time.Millisecond) // inside startWorker
	if err := h.m.Close(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done

	// The restarted worker may print its pid and then be killed by Close, so it
	// cannot be mapped to a host process afterwards. The kernel's record is
	// checked instead: no worker cgroup may remain (Linux), and off Linux the
	// sandbox pid is the host pid.
	if !workerProcessesGone(h.m) {
		t.Fatalf("a worker cgroup survived Close: %v", h.m.cgroups.liveLeaves())
	}
	if pid := int(observed.Load()); pid > 0 && runtime.GOOS != "linux" && processAlive(pid) {
		t.Fatalf("restart worker %d survived Close", pid)
	}
	s, err := h.m.session(session.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if w := installedWorker(s); w != nil {
		t.Fatalf("Close left a worker installed")
	}
	public, err := h.m.Session(context.Background(), session.ID, "owner")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if public.State != evalapi.StateStopped && public.State != evalapi.StateFailed {
		t.Fatalf("state after close = %q", public.State)
	}
}

// SPI-2: Dispose stops every worker and waits for it.
func TestDisposeLeavesNoWorkerBehind(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	pidResult := h.expectCompleted(session, "owner", "import os\nos.getpid()")
	if len(pidResult.Displays) == 0 {
		t.Fatalf("no pid display: %+v", pidResult.Displays)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(pidResult.Displays[0].Text))
	if convErr != nil {
		t.Fatalf("pid display: %v", convErr)
	}
	worker := hostProcessOf(t, h.m, session.ID, pid)
	if err := h.m.Dispose(context.Background()); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if worker.alive() {
		t.Fatalf("worker %+v survived Dispose", worker)
	}
	if !workerProcessesGone(h.m) {
		t.Fatalf("a worker cgroup survived Dispose: %v", h.m.cgroups.liveLeaves())
	}
}

// SPI-3: an interrupt that the cell ignores must escalate to a forced stop, and
// the stop must be complete when Interrupt returns.
func TestInterruptEscalationStopsAnIgnoringCell(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	pidPath := filepath.Join(session.Workspace, "work", "pid.txt")

	done := make(chan evalapi.Result, 1)
	go func() {
		result, err, _ := lifecycleExec(t, h, session, "owner", `
import os, time
open('pid.txt', 'w').write(str(os.getpid()))
while True:
    try:
        time.sleep(30)
    except KeyboardInterrupt:
        pass
`, func(req *evalapi.Request) { req.Budget.WallSeconds = 60 }, nil)
		if err != nil {
			done <- evalapi.Result{Status: "execute-error", Error: err.Error()}
			return
		}
		done <- result
	}()

	pid, convErr := strconv.Atoi(waitForFile(t, pidPath, 20*time.Second))
	if convErr != nil {
		t.Fatalf("pid file: %v", convErr)
	}
	worker := hostProcessOf(t, h.m, session.ID, pid)
	time.Sleep(300 * time.Millisecond)

	start := time.Now()
	if err := h.m.Interrupt(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("interrupt escalation took %s", elapsed)
	}
	// The guarantee: after Interrupt returns, the owned pid is gone.
	if worker.alive() {
		t.Fatalf("worker %+v survived an ignored interrupt", worker)
	}
	select {
	case result := <-done:
		if result.Status == "execute-error" {
			t.Fatalf("interrupted cell returned an execute error: %s", result.Error)
		}
		if result.Succeeded() {
			t.Fatalf("an ignored interrupt still completed: %+v", result)
		}
		if result.StateStatus != evalapi.StateStatusLost && result.StateStatus != evalapi.StateStatusOK {
			t.Fatalf("state after a forced stop = %q", result.StateStatus)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("interrupted execution never returned")
	}
	// The kernel restarts and works.
	if next := h.expectCompleted(session, "owner", "1 + 1"); len(next.Displays) == 0 || next.Displays[0].Text != "2" {
		t.Fatalf("kernel unusable after a forced interrupt: %+v", next.Displays)
	}
}

// SPI-5: disk accounting must fail closed. A directory too large to account
// within the entry bound is treated as over quota, never as an undercount.
func TestDiskAccountingFailsClosedAtTheEntryBound(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Budget.DiskBytes = 2 << 20 })
	session := h.create("python", "owner", "owner")

	result, err, elapsed := lifecycleExec(t, h, session, "owner", `
import os
home = os.environ['HOME']
for i in range(4300):
    open(os.path.join(home, 'e%05d' % i), 'w').close()
'created'
`, func(req *evalapi.Request) { req.Budget.WallSeconds = 60 }, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if elapsed > 45*time.Second {
		t.Fatalf("the entry-bound bypass was not bounded: %s", elapsed)
	}

	var reason string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && reason == "" {
		for _, event := range h.events.byType(evalapi.EventResourceLimit) {
			data, ok := event.Data.(map[string]string)
			if !ok {
				continue
			}
			if text := data["reason"]; strings.Contains(text, "accounting") || strings.Contains(text, "quota") {
				reason = text
			}
		}
		if reason == "" {
			time.Sleep(100 * time.Millisecond)
		}
	}
	if reason == "" {
		t.Fatalf("no limit event for an unaccountable kernel directory; result=%+v", result)
	}
	if result.StateStatus != evalapi.StateStatusLost && result.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("state = %q", result.StateStatus)
	}
	// The kernel is either still usable or was discarded; it is never silently
	// over quota.
	next := h.exec(session, "owner", "1 + 1", evalapi.Budget{})
	if !next.Succeeded() && next.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("unexpected state after a disk limit: %+v", next)
	}
}

// SPI-6: the callback budget is reserved atomically, so concurrent worker
// goroutines cannot overshoot MaxCallbacks.
func TestCallbackBudgetIsReservedAtomically(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")
	caller := echoCaller(func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
		time.Sleep(50 * time.Millisecond)
		return protocol.ToolResult{OK: true, Content: "ok"}, nil
	})
	result, err, _ := lifecycleExec(t, h, session, "owner",
		`const rs = await Promise.all(Array.from({length: 20}, (_, i) => lc_tool('echo', {text: String(i)}).then(() => 'ok', e => e.name))); JSON.stringify(rs)`,
		func(req *evalapi.Request) {
			req.AllowedCapabilities = []string{"echo"}
			req.Budget.MaxCallbacks = 3
		}, caller)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Usage.Callbacks > 3 {
		t.Fatalf("callback budget overshot: usage.callbacks = %d", result.Usage.Callbacks)
	}
	if caller.callCount() > 3 {
		t.Fatalf("callback budget overshot: host executions = %d, MaxCallbacks = 3", caller.callCount())
	}
	if caller.callCount() == 0 {
		t.Fatalf("the budget denied every call; the happy path regressed")
	}
}

// SPI-7: a kernel whose worker died still holds its slot, so a restart can never
// push the host over MaxWorkers.
func TestKernelCapCountsACrashedKernel(t *testing.T) {
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Budget.MaxWorkers = 2 })
	first := h.create("python", "owner", "owner")
	second := h.create("python", "owner", "owner")

	// Crash the first kernel's worker between cells.
	_, _, _ = lifecycleExec(t, h, first, "owner", "import os\nos._exit(1)", nil, nil)
	time.Sleep(500 * time.Millisecond)

	_, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "python", OwnerID: "owner", AgentID: "owner",
		Workspace: h.workspace, Permission: "workspace",
	})
	if !errors.Is(err, evalapi.ErrLimit) {
		t.Fatalf("Create over the cap after a crash = %v, want ErrLimit", err)
	}
	// The crashed kernel's slot was never released: it can still restart.
	if result := h.expectCompleted(first, "owner", "1 + 1"); !result.Succeeded() {
		t.Fatalf("the crashed kernel could not restart: %+v", result)
	}
	_ = second
}

// SPI-8: one kernel's slow startup never serializes an unrelated Create.
func TestCreateIsNotSerializedByAnotherStartup(t *testing.T) {
	h, manifest := newFixtureHarness(t)
	slow := manifest
	slow.ID = "fixture-slow"
	slow.Args = []string{"--init-delay=2s"}
	if err := h.m.Register(slow); err != nil {
		t.Fatalf("Register slow runtime: %v", err)
	}

	slowDone := make(chan error, 1)
	go func() {
		_, err := h.m.Create(context.Background(), evalapi.CreateOptions{
			RuntimeID: "fixture-slow", OwnerID: "owner-slow", AgentID: "owner-slow",
			Workspace: h.workspace, Permission: "workspace",
		})
		slowDone <- err
	}()
	time.Sleep(200 * time.Millisecond)

	start := time.Now()
	if _, err := h.m.Create(context.Background(), evalapi.CreateOptions{
		RuntimeID: "fixture", OwnerID: "owner-fast", AgentID: "owner-fast",
		Workspace: h.workspace, Permission: "workspace",
	}); err != nil {
		t.Fatalf("Create while another kernel starts: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("Create waited %s for another kernel's startup", elapsed)
	}
	if err := <-slowDone; err != nil {
		t.Fatalf("slow Create: %v", err)
	}
}

// SPI-9: a worker that dies between cells is reported as a lost kernel, not as a
// healthy idle session.
func TestIdleWorkerDeathIsReportedAsLost(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	before := h.expectCompleted(session, "owner", "kept = 1")

	h.expectCompleted(session, "owner", `
import threading, time, os
threading.Thread(target=lambda: (time.sleep(0.6), os._exit(9)), daemon=True).start()
'armed'
`)
	time.Sleep(1800 * time.Millisecond)

	public, err := h.m.Session(context.Background(), session.ID, "owner")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if public.State != evalapi.StateFailed {
		t.Fatalf("state after an idle death = %q, want failed", public.State)
	}
	if public.LastError == "" {
		t.Fatalf("no loss reason recorded")
	}
	found := false
	for _, event := range h.events.byType(evalapi.EventWorkerCrashed) {
		data, ok := event.Data.(map[string]any)
		if ok && data["stateStatus"] == evalapi.StateStatusLost {
			found = true
		}
	}
	if !found {
		t.Fatalf("no loss-flagged worker_crashed event: %+v", h.events.byType(evalapi.EventWorkerCrashed))
	}
	next := h.expectCompleted(session, "owner", "1 + 1")
	if next.Generation != before.Generation+1 {
		t.Fatalf("generation after a loss = %d, want %d", next.Generation, before.Generation+1)
	}
}

// SPI-10: an oversized cell is an admission error, never a reason to kill a
// healthy kernel.
func TestOversizedCodeIsRefusedWithoutKillingTheKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 41")

	_, err, _ := lifecycleExec(t, h, session, "owner", "# "+strings.Repeat("x", maxCodeBytes+1024), nil, nil)
	if !errors.Is(err, evalapi.ErrLimit) {
		t.Fatalf("oversized code = %v, want ErrLimit", err)
	}
	kept := h.expectCompleted(session, "owner", "kept + 1")
	if len(kept.Displays) == 0 || kept.Displays[0].Text != "42" {
		t.Fatalf("the kernel did not survive an oversized admission: %+v", kept.Displays)
	}
}

// SPI-10: a large display is spilled to an artifact instead of overflowing the
// control frame and killing the worker.
func TestLargeDisplayIsSpilledNotFatal(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")

	result, err, _ := lifecycleExec(t, h, session, "owner",
		"import os\nb'\\x89PNG\\r\\n\\x1a\\n' + os.urandom(3_000_000)", nil, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("kernel lost on a large display: %+v", result)
	}
	if len(result.Artifacts) == 0 {
		t.Fatalf("large display was neither inline nor stored: displays=%+v", result.Displays)
	}
	if result.Artifacts[0].Size < 2_000_000 {
		t.Fatalf("spilled display is %d bytes, want the full payload", result.Artifacts[0].Size)
	}
	if next := h.expectCompleted(session, "owner", "1 + 1"); len(next.Displays) == 0 {
		t.Fatalf("kernel unusable after a spilled display")
	}
}

// SPI-11: an interrupt that lands while the result is being rendered must still
// produce a reply instead of losing the cell.
func TestInterruptDuringResultRenderingStillReplies(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")

	done := make(chan evalapi.Result, 1)
	go func() {
		result, err, _ := lifecycleExec(t, h, session, "owner", `
import time
class Slow:
    def __repr__(self):
        time.sleep(30)
        return 'never'
Slow()
`, func(req *evalapi.Request) { req.Budget.WallSeconds = 60 }, nil)
		if err != nil {
			done <- evalapi.Result{Status: "execute-error", Error: err.Error()}
			return
		}
		done <- result
	}()
	time.Sleep(700 * time.Millisecond)
	if err := h.m.Interrupt(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	select {
	case result := <-done:
		if result.Status == "execute-error" {
			t.Fatalf("the interrupted cell produced no result: %s", result.Error)
		}
		if result.Status != evalapi.ResultStatusCancelled && result.Status != evalapi.ResultStatusFailed && result.Status != evalapi.ResultStatusTimeout {
			t.Fatalf("status after an interrupt during rendering = %q", result.Status)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("no reply after an interrupt during result rendering")
	}
}

// SPI-13: output a JavaScript cell writes to the real stdout belongs to that
// cell, not to the next one.
func TestJavaScriptDirectStdoutBelongsToItsCell(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")

	first := h.expectCompleted(session, "owner", "process.stdout.write('DIRECT-ONE'); 'done'")
	if !strings.Contains(first.Stdout, "DIRECT-ONE") {
		t.Fatalf("stdout not attributed to the cell that wrote it: %q", first.Stdout)
	}
	second := h.expectCompleted(session, "owner", "'second'")
	if strings.Contains(second.Stdout, "DIRECT-ONE") {
		t.Fatalf("output leaked into the next cell: %q", second.Stdout)
	}
}

// SPI-14: an artifact flood is bounded and the kernel survives it.
func TestArtifactFloodIsBounded(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")

	start := time.Now()
	result, err, _ := lifecycleExec(t, h, session, "owner", `
for i in range(300):
    lc_artifact('x', name='a%03d.txt' % i)
'flooded'
`, func(req *evalapi.Request) { req.Budget.WallSeconds = 60 }, nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("artifact flood was not bounded: %s", elapsed)
	}
	if len(result.Artifacts) > maxCellArtifacts {
		t.Fatalf("adopted %d artifacts, the per-cell limit is %d", len(result.Artifacts), maxCellArtifacts)
	}
	if result.Succeeded() {
		t.Fatalf("a cell that exceeded the artifact limit reported success: %+v", result)
	}
	if !strings.Contains(result.Error, "artifact") {
		t.Fatalf("the artifact limit was not reported clearly: %q", result.Error)
	}
	if next := h.expectCompleted(session, "owner", "1 + 1"); !next.Succeeded() {
		t.Fatalf("kernel unusable after an artifact flood: %+v", next)
	}
}

// A JavaScript cell that throws keeps the state it wrote to already-declared
// bindings; a `const`/`let` introduced by the throwing cell is gone with its
// block scope, which is JavaScript's own rule (a Python cell behaves the same
// way for names bound inside a failing statement).
func TestJavaScriptThrowKeepsPreviouslyDeclaredState(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")

	h.expectCompleted(session, "owner", "let counter = 1; function bump(){ counter += 1; return counter; }")
	thrown := h.exec(session, "owner", "bump(); throw new Error('boom')", evalapi.Budget{})
	if thrown.Status != evalapi.ResultStatusFailed {
		t.Fatalf("status = %q", thrown.Status)
	}
	after := h.expectCompleted(session, "owner", "counter")
	if len(after.Displays) == 0 || after.Displays[0].Text != "2" {
		t.Fatalf("state written before the throw was lost: %+v", after.Displays)
	}
}

// OPUS-P1-01 (fixed part): a callback is bound to the execution it belongs to.
// A tool call that names a different execution never reaches the host caller.
func TestToolCallNamingAnotherExecutionIsDenied(t *testing.T) {
	h, _ := newFixtureHarness(t)
	created := h.create("fixture", "owner", "owner")
	session, err := h.m.session(created.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	caller := echoCaller(nil)
	budget := evalapi.Budget{MaxCallbacks: 4, MaxDepth: 2, OutputBytes: 8 << 10, ArtifactBytes: 1 << 20}
	call := newActiveCall(context.Background(), session, caller, evalapi.Request{ID: "current", AllowedCapabilities: []string{"echo"}}, budget, time.Now().Add(time.Minute))
	if _, err := call.loadTools(context.Background()); err != nil {
		t.Fatalf("loadTools: %v", err)
	}
	w := &worker{s: session, m: h.m}
	w.active.Store(call)

	params, err := json.Marshal(wireToolCall{
		CallID: "c1", Name: "echo", Arguments: map[string]any{"text": "x"}, RequestID: "stale",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	value, err := w.handleRequest(context.Background(), "tool/call", params)
	if err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	denied, ok := value.(map[string]any)
	if !ok || denied["denied"] != true {
		t.Fatalf("a callback naming a stale execution was not denied: %+v", value)
	}
	if !strings.Contains(fmt.Sprint(denied["content"]), "stale") {
		t.Fatalf("denial does not name the mismatched execution: %v", denied["content"])
	}
	if caller.callCount() != 0 {
		t.Fatalf("a stale callback reached the host caller")
	}
	// The same call naming the current execution is served.
	params, err = json.Marshal(wireToolCall{
		CallID: "c2", Name: "echo", Arguments: map[string]any{"text": "x"}, RequestID: "current",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if _, err := w.handleRequest(context.Background(), "tool/call", params); err != nil {
		t.Fatalf("handleRequest: %v", err)
	}
	if caller.callCount() != 1 {
		t.Fatalf("a correctly bound callback was not served: %d", caller.callCount())
	}
}

// OPUS-P1-01 (closed by authority-domain isolation): audit events name the actor
// whose domain ran the cell, and a granted actor cannot run in the owner's kernel.
func TestToolAuditNamesTheExecutingActor(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner-a", "owner-a")
	if err := h.m.Grant(context.Background(), session.ID, "owner-a", evalapi.Grant{
		AgentID: "child-b", Role: evalapi.RoleObserver,
	}); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	// The grantee has no execution authority in this kernel at all.
	if _, err, _ := lifecycleExec(t, h, session, "child-b", "1 + 1", nil, nil); !errors.Is(err, evalapi.ErrAuthorityDomain) {
		t.Fatalf("grantee execute = %v, want ErrAuthorityDomain", err)
	}
	caller := echoCaller(nil)
	result, err, _ := lifecycleExec(t, h, session, "owner-a", `print(lc_tool("echo", text="as-owner"))`,
		func(req *evalapi.Request) { req.AllowedCapabilities = []string{"echo"} }, caller)
	if err != nil || !result.Succeeded() {
		t.Fatalf("owner cell = %+v err=%v", result, err)
	}
	calls := h.events.byType(evalapi.EventToolCall)
	if len(calls) == 0 {
		t.Fatalf("no tool_call audit event")
	}
	last := calls[len(calls)-1]
	if last.AgentID != "owner-a" {
		t.Fatalf("audit attributed the call to %q, want the executing owner", last.AgentID)
	}
	if last.OwnerID != "owner-a" {
		t.Fatalf("audit owner = %q, want owner-a", last.OwnerID)
	}
	if last.RequestID == "" {
		t.Fatalf("audit event has no execution id")
	}
}

// OPUS-P1-01 (closed): two actors never share a live heap, so a tool with the
// same name but a different scope/backing cannot be reached across domains. Each
// kernel calls its own host caller, and a grant only shares read access.
func TestSameNamedToolStaysInItsOwnDomain(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	ownerKernel := h.create("python", "owner-a", "owner-a")
	childKernel := h.create("python", "child-b", "child-b")
	if ownerKernel.ID == childKernel.ID {
		t.Fatalf("two actors received the same kernel")
	}
	if err := h.m.Grant(context.Background(), ownerKernel.ID, "owner-a", evalapi.Grant{
		AgentID: "child-b", Role: evalapi.RoleObserver,
	}); err != nil {
		t.Fatalf("Grant read access: %v", err)
	}

	var ownerCalls, childCalls atomic.Int64
	var mu sync.Mutex
	var ownerNotes, childNotes []string
	record := func(sink *[]string, counter *atomic.Int64) func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
		return func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
			counter.Add(1)
			mu.Lock()
			*sink = append(*sink, fmt.Sprint(args["note"]))
			mu.Unlock()
			return protocol.ToolResult{OK: true, Content: "ok"}, nil
		}
	}
	// The same tool name, two different host-side scopes.
	ownerCaller := &fakeCaller{specs: []protocol.ToolSpec{{Name: "write_file", Parameters: map[string]any{"type": "object"}}},
		fn: record(&ownerNotes, &ownerCalls)}
	childCaller := &fakeCaller{specs: []protocol.ToolSpec{{Name: "write_file", Parameters: map[string]any{"type": "object"}}},
		fn: record(&childNotes, &childCalls)}
	allow := func(req *evalapi.Request) { req.AllowedCapabilities = []string{"write_file"} }

	if result, err, _ := lifecycleExec(t, h, ownerKernel, "owner-a", `lc_tool("write_file", note="owner-A")`, allow, ownerCaller); err != nil || !result.Succeeded() {
		t.Fatalf("owner cell = %+v err=%v", result, err)
	}
	if result, err, _ := lifecycleExec(t, h, childKernel, "child-b", `lc_tool("write_file", note="child-B")`, allow, childCaller); err != nil || !result.Succeeded() {
		t.Fatalf("child cell = %+v err=%v", result, err)
	}
	mu.Lock()
	ownerSeen := append([]string(nil), ownerNotes...)
	childSeen := append([]string(nil), childNotes...)
	mu.Unlock()
	if len(ownerSeen) != 1 || ownerSeen[0] != "owner-A" {
		t.Fatalf("owner domain calls = %v", ownerSeen)
	}
	if len(childSeen) != 1 || childSeen[0] != "child-B" {
		t.Fatalf("child domain calls = %v", childSeen)
	}
	// A grant does not create a way into the owner's process.
	if _, err, _ := lifecycleExec(t, h, ownerKernel, "child-b", `lc_tool("write_file", note="cross")`, allow, childCaller); !errors.Is(err, evalapi.ErrAuthorityDomain) {
		t.Fatalf("cross-domain cell = %v, want ErrAuthorityDomain", err)
	}
	if childCalls.Load() != 1 {
		t.Fatalf("a cross-domain attempt reached the child caller: %d", childCalls.Load())
	}
}

// OPUS-P1-01 (closed): code an actor left running in its own kernel cannot reach
// another actor's caller, before or after a revocation, and the owner's own
// capability stays intact.
func TestRevokedActorsThreadCannotReachTheOwnersCapability(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	ownerKernel := h.create("python", "owner-a", "owner-a")
	childKernel := h.create("python", "child-b", "child-b")
	if err := h.m.Grant(context.Background(), ownerKernel.ID, "owner-a", evalapi.Grant{
		AgentID: "child-b", Role: evalapi.RoleObserver,
	}); err != nil {
		t.Fatalf("Grant read access: %v", err)
	}

	var ownerCalls, childCalls atomic.Int64
	var mu sync.Mutex
	var ownerNotes, childNotes []string
	record := func(sink *[]string, counter *atomic.Int64) func(context.Context, string, map[string]any) (protocol.ToolResult, error) {
		return func(ctx context.Context, name string, args map[string]any) (protocol.ToolResult, error) {
			counter.Add(1)
			mu.Lock()
			*sink = append(*sink, fmt.Sprint(args["note"]))
			mu.Unlock()
			return protocol.ToolResult{OK: true, Content: "ok"}, nil
		}
	}
	ownerCaller := &fakeCaller{specs: []protocol.ToolSpec{{Name: "write_file", Parameters: map[string]any{"type": "object"}}},
		fn: record(&ownerNotes, &ownerCalls)}
	childCaller := &fakeCaller{specs: []protocol.ToolSpec{{Name: "write_file", Parameters: map[string]any{"type": "object"}}},
		fn: record(&childNotes, &childCalls)}
	allow := func(req *evalapi.Request) { req.AllowedCapabilities = []string{"write_file"} }

	// The child plants a thread in its own kernel that keeps calling the tool.
	if result, err, _ := lifecycleExec(t, h, childKernel, "child-b", `
import threading, time
def spy():
    while True:
        time.sleep(0.02)
        try:
            lc_tool("write_file", note="child-thread")
        except Exception:
            pass
threading.Thread(target=spy, daemon=True).start()
'planted'
`, nil, childCaller); err != nil || !result.Succeeded() {
		t.Fatalf("planting cell = %+v err=%v", result, err)
	}
	// Revoke the read grant, then run the owner's own cell.
	if err := h.m.Revoke(context.Background(), ownerKernel.ID, "owner-a", "child-b"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	before := ownerCalls.Load()
	if result, err, _ := lifecycleExec(t, h, ownerKernel, "owner-a", `
import time
time.sleep(0.5)
lc_tool("write_file", note="owner-only")
`, allow, ownerCaller); err != nil || !result.Succeeded() {
		t.Fatalf("owner cell after revoke = %+v err=%v", result, err)
	}
	mu.Lock()
	ownerSeen := append([]string(nil), ownerNotes...)
	mu.Unlock()
	if len(ownerSeen) != 1 || ownerSeen[0] != "owner-only" {
		t.Fatalf("owner caller saw %v; another actor's thread reached it", ownerSeen)
	}
	if after := ownerCalls.Load(); after != before+1 {
		t.Fatalf("owner caller count %d -> %d", before, after)
	}
	for _, note := range ownerSeen {
		if strings.Contains(note, "child") {
			t.Fatalf("owner caller received a cross-domain call: %q", note)
		}
	}
	// The child's own kernel keeps its persistent execution, and its own
	// callbacks still work while one of its cells is running (a thread may only
	// call the bridge during an active execution).
	if result, err, _ := lifecycleExec(t, h, childKernel, "child-b", `
import time
time.sleep(0.4)
print("still alive")
`, allow, childCaller); err != nil || !result.Succeeded() {
		t.Fatalf("child kernel after revoke = %+v err=%v", result, err)
	}
	if childCalls.Load() == 0 {
		t.Fatalf("the child's own callbacks stopped working")
	}
}
