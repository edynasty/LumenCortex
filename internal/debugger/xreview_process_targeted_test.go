//go:build unix

package debugger

// Cross-review (B2 process/eval/DAP) regressions with a real adapter:
//
//  2. Ending one debug session - its adapter crashing, or CloseOwner - must
//     leave another session's adapter and debuggee running, and must not touch
//     a different session's census row.
//  3/4. The census holds exactly the live adapter, drops the crashed one, and
//     is empty after CloseOwner.
//  PED-3: a host-chosen end (CloseOwner) is not reported as
//     debug.session.lost; only the adapter crash is.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/procrt"
	"syscall"
)

type xreviewEvents struct {
	mu   sync.Mutex
	rows []string
}

func (e *xreviewEvents) add(sessionID, event string) {
	e.mu.Lock()
	e.rows = append(e.rows, sessionID+":"+event)
	e.mu.Unlock()
}

func (e *xreviewEvents) count(sessionID, event string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, row := range e.rows {
		if row == sessionID+":"+event {
			n++
		}
	}
	return n
}

// xreviewStartStopped starts a real lldb session stopped at line 3 of program.
func xreviewStartStopped(t *testing.T, ctx context.Context, m *Manager, sessionID, root, program, source string) Snapshot {
	t.Helper()
	snap, err := m.Start(ctx, StartOptions{SessionID: sessionID, AdapterID: "lldb", Workspace: root, Program: program, Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	c := Control{SessionID: sessionID, DebugID: snap.ID, Epoch: snap.Epoch, Actor: "agent"}
	if _, err := m.Breakpoints(ctx, c, source, []Breakpoint{{Line: 3}}); err != nil {
		t.Fatalf("breakpoints: %v", err)
	}
	c.Action = "configure"
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stopped, stop, err := m.ControlAndWait(waitCtx, c, WaitOptions{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if stopped.State != "stopped" || stop.State != "stopped" || len(stop.TopFrames) == 0 {
		t.Fatalf("session %s did not stop: %+v %+v", sessionID, stopped, stop)
	}
	return snap
}

func TestXReviewRealAdapterIsolationAcrossSessions(t *testing.T) {
	if os.Getenv("LC_TEST_LLDB") == "" {
		t.Skip("set LC_TEST_LLDB for the real adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	adapter := testLLDB(t)
	root := t.TempDir()
	source := filepath.Join(root, "target.c")
	program := filepath.Join(root, "target")
	if err := os.WriteFile(source, []byte("#include <unistd.h>\nint main(void) {\n int value=41;\n value++;\n sleep(30);\n return value==42?0:1;\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "cc", "-g", "-O0", source, "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile %v %s", err, out)
	}
	events := &xreviewEvents{}
	m, err := New(Options{
		Sandbox:  testSandbox(t),
		Adapters: []Adapter{{ID: "lldb", Command: adapter, ReadPaths: xcodeReadPaths(adapter), Launch: map[string]any{"disableASLR": false}}},
		OnEvent:  func(sessionID, event string, _ map[string]any) { events.add(sessionID, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	first := xreviewStartStopped(t, ctx, m, "xreview-a", root, program, source)
	second := xreviewStartStopped(t, ctx, m, "xreview-b", root, program, source)
	if first.AdapterPID <= 1 || second.AdapterPID <= 1 || first.AdapterPID == second.AdapterPID {
		t.Fatalf("adapter pids a=%d b=%d", first.AdapterPID, second.AdapterPID)
	}
	rows := procrt.Default().List(procrt.Owner{Component: procrt.ComponentDAP})
	if len(rows) != 2 {
		t.Fatalf("census = %+v", rows)
	}
	rowPID := map[string]int{}
	for _, row := range rows {
		rowPID[row.Owner.SessionID] = row.PID
	}
	if rowPID["xreview-a"] != first.AdapterPID || rowPID["xreview-b"] != second.AdapterPID {
		t.Fatalf("census rows %+v do not match adapters %d/%d", rowPID, first.AdapterPID, second.AdapterPID)
	}

	// Crash session A's adapter: only A is lost.
	proc, err := os.FindProcess(first.AdapterPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.List("xreview-a")) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if live := m.List("xreview-a"); len(live) != 0 {
		t.Fatalf("crashed session still live: %+v", live)
	}
	if events.count("xreview-a", "debug.session.lost") != 1 {
		t.Fatalf("crashed session lost events = %d", events.count("xreview-a", "debug.session.lost"))
	}
	if events.count("xreview-b", "debug.session.lost") != 0 {
		t.Fatalf("healthy session reported lost: %v", events.rows)
	}
	// Session B's adapter is alive, its control channel still answers, and its
	// census row was not removed by A's teardown.
	if !processAlive(second.AdapterPID) {
		t.Fatalf("crash of A killed B's adapter %d", second.AdapterPID)
	}
	b := Control{SessionID: "xreview-b", DebugID: second.ID, Epoch: second.Epoch, Actor: "agent"}
	snap, err := m.Snapshot("xreview-b", second.ID)
	if err != nil {
		t.Fatalf("B snapshot after A's crash: %v", err)
	}
	if snap.State != "stopped" {
		t.Fatalf("B state after A's crash = %q", snap.State)
	}
	stack, err := m.Inspect(ctx, b, "stackTrace", snap.ThreadID, 0, 4)
	if err != nil || len(stack) == 0 {
		t.Fatalf("B inspect after A's crash: %v %s", err, stack)
	}
	if rows := procrt.Default().List(procrt.Owner{SessionID: "xreview-b", Component: procrt.ComponentDAP}); len(rows) != 1 || rows[0].PID != second.AdapterPID {
		t.Fatalf("B census after A's crash = %+v", rows)
	}

	// A host close of B is graceful: no lost event, census empty, adapter gone.
	if err := m.CloseOwner("xreview-b"); err != nil {
		t.Fatalf("CloseOwner B: %v", err)
	}
	if events.count("xreview-b", "debug.session.lost") != 0 {
		t.Fatalf("host close reported lost: %v", events.rows)
	}
	if _, err := m.Snapshot("xreview-b", second.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("closed session still resolvable: %v", err)
	}
	waitDeadline := time.Now().Add(10 * time.Second)
	for processAlive(second.AdapterPID) && time.Now().Before(waitDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processAlive(second.AdapterPID) {
		t.Fatalf("B adapter %d survived CloseOwner", second.AdapterPID)
	}
	if rows := procrt.Default().List(procrt.Owner{Component: procrt.ComponentDAP}); len(rows) != 0 {
		t.Fatalf("census after closes = %+v", rows)
	}
	if events.count("xreview-a", "debug.session.lost") != 1 {
		t.Fatalf("A lost event changed: %v", events.rows)
	}
	fmt.Fprintf(os.Stderr, "XREVIEW_DAP_ISOLATION: a=%d b=%d\n", first.AdapterPID, second.AdapterPID)
}

// processAlive reports whether pid is still a live process on this host.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
