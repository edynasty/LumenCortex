package evalruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

// boundaryRounds is the number of rounds of the schedule tests;
// LC_BOUNDARY_ROUNDS raises it for stress runs.
func boundaryRounds(fallback int) int {
	if value, err := strconv.Atoi(os.Getenv("LC_BOUNDARY_ROUNDS")); err == nil && value > 0 {
		return value
	}
	return fallback
}

// shortInterruptGrace shortens the interrupt grace period for one test.
func shortInterruptGrace(t *testing.T, grace, repeat time.Duration) {
	t.Helper()
	oldGrace, oldRepeat := interruptGrace, interruptRepeat
	interruptGrace, interruptRepeat = grace, repeat
	t.Cleanup(func() { interruptGrace, interruptRepeat = oldGrace, oldRepeat })
}

// escalationGate holds an interrupt escalation at one named point until the
// test releases it.
type escalationGate struct {
	reached chan struct{}
	release chan struct{}
}

func holdEscalationAt(t *testing.T, point string) *escalationGate {
	t.Helper()
	gate := &escalationGate{reached: make(chan struct{}), release: make(chan struct{})}
	escalationHook = func(at string) {
		if at == point {
			close(gate.reached)
			<-gate.release
		}
	}
	t.Cleanup(func() { escalationHook = nil })
	return gate
}

func (g *escalationGate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the escalation never reached its hold point")
	}
}

// startIgnoringCell runs a cell that ignores SIGINT and runs until the test
// creates <id>.finish. It returns once SIGINT is ignored.
func startIgnoringCell(t *testing.T, h *harness, session evalapi.Session, id string) <-chan evalapi.Result {
	t.Helper()
	done := make(chan evalapi.Result, 1)
	code := fmt.Sprintf("import signal, os, time\nsignal.signal(signal.SIGINT, signal.SIG_IGN)\nwith open(%q, 'w') as f: f.write('r')\nwhile not os.path.exists(%q): time.sleep(0.001)\n%q", id+".ignoring", id+".finish", id+" finished")
	go func() {
		result, err := h.m.Execute(context.Background(), evalapi.Request{ID: id, SessionID: session.ID, AgentID: "owner", CellID: id, Code: code}, nil)
		if err != nil {
			result.Error = err.Error()
		}
		done <- result
	}()
	waitForFile(t, filepath.Join(session.Workspace, "work", id+".ignoring"), 10*time.Second)
	return done
}

func finishCell(t *testing.T, session evalapi.Session, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(session.Workspace, "work", id+".finish"), []byte("r"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func interruptAsync(h *harness, sessionID string) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- interruptActive(h, sessionID) }()
	return errc
}

func awaitErr(t *testing.T, errc <-chan error) error {
	t.Helper()
	select {
	case err := <-errc:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Interrupt did not return")
		return nil
	}
}

// An escalation that reaches its kill only after the cell completed on its own
// must not touch the worker: the next cell, already running on the same
// worker and heap, completes, and Interrupt reports success without a kill.
// This is the schedule of R2-SOL-P1-01, held deterministically at the claim.
func TestEscalationAfterTheCellCompletedSparesTheNextCell(t *testing.T) {
	shortInterruptGrace(t, 200*time.Millisecond, 50*time.Millisecond)
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 0")
	proc := workerProc(t, h, session.ID)
	for round := 1; round <= boundaryRounds(5); round++ {
		gate := holdEscalationAt(t, "claim")
		id := fmt.Sprintf("old%d", round)
		old := startIgnoringCell(t, h, session, id)
		interrupted := interruptAsync(h, session.ID)
		gate.await(t)
		finishCell(t, session, id)
		first := awaitResult(t, old)
		if first.Status != evalapi.ResultStatusCompleted || first.StateStatus != evalapi.StateStatusOK {
			t.Fatalf("round %d: old cell %s/%s %s", round, first.Status, first.StateStatus, first.Error)
		}
		next := startBusyCell(t, h, session, fmt.Sprintf("next%d", round), "import time\ntime.sleep(0.2)\nkept += 1\nkept")
		close(gate.release)
		if err := awaitErr(t, interrupted); err != nil {
			t.Fatalf("round %d: Interrupt = %v", round, err)
		}
		second := awaitResult(t, next)
		if !second.Succeeded() || len(second.Displays) == 0 || second.Displays[0].Text != strconv.Itoa(round) {
			t.Fatalf("round %d: the old escalation reached the next cell: %s/%s %s %+v", round, second.Status, second.StateStatus, second.Error, second.Displays)
		}
		if workerProc(t, h, session.ID) != proc || second.Generation != first.Generation {
			t.Fatalf("round %d: the worker was replaced", round)
		}
		escalationHook = nil
	}
}

// An escalation that claimed the kill before the cell's outcome was settled
// wins: the old cell is reported cancelled with its kernel lost, and the next
// cell is held back until the kill has returned, then runs on a new worker.
func TestEscalationClaimedFirstHoldsTheNextCellUntilTheKillReturns(t *testing.T) {
	shortInterruptGrace(t, 200*time.Millisecond, 50*time.Millisecond)
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	for round := 1; round <= boundaryRounds(5); round++ {
		h.expectCompleted(session, "owner", "kept = 'heap'")
		proc := workerProc(t, h, session.ID)
		gate := holdEscalationAt(t, "kill")
		id := fmt.Sprintf("old%d", round)
		old := startIgnoringCell(t, h, session, id)
		interrupted := interruptAsync(h, session.ID)
		gate.await(t)
		finishCell(t, session, id)
		nextID := fmt.Sprintf("next%d", round)
		next := make(chan evalapi.Result, 1)
		go func() {
			result, err := h.m.Execute(context.Background(), evalapi.Request{ID: nextID, SessionID: session.ID, AgentID: "owner", CellID: nextID,
				Code: fmt.Sprintf("open(%q, 'w').write('r')\n'fresh' if 'kept' not in globals() else kept", nextID+".ran")}, nil)
			if err != nil {
				result.Error = err.Error()
			}
			next <- result
		}()
		// The kill is claimed but not done: neither the old result nor the next
		// cell may get past it.
		time.Sleep(300 * time.Millisecond)
		select {
		case r := <-old:
			t.Fatalf("round %d: the old cell settled before the claimed kill returned: %+v", round, r)
		case r := <-next:
			t.Fatalf("round %d: the next cell ran before the claimed kill returned: %+v", round, r)
		default:
		}
		if _, err := os.Stat(filepath.Join(session.Workspace, "work", nextID+".ran")); err == nil {
			t.Fatalf("round %d: the next cell's code ran on the doomed worker", round)
		}
		close(gate.release)
		if err := awaitErr(t, interrupted); err != nil {
			t.Fatalf("round %d: Interrupt = %v", round, err)
		}
		first := awaitResult(t, old)
		if first.Status != evalapi.ResultStatusCancelled || first.StateStatus != evalapi.StateStatusLost {
			t.Fatalf("round %d: old cell %s/%s, want cancelled/lost", round, first.Status, first.StateStatus)
		}
		second := awaitResult(t, next)
		if !second.Succeeded() || len(second.Displays) == 0 || second.Displays[0].Text != "'fresh'" {
			t.Fatalf("round %d: next cell %s/%s %s %+v", round, second.Status, second.StateStatus, second.Error, second.Displays)
		}
		if second.Generation <= first.Generation || workerProc(t, h, session.ID) == proc {
			t.Fatalf("round %d: the next cell did not get a new worker (generation %d -> %d)", round, first.Generation, second.Generation)
		}
		escalationHook = nil
	}
}

// A cell that ignores the interrupt and ends on its own close to the end of the
// grace period: whichever side wins, the reported state matches what happened
// to the worker, and the next cell is never killed.
func TestInterruptAtTheGraceDeadlineIsConsistent(t *testing.T) {
	grace := 150 * time.Millisecond
	shortInterruptGrace(t, grace, 40*time.Millisecond)
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	kept, lost := 0, 0
	for round := 1; round <= boundaryRounds(20); round++ {
		mark := fmt.Sprintf("'r%d'", round)
		h.expectCompleted(session, "owner", "kept = "+mark)
		proc := workerProc(t, h, session.ID)
		// The cell ends on its own just before, at or just after the deadline.
		delay := grace - 20*time.Millisecond + time.Duration(round%9)*5*time.Millisecond
		id := fmt.Sprintf("edge%d", round)
		code := fmt.Sprintf("import signal, time\nsignal.signal(signal.SIGINT, signal.SIG_IGN)\nopen(%q, 'w').write('r')\ntime.sleep(%f)\n'done'", id+".ready", delay.Seconds())
		old := make(chan evalapi.Result, 1)
		go func() {
			result, err := h.m.Execute(context.Background(), evalapi.Request{ID: id, SessionID: session.ID, AgentID: "owner", CellID: id, Code: code}, nil)
			if err != nil {
				result.Error = err.Error()
			}
			old <- result
		}()
		waitForFile(t, filepath.Join(session.Workspace, "work", id+".ready"), 10*time.Second)
		interrupted := interruptAsync(h, session.ID)
		first := awaitResult(t, old)
		next := h.exec(session, "owner", "globals().get('kept', 'fresh')", evalapi.Budget{})
		if err := awaitErr(t, interrupted); err != nil {
			t.Fatalf("round %d: Interrupt = %v", round, err)
		}
		if !next.Succeeded() || len(next.Displays) == 0 {
			t.Fatalf("round %d: the next cell was hit: %s/%s %s", round, next.Status, next.StateStatus, next.Error)
		}
		switch {
		case first.Status == evalapi.ResultStatusCompleted && first.StateStatus == evalapi.StateStatusOK:
			kept++
			if next.Displays[0].Text != mark || workerProc(t, h, session.ID) != proc {
				t.Fatalf("round %d: old completed/ok but the next cell saw %s on a different worker", round, next.Displays[0].Text)
			}
		case first.Status == evalapi.ResultStatusCancelled && first.StateStatus == evalapi.StateStatusLost:
			lost++
			if next.Displays[0].Text != "'fresh'" || workerProc(t, h, session.ID) == proc {
				t.Fatalf("round %d: old lost but the next cell saw %s", round, next.Displays[0].Text)
			}
		default:
			t.Fatalf("round %d: old cell %s/%s %s", round, first.Status, first.StateStatus, first.Error)
		}
	}
	t.Logf("grace edge: kept=%d lost=%d", kept, lost)
}

// Reset, Close and Dispose that race an escalation holding its claimed kill
// finish, leave no worker, and never hang.
func TestEscalationRacesLifecycleOperations(t *testing.T) {
	shortInterruptGrace(t, 200*time.Millisecond, 50*time.Millisecond)
	for _, op := range []string{"reset", "close", "dispose"} {
		for _, point := range []string{"claim", "kill"} {
			t.Run(op+"/"+point, func(t *testing.T) {
				h, _ := newPythonHarness(t, nil)
				session := h.create("python", "owner", "owner")
				oldProc := workerProc(t, h, session.ID)
				gate := holdEscalationAt(t, point)
				old := startIgnoringCell(t, h, session, "old")
				interrupted := interruptAsync(h, session.ID)
				gate.await(t)
				lifecycle := make(chan error, 1)
				go func() {
					switch op {
					case "reset":
						lifecycle <- h.m.Reset(context.Background(), session.ID, "owner")
					case "close":
						lifecycle <- h.m.Close(context.Background(), session.ID, "owner")
					case "dispose":
						lifecycle <- h.m.Dispose(context.Background())
					}
				}()
				time.Sleep(50 * time.Millisecond)
				close(gate.release)
				if err := awaitErr(t, lifecycle); err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				_ = awaitErr(t, interrupted)
				first := awaitResult(t, old)
				if first.StateStatus != evalapi.StateStatusLost {
					t.Fatalf("old cell %s/%s", first.Status, first.StateStatus)
				}
				select {
				case <-oldProc.waitCh:
				case <-time.After(10 * time.Second):
					t.Fatal("the old worker still runs")
				}
				if op == "reset" {
					h.expectCompleted(session, "owner", "1 + 1")
				}
			})
		}
	}
}

// A cell that blocks SIGINT and completes before the interrupt escalates leaves
// that SIGINT pending. It belongs to the finished cell: the next cell, even one
// that unblocks SIGINT itself, must not receive it, and the kernel keeps its
// state (R2-SOL-P2-02).
func TestPendingInterruptDoesNotCrossTheCellBoundary(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 0")
	proc := workerProc(t, h, session.ID)
	for round := 1; round <= boundaryRounds(10); round++ {
		id := fmt.Sprintf("masked%d", round)
		code := fmt.Sprintf("import signal, time\nsignal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT})\nopen(%q, 'w').write('r')\ntime.sleep(0.25)\nsignal.SIGINT in signal.sigpending()", id+".ready")
		old := make(chan evalapi.Result, 1)
		go func() {
			result, err := h.m.Execute(context.Background(), evalapi.Request{ID: id, SessionID: session.ID, AgentID: "owner", CellID: id, Code: code}, nil)
			if err != nil {
				result.Error = err.Error()
			}
			old <- result
		}()
		waitForFile(t, filepath.Join(session.Workspace, "work", id+".ready"), 10*time.Second)
		if err := interruptRequest(h, session.ID, id); err != nil {
			t.Fatalf("round %d: Interrupt = %v", round, err)
		}
		first := awaitResult(t, old)
		if first.Status != evalapi.ResultStatusCompleted || len(first.Displays) == 0 || first.Displays[0].Text != "True" {
			t.Fatalf("round %d: the masked cell did not complete with SIGINT pending: %s/%s %+v", round, first.Status, first.StateStatus, first.Displays)
		}
		next := h.exec(session, "owner", "import signal\nmask = signal.pthread_sigmask(signal.SIG_UNBLOCK, {signal.SIGINT})\nkept += 1\n(signal.SIGINT in mask, signal.getsignal(signal.SIGINT) not in (signal.SIG_IGN, signal.SIG_DFL), kept)", evalapi.Budget{})
		want := fmt.Sprintf("(False, True, %d)", round)
		if !next.Succeeded() || len(next.Displays) == 0 || next.Displays[0].Text != want {
			t.Fatalf("round %d: next cell %s/%s %s %+v, want %s", round, next.Status, next.StateStatus, next.Error, next.Displays, want)
		}
		if workerProc(t, h, session.ID) != proc {
			t.Fatalf("round %d: the worker was replaced", round)
		}
	}
}

// R3-SOL-P2-02: the user code that renders a cell's result runs inside the
// cell's signal window. A __repr__ that blocks SIGINT and receives the
// interrupt leaves it pending; it is discarded when that last user code
// returns, so the next cell, which unblocks SIGINT, is not interrupted and the
// kernel keeps its state.
func TestPendingInterruptFromReprDoesNotCrossTheCellBoundary(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 0")
	proc := workerProc(t, h, session.ID)
	for round := 1; round <= boundaryRounds(5); round++ {
		id := fmt.Sprintf("repr%d", round)
		code := fmt.Sprintf("import signal, time\nclass Masked:\n    def __repr__(self):\n        signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT})\n        open(%q, 'w').write('r')\n        time.sleep(0.4)\n        return repr(signal.SIGINT in signal.sigpending())\nMasked()", id+".ready")
		old := make(chan evalapi.Result, 1)
		go func() {
			result, err := h.m.Execute(context.Background(), evalapi.Request{ID: id, SessionID: session.ID, AgentID: "owner", CellID: id, Code: code}, nil)
			if err != nil {
				result.Error = err.Error()
			}
			old <- result
		}()
		waitForFile(t, filepath.Join(session.Workspace, "work", id+".ready"), 10*time.Second)
		if err := interruptRequest(h, session.ID, id); err != nil {
			t.Fatalf("round %d: Interrupt = %v", round, err)
		}
		first := awaitResult(t, old)
		if first.Status != evalapi.ResultStatusCompleted || len(first.Displays) == 0 || first.Displays[0].Text != "True" {
			t.Fatalf("round %d: the repr did not complete with SIGINT pending: %s/%s %s %+v", round, first.Status, first.StateStatus, first.Error, first.Displays)
		}
		next := h.exec(session, "owner", "import signal\nmask = signal.pthread_sigmask(signal.SIG_UNBLOCK, {signal.SIGINT})\nkept += 1\n(signal.SIGINT in mask, signal.getsignal(signal.SIGINT) not in (signal.SIG_IGN, signal.SIG_DFL), kept)", evalapi.Budget{})
		want := fmt.Sprintf("(False, True, %d)", round)
		if !next.Succeeded() || len(next.Displays) == 0 || next.Displays[0].Text != want {
			t.Fatalf("round %d: next cell %s/%s %s %+v, want %s", round, next.Status, next.StateStatus, next.Error, next.Displays, want)
		}
		if workerProc(t, h, session.ID) != proc {
			t.Fatalf("round %d: the worker was replaced", round)
		}
	}
}

// A cell that replaces the SIGINT handler does not take the interrupt away from
// the cells after it: the next cell is interrupted cooperatively and the kernel
// keeps its state.
func TestCellSignalHandlerDoesNotOutliveTheCell(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "import signal\nsignal.signal(signal.SIGINT, signal.SIG_IGN)\nkept = 'heap'")
	done := startBusyCell(t, h, session, "loop", "while True: pass")
	if err := interruptActive(h, session.ID); err != nil {
		t.Fatalf("Interrupt = %v", err)
	}
	result := awaitResult(t, done)
	if result.Status != evalapi.ResultStatusCancelled || result.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("interrupt after a SIG_IGN cell: %s/%s %s", result.Status, result.StateStatus, result.Error)
	}
	kept := h.expectCompleted(session, "owner", "kept")
	if len(kept.Displays) == 0 || kept.Displays[0].Text != "'heap'" {
		t.Fatalf("kernel state lost: %+v", kept.Displays)
	}
}
