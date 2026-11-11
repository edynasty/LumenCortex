package evalruntime

// Cross-review (B2 process/eval/DAP) regressions:
//
//  1. An Inspect whose context is cancelled or has expired must not stop,
//     restart or reset the kernel: the same worker keeps running with the same
//     generation and its variables intact.
//  2. Closing one kernel owner must leave another session's kernel (and its
//     census row) alone.
//  5. macOS Seatbelt: a program profile (DAP adapter/debuggee, command hook)
//     and an Eval kernel can neither signal the other nor a host process.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/procrt"
)

func TestXReviewInspectCancellationKeepsKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "xreview-inspect", "xreview-inspect")
	first := h.expectCompleted(session, "xreview-inspect", "kept = 41\nprint(kept + 1)")
	if first.Generation == 0 {
		t.Fatalf("no generation: %+v", first)
	}
	s, err := h.m.session(session.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	before := installedWorker(s)
	if before == nil || !before.alive() {
		t.Fatalf("no live worker before Inspect")
	}

	// Already-expired context: the probe can never wait for an answer.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	value, err := h.m.Inspect(expired, session.ID, "xreview-inspect", evalapi.InspectOptions{})
	if err == nil {
		// The worker may answer before the call observes the expiry; that is
		// not a failure. The invariants below still have to hold.
		t.Logf("Inspect answered despite an expired context: alive=%v entries=%d", value.Alive, len(value.Entries))
	} else if !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("Inspect with an expired context returned %v", err)
	}

	// A live caller can cancel while the probe is queued: the cancellation
	// must not reach the worker's process.
	ctx, cancelCall := context.WithCancel(context.Background())
	cancelCall()
	if _, err := h.m.Inspect(ctx, session.ID, "xreview-inspect", evalapi.InspectOptions{}); err == nil {
		t.Logf("Inspect answered despite a cancelled context")
	}

	after := installedWorker(s)
	if after == nil || !after.alive() {
		t.Fatalf("Inspect cancellation killed the kernel")
	}
	if before.proc.pid != after.proc.pid || before.generation != after.generation {
		t.Fatalf("Inspect restarted the kernel: pid %d->%d generation %d->%d", before.proc.pid, after.proc.pid, before.generation, after.generation)
	}
	view, err := h.m.Session(context.Background(), session.ID, "xreview-inspect")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if view.Generation != first.Generation {
		t.Fatalf("kernel generation changed across Inspect: %d -> %d", first.Generation, view.Generation)
	}
	// Variables survived: this is the state a cancelled inspection must not lose.
	kept := h.expectCompleted(session, "xreview-inspect", "print(kept + 1)")
	if !strings.Contains(kept.Stdout, "42") || kept.Generation != first.Generation {
		t.Fatalf("kernel state lost after cancelled Inspect: stdout=%q generation=%d", kept.Stdout, kept.Generation)
	}
	if rows := censusRows("xreview-inspect"); len(rows) != 1 || rows[0].PID != after.proc.pid {
		t.Fatalf("census after cancelled Inspect = %+v", rows)
	}
}

func TestXReviewCloseOwnerLeavesForeignKernelAlone(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	first := h.create("python", "xreview-close-a", "xreview-close-a")
	second := h.create("python", "xreview-close-b", "xreview-close-b")
	h.expectCompleted(first, "xreview-close-a", "a = 1\nprint(a)")
	h.expectCompleted(second, "xreview-close-b", "b = 2\nprint(b)")

	sb, err := h.m.session(second.ID)
	if err != nil {
		t.Fatalf("second session: %v", err)
	}
	foreign := installedWorker(sb)
	if foreign == nil || !foreign.alive() {
		t.Fatalf("second kernel is not running")
	}

	if err := h.m.CloseOwner(context.Background(), "xreview-close-a"); err != nil {
		t.Fatalf("CloseOwner: %v", err)
	}
	waitCensusEmpty(t, "xreview-close-a")
	if !foreign.alive() {
		t.Fatalf("closing another owner killed kernel %d", foreign.proc.pid)
	}
	if rows := censusRows("xreview-close-b"); len(rows) != 1 || rows[0].PID != foreign.proc.pid {
		t.Fatalf("census for the untouched kernel = %+v", rows)
	}
	kept := h.expectCompleted(second, "xreview-close-b", "print(b + 39)")
	if !strings.Contains(kept.Stdout, "41") {
		t.Fatalf("untouched kernel lost its state: %q", kept.Stdout)
	}
	// It is still the same worker, not a restart.
	if installedWorker(sb) != foreign {
		t.Fatalf("untouched kernel was restarted")
	}
	_ = h.m.CloseOwner(context.Background(), "xreview-close-b")
	waitCensusEmpty(t, "xreview-close-b")
}

// TestXReviewProgramAndKernelCannotSignalEachOther checks the shared Seatbelt
// profile after PED-1 in both directions: a program (the profile DAP adapters,
// debuggees and command hooks use) cannot signal an Eval kernel, and an Eval
// cell cannot signal a program. A host process is the outside control in both
// cases.
func TestXReviewProgramAndKernelCannotSignalEachOther(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Seatbelt signal scoping is macOS-specific")
	}
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "xreview-signal", "xreview-signal")
	h.expectCompleted(session, "xreview-signal", "1 + 1")
	s, err := h.m.session(session.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	w := installedWorker(s)
	if w == nil || w.proc == nil {
		t.Fatalf("no worker")
	}
	kernelPID := w.proc.pid
	// The worker's own sandbox pid equals its host pid on macOS.
	hostKernelPID := kernelPID

	// A program under the shared profile (debug/hook shape, fork allowed) that
	// tries to signal the kernel.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	programDir := filepath.Join(root, "program")
	if err := os.Mkdir(programDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd, err := ProgramCommand(context.Background(), ProgramOptions{
		Sandbox:    SandboxOptions{},
		Executable: "/bin/sh",
		Args:       []string{"-c", fmt.Sprintf("kill -TERM %d 2>/dev/null && echo SIGNALLED || echo DENIED", hostKernelPID)},
		Workspace:  programDir,
		PrivateDir: programDir,
		Env:        []string{"PATH=/usr/bin:/bin", "HOME=" + programDir},
		CPUSeconds: 5,
		FileBytes:  1 << 20,
		Debug:      true,
	})
	if err != nil {
		t.Fatalf("ProgramCommand: %v", err)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sandboxed program: %v: %s", err, out)
	}
	if !strings.Contains(string(out), "DENIED") {
		t.Fatalf("a program under the debug profile signalled an Eval kernel: %s", out)
	}
	if !w.alive() {
		t.Fatalf("kernel %d died from a foreign signal", hostKernelPID)
	}

	// Now the kernel tries to signal a host process it does not own.
	victim := exec.Command("/bin/sleep", "60")
	if err := victim.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = victim.Wait(); close(done) }()
	t.Cleanup(func() { _ = victim.Process.Kill(); <-done })
	victimPID := victim.Process.Pid
	result := h.exec(session, "xreview-signal", fmt.Sprintf("import os, signal\ntry:\n    os.kill(%d, signal.SIGTERM)\n    print('SENT')\nexcept PermissionError:\n    print('DENIED')", victimPID), evalapi.Budget{})
	if !strings.Contains(result.Stdout, "DENIED") || strings.Contains(result.Stdout, "SENT") {
		t.Fatalf("kernel signalled a foreign process: stdout=%q stderr=%q", result.Stdout, result.Stderr)
	}
	select {
	case <-done:
		t.Fatalf("host victim %d died", victimPID)
	case <-time.After(300 * time.Millisecond):
	}
	if !w.alive() {
		t.Fatalf("kernel died during its own denied signal")
	}
	// Control: the census still holds exactly this kernel, and the program did
	// not add a row.
	if rows := censusRows("xreview-signal"); len(rows) != 1 || rows[0].Owner.Component != procrt.ComponentEval {
		t.Fatalf("census after sandbox checks = %+v", rows)
	}
}

// TestXReviewInspectDoesNotRestartADeadKernel: Inspecting a kernel whose worker
// died reports the fact. It does not start a replacement process (that is the
// next execution's job) and does not advance the generation by itself.
func TestXReviewInspectDoesNotRestartADeadKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "xreview-dead", "xreview-dead")
	before := h.expectCompleted(session, "xreview-dead", "print('alive')")
	s, err := h.m.session(session.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	dead := installedWorker(s)
	// The crash is the setup.
	crashed := h.exec(session, "xreview-dead", "import os\nos._exit(7)", evalapi.Budget{})
	if crashed.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("crash state = %q", crashed.StateStatus)
	}
	if dead == nil || dead.alive() {
		t.Fatalf("worker survived its own exit")
	}
	deadPID := dead.proc.pid
	// The loss already advanced the host generation; Inspect must report that
	// generation without changing it.
	healthBefore, err := h.m.Health(context.Background(), session.ID, "xreview-dead")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	value, err := h.m.Inspect(context.Background(), session.ID, "xreview-dead", evalapi.InspectOptions{})
	if err != nil {
		t.Fatalf("Inspect of a dead kernel: %v", err)
	}
	if value.Alive || len(value.Entries) != 0 {
		t.Fatalf("dead kernel reported alive with entries: %+v", value)
	}
	if value.Generation != healthBefore.Generation {
		t.Fatalf("Inspect disagreed with health on the host generation: %d vs %d", value.Generation, healthBefore.Generation)
	}
	healthAfter, err := h.m.Health(context.Background(), session.ID, "xreview-dead")
	if err != nil {
		t.Fatalf("Health after Inspect: %v", err)
	}
	if healthAfter.Generation != healthBefore.Generation {
		t.Fatalf("Inspect advanced the host generation: %d -> %d", healthBefore.Generation, healthAfter.Generation)
	}
	if rows := censusRows("xreview-dead"); len(rows) != 0 {
		t.Fatalf("Inspect left a census row for the dead worker: %+v", rows)
	}
	if processAlive(deadPID) {
		t.Fatalf("Inspect restarted worker %d", deadPID)
	}
	// Only execution starts the next generation.
	revived := h.expectCompleted(session, "xreview-dead", "print('again')")
	if revived.Generation <= crashed.Generation || revived.Generation <= before.Generation {
		t.Fatalf("restart did not advance the generation: before=%d crashed=%d revived=%d", before.Generation, crashed.Generation, revived.Generation)
	}
}

// TestXReviewSandboxUnavailableFailsClosed: when the configured backend or the
// profile cannot be applied, building the command fails. No call path returns a
// live command that would run the program unsandboxed.
func TestXReviewSandboxUnavailableFailsClosed(t *testing.T) {
	// A backend this platform does not have is refused by New.
	foreign := evalapi.SandboxBubblewrap
	if runtime.GOOS == "linux" {
		foreign = evalapi.SandboxSeatbelt
	}
	if _, err := newLauncher(SandboxOptions{Backend: foreign}); err == nil {
		t.Fatalf("launcher accepted the %q backend on %s", foreign, runtime.GOOS)
	}
	if _, err := newLauncher(SandboxOptions{Backend: "none"}); err == nil {
		t.Fatalf("launcher accepted a backend that applies nothing")
	}
	if runtime.GOOS == "darwin" {
		l, err := newLauncher(SandboxOptions{})
		if err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		kernel := filepath.Join(root, "kernel")
		if err := os.Mkdir(kernel, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err = l.command(launchSpec{
			exe: "/bin/sh", dir: kernel, kernelDir: kernel, workersDir: kernel,
			// The parent directory does not exist, so the profile cannot be written.
			profilePath: filepath.Join(root, "missing", "sandbox.sb"),
		})
		if err == nil {
			t.Fatalf("a profile that cannot be written produced a runnable command")
		}
		// A workspace that is not a directory is refused before any launch.
		if _, err := ProgramCommand(context.Background(), ProgramOptions{
			Executable: "/bin/sh", Workspace: filepath.Join(root, "nope"), PrivateDir: root,
		}); err == nil {
			t.Fatalf("ProgramCommand accepted a missing workspace")
		}
	}
}
