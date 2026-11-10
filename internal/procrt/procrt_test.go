package procrt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
	"time"
)

// fakeHandle records the escalation it received. exitAfter says which step
// closes Done; steps beyond it must not be called.
type fakeHandle struct {
	pid       int
	mu        sync.Mutex
	steps     []string
	exitAfter string
	done      chan struct{}
	closed    sync.Once
	survivors []int
}

func newFake(pid int, exitAfter string) *fakeHandle {
	return &fakeHandle{pid: pid, exitAfter: exitAfter, done: make(chan struct{})}
}

func (f *fakeHandle) close() { f.closed.Do(func() { close(f.done) }) }

func (f *fakeHandle) note(step string) {
	f.mu.Lock()
	f.steps = append(f.steps, step)
	f.mu.Unlock()
	if f.exitAfter == step {
		f.close()
	}
}

func (f *fakeHandle) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.steps...)
}

func (f *fakeHandle) PID() int        { return f.pid }
func (f *fakeHandle) TreePIDs() []int { return nil }
func (f *fakeHandle) State() string {
	select {
	case <-f.done:
		return StateExited
	default:
		return StateRunning
	}
}
func (f *fakeHandle) Usage() Usage { return Usage{CPUMs: 5, RSSBytes: 1 << 20} }
func (f *fakeHandle) Enforcement() Enforcement {
	return Enforcement{Memory: Supervised, CPU: Hard, Disk: Hard, MemoryPollMS: 200}
}
func (f *fakeHandle) Interrupt() error { f.note("interrupt"); return nil }
func (f *fakeHandle) Terminate(context.Context) []int {
	f.note("terminate")
	return f.survivors
}
func (f *fakeHandle) Kill() []int { f.note("kill"); return f.survivors }
func (f *fakeHandle) Done() <-chan struct{} {
	return f.done
}

func TestRegistryCensusFilteringAndUnregister(t *testing.T) {
	r := New()
	a := newFake(101, "")
	b := newFake(102, "")
	defer a.close()
	defer b.close()
	unregisterA := r.Register(Owner{SessionID: "s1", Component: ComponentEval, ObjectID: "k1"}, a)
	registerB := r.Register(Owner{SessionID: "s1", Component: ComponentDAP, ObjectID: "d1"}, b)
	defer registerB()

	if got := r.Count(); got != 2 {
		t.Fatalf("count = %d", got)
	}
	rows := r.List(Owner{SessionID: "s1"})
	if len(rows) != 2 || rows[0].PID != 101 || rows[1].PID != 102 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].Enforcement.Memory != Supervised || rows[0].Enforcement.MemoryLimitLabel() != Supervised {
		t.Fatalf("enforcement = %+v", rows[0].Enforcement)
	}
	if got := r.List(Owner{Component: ComponentDAP}); len(got) != 1 || got[0].PID != 102 {
		t.Fatalf("component filter = %+v", got)
	}
	if got := r.List(Owner{SessionID: "other"}); len(got) != 0 {
		t.Fatalf("unrelated owner listed: %+v", got)
	}
	unregisterA()
	unregisterA() // idempotent
	if got := r.Count(); got != 1 {
		t.Fatalf("count after unregister = %d", got)
	}
}

func TestCloseOwnerEscalatesInterruptThenTerminateThenKill(t *testing.T) {
	r := New()
	// The cooperative process exits on interrupt.
	cooperative := newFake(201, "interrupt")
	// The stubborn process survives interrupt and terminate; only kill ends it.
	stubborn := newFake(202, "kill")
	unregisterA := r.Register(Owner{SessionID: "run", Component: ComponentEval, ObjectID: "a"}, cooperative)
	unregisterB := r.Register(Owner{SessionID: "run", Component: ComponentEval, ObjectID: "b"}, stubborn)
	defer unregisterA()
	defer unregisterB()

	reports, err := r.CloseOwner(context.Background(), Owner{SessionID: "run"}, Ladder{
		InterruptWait: 50 * time.Millisecond,
		TerminateWait: 50 * time.Millisecond,
		KillWait:      50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("CloseOwner: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %+v", reports)
	}
	if got := cooperative.recorded(); len(got) != 1 || got[0] != "interrupt" {
		t.Fatalf("cooperative steps = %v (a cooperative stop must not escalate)", got)
	}
	if got := stubborn.recorded(); len(got) != 3 || got[0] != "interrupt" || got[1] != "terminate" || got[2] != "kill" {
		t.Fatalf("stubborn steps = %v", got)
	}
	// The census is empty after the stop: the acceptance invariant.
	if got := r.Count(); got != 0 {
		t.Fatalf("census after CloseOwner = %d", got)
	}
}

func TestCloseOwnerReportsSurvivorsAsError(t *testing.T) {
	r := New()
	// A handle that claims its tree survives every step. Kill's survivor list is
	// reported, so this test injects a pid that cannot be alive.
	h := newFake(203, "")
	// The test process is a real live pid, so the liveness check keeps it.
	h.survivors = []int{os.Getpid()}
	unregister := r.Register(Owner{SessionID: "run", Component: ComponentDAP}, h)
	defer unregister()

	reports, err := r.CloseOwner(context.Background(), Owner{SessionID: "run"}, Ladder{
		InterruptWait: 10 * time.Millisecond,
		TerminateWait: 10 * time.Millisecond,
		KillWait:      10 * time.Millisecond,
	})
	if !errors.Is(err, ErrCleanupFailed) {
		t.Fatalf("err = %v, want ErrCleanupFailed", err)
	}
	if len(reports) != 1 || len(reports[0].Survivors) != 1 {
		t.Fatalf("reports = %+v", reports)
	}
	if got := r.Count(); got != 0 {
		t.Fatalf("census after a failed stop = %d; it must still be unregistered", got)
	}
}

func TestCloseOwnerLeavesOtherOwnersAlone(t *testing.T) {
	r := New()
	mine := newFake(301, "")
	other := newFake(302, "")
	defer mine.close()
	defer other.close()
	defer r.Register(Owner{SessionID: "mine"}, mine)()
	defer r.Register(Owner{SessionID: "other"}, other)()

	if _, err := r.CloseOwner(context.Background(), Owner{SessionID: "mine"}, Ladder{
		InterruptWait: 10 * time.Millisecond, TerminateWait: 10 * time.Millisecond, KillWait: 10 * time.Millisecond,
	}); err != nil {
		t.Fatalf("CloseOwner: %v", err)
	}
	if got := other.recorded(); len(got) != 0 {
		t.Fatalf("another owner's process was signalled: %v", got)
	}
}

func TestRegisterCommandCensusAndClose(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the command handle test signals process groups")
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 30")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot start a helper process: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()

	unregister := RegisterCommand(Owner{SessionID: "dap-run", Component: ComponentDAP, ObjectID: "debug-1"}, cmd)
	rows := Default().List(Owner{SessionID: "dap-run", Component: ComponentDAP})
	if len(rows) != 1 || rows[0].PID != cmd.Process.Pid || rows[0].State != StateRunning {
		unregister()
		_ = cmd.Process.Kill()
		<-reaped
		t.Fatalf("census = %+v", rows)
	}
	if rows[0].Enforcement.Memory != None {
		// The zero label reads as none, never as a limit.
		t.Fatalf("a plain command must not claim an enforcement: %+v", rows[0].Enforcement)
	}
	if _, err := Default().CloseOwner(context.Background(), Owner{SessionID: "dap-run"}, Ladder{
		InterruptWait: 100 * time.Millisecond, TerminateWait: 100 * time.Millisecond, KillWait: 100 * time.Millisecond,
	}); err != nil {
		unregister()
		t.Fatalf("CloseOwner: %v", err)
	}
	select {
	case <-reaped:
	case <-time.After(5 * time.Second):
		t.Fatalf("the command was not stopped")
	}
	unregister()
	if got := Default().List(Owner{SessionID: "dap-run"}); len(got) != 0 {
		t.Fatalf("census not empty: %+v", got)
	}
}

func TestRingIsBounded(t *testing.T) {
	ring := NewRing(4, 4)
	for range 100 {
		_, _ = ring.Write([]byte("abcdefgh"))
	}
	snap := ring.Snapshot()
	if !snap.Truncated || snap.Total != 800 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if len(snap.Head)+len(snap.Tail) > 8 {
		t.Fatalf("retained %d bytes", len(snap.Head)+len(snap.Tail))
	}
}
