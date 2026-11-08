package evalruntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

// startBusyCell runs code in the session after a marker file is written, and
// returns once the cell's code is running. The result arrives on the channel.
func startBusyCell(t *testing.T, h *harness, session evalapi.Session, id, code string) <-chan evalapi.Result {
	t.Helper()
	marker := filepath.Join(session.Workspace, "work", id+".ready")
	_ = os.Remove(marker)
	done := make(chan evalapi.Result, 1)
	go func() {
		result, err := h.m.Execute(context.Background(), evalapi.Request{
			ID: id, SessionID: session.ID, AgentID: "owner", CellID: id,
			Code: fmt.Sprintf("open(%q, 'w').write('r')\n%s", id+".ready", code),
		}, nil)
		if err != nil {
			result.Error = err.Error()
		}
		done <- result
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return done
		}
		select {
		case result := <-done:
			t.Fatalf("cell %s ended before it was ready: %+v", id, result)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("cell %s never became ready", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// interruptConcurrently issues n Interrupt calls at once and returns their errors.
func interruptConcurrently(h *harness, sessionID string, n int) []error {
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = h.m.Interrupt(context.Background(), sessionID, "owner")
		}()
	}
	close(start)
	wg.Wait()
	return errs
}

func awaitResult(t *testing.T, done <-chan evalapi.Result) evalapi.Result {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(30 * time.Second):
		t.Fatal("interrupted cell did not return")
		return evalapi.Result{}
	}
}

// expectRetained checks that an interrupted cell was cancelled cooperatively
// and the same worker process still holds the kernel with its variables.
func expectRetained(t *testing.T, h *harness, session evalapi.Session, result evalapi.Result, proc *process, want string) {
	t.Helper()
	if result.Status != evalapi.ResultStatusCancelled || result.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("interrupted cell: status=%q state=%q error=%q", result.Status, result.StateStatus, result.Error)
	}
	next := h.exec(session, "owner", "kept", evalapi.Budget{})
	if !next.Succeeded() || len(next.Displays) == 0 || next.Displays[0].Text != want {
		t.Fatalf("kernel lost after the interrupt: %+v", next)
	}
	s, err := h.m.session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := installedWorker(s); w == nil || w.proc != proc {
		t.Fatal("the worker was replaced by a cooperative interrupt")
	}
}

func workerProc(t *testing.T, h *harness, sessionID string) *process {
	t.Helper()
	s, err := h.m.session(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	w := installedWorker(s)
	if w == nil {
		t.Fatal("no worker")
	}
	return w.proc
}

// Any number of concurrent Interrupts of one execution are one interrupt: every
// caller succeeds, the cell is cancelled once, and the kernel survives.
func TestConcurrentInterruptsRetainTheKernel(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8, 16, 32} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			h, _ := newPythonHarness(t, nil)
			session := h.create("python", "owner", "owner")
			h.expectCompleted(session, "owner", "kept = 123")
			proc := workerProc(t, h, session.ID)
			done := startBusyCell(t, h, session, "busy", "while True: pass")
			for i, err := range interruptConcurrently(h, session.ID, n) {
				if err != nil {
					t.Fatalf("Interrupt %d: %v", i, err)
				}
			}
			expectRetained(t, h, session, awaitResult(t, done), proc, "123")
		})
	}
}

// Rounds of concurrent interrupts on one kernel: each round's cell is cancelled,
// the kernel and its state carry across all rounds, and an interrupt never
// reaches the cell that follows it. LC_INTERRUPT_ROUNDS raises the round count
// for stress runs.
func TestRepeatedConcurrentInterruptsRetainTheKernel(t *testing.T) {
	rounds := 10
	if value, err := strconv.Atoi(os.Getenv("LC_INTERRUPT_ROUNDS")); err == nil && value > 0 {
		rounds = value
	}
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 0")
	proc := workerProc(t, h, session.ID)
	for round := 1; round <= rounds; round++ {
		code := "while True: pass"
		if round%2 == 0 {
			code = "import time\ntime.sleep(60)" // blocked in a system call
		}
		done := startBusyCell(t, h, session, fmt.Sprintf("round%d", round), code)
		for i, err := range interruptConcurrently(h, session.ID, 8) {
			if err != nil {
				t.Fatalf("round %d: Interrupt %d: %v", round, i, err)
			}
		}
		result := awaitResult(t, done)
		if result.Status != evalapi.ResultStatusCancelled || result.StateStatus != evalapi.StateStatusOK {
			t.Fatalf("round %d: status=%q state=%q error=%q", round, result.Status, result.StateStatus, result.Error)
		}
		// The next cell runs right after the interrupts returned and must not
		// see any of them.
		next := h.exec(session, "owner", "import time\ntime.sleep(0.05)\nkept += 1\nkept", evalapi.Budget{})
		if !next.Succeeded() || len(next.Displays) == 0 || next.Displays[0].Text != strconv.Itoa(round) {
			t.Fatalf("round %d: next cell %+v", round, next)
		}
	}
	if workerProc(t, h, session.ID) != proc {
		t.Fatal("the worker was replaced during cooperative interrupts")
	}
}

// Interrupting one session leaves another session's running cell alone.
func TestConcurrentInterruptsDoNotReachOtherSessions(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	a := h.create("python", "owner", "owner")
	b := h.create("python", "owner", "owner")
	h.expectCompleted(a, "owner", "kept = 'a'")
	procB := workerProc(t, h, b.ID)
	doneB := startBusyCell(t, h, b, "other", "import time\ntime.sleep(1)\n'b finished'")
	doneA := startBusyCell(t, h, a, "target", "while True: pass")
	for i, err := range interruptConcurrently(h, a.ID, 8) {
		if err != nil {
			t.Fatalf("Interrupt %d: %v", i, err)
		}
	}
	expectRetained(t, h, a, awaitResult(t, doneA), workerProc(t, h, a.ID), "'a'")
	resultB := awaitResult(t, doneB)
	if !resultB.Succeeded() || len(resultB.Displays) == 0 || resultB.Displays[0].Text != "'b finished'" {
		t.Fatalf("the other session's cell was disturbed: %+v", resultB)
	}
	if workerProc(t, h, b.ID) != procB {
		t.Fatal("the other session's worker was replaced")
	}
}

// A cooperative interrupt in a later generation (after Reset) works the same.
func TestConcurrentInterruptsAcrossGenerations(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	for generation := 1; generation <= 3; generation++ {
		if generation > 1 {
			if err := h.m.Reset(context.Background(), session.ID, "owner"); err != nil {
				t.Fatalf("Reset: %v", err)
			}
		}
		h.expectCompleted(session, "owner", fmt.Sprintf("kept = %d", generation))
		proc := workerProc(t, h, session.ID)
		done := startBusyCell(t, h, session, fmt.Sprintf("gen%d", generation), "while True: pass")
		for i, err := range interruptConcurrently(h, session.ID, 8) {
			if err != nil {
				t.Fatalf("generation %d: Interrupt %d: %v", generation, i, err)
			}
		}
		expectRetained(t, h, session, awaitResult(t, done), proc, strconv.Itoa(generation))
	}
}

// Interrupts racing a Reset, Close or Dispose of the same session end without a
// hang or error other than the closed session, and leave no worker behind.
func TestConcurrentInterruptsRaceLifecycle(t *testing.T) {
	for _, op := range []string{"reset", "close", "dispose"} {
		t.Run(op, func(t *testing.T) {
			h, _ := newPythonHarness(t, nil)
			session := h.create("python", "owner", "owner")
			old := workerProc(t, h, session.ID)
			done := startBusyCell(t, h, session, "busy", "while True: pass")
			var wg sync.WaitGroup
			var lifecycleErr error
			wg.Add(1)
			go func() {
				defer wg.Done()
				switch op {
				case "reset":
					lifecycleErr = h.m.Reset(context.Background(), session.ID, "owner")
				case "close":
					lifecycleErr = h.m.Close(context.Background(), session.ID, "owner")
				case "dispose":
					lifecycleErr = h.m.Dispose(context.Background())
				}
			}()
			interruptConcurrently(h, session.ID, 8)
			wg.Wait()
			if lifecycleErr != nil {
				t.Fatalf("%s: %v", op, lifecycleErr)
			}
			awaitResult(t, done)
			select {
			case <-old.waitCh:
			case <-time.After(10 * time.Second):
				t.Fatal("the old worker still runs")
			}
			if op == "reset" {
				h.expectCompleted(session, "owner", "1 + 1")
			}
		})
	}
}

// A cell that swallows the first KeyboardInterrupt (as a finalizer running when
// the signal lands does) is interrupted again while the grace period lasts: it
// is cancelled cooperatively and the kernel survives.
func TestInterruptRepeatsWhenTheCellSwallowsIt(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 'k'\ncaught = 0")
	proc := workerProc(t, h, session.ID)
	done := startBusyCell(t, h, session, "swallow", "try:\n    with open('armed', 'w') as f: f.write('r')\n    while True: pass\nexcept KeyboardInterrupt:\n    caught += 1\nwhile True: pass")
	// Interrupt only once the cell is inside its try, so the first signal is
	// the one it swallows.
	armed := filepath.Join(session.Workspace, "work", "armed")
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(2 * time.Millisecond) {
		if _, err := os.Stat(armed); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the cell never entered its try")
		}
	}
	time.Sleep(20 * time.Millisecond) // let the with block close the file
	for i, err := range interruptConcurrently(h, session.ID, 8) {
		if err != nil {
			t.Fatalf("Interrupt %d: %v", i, err)
		}
	}
	expectRetained(t, h, session, awaitResult(t, done), proc, "'k'")
	caught := h.expectCompleted(session, "owner", "caught")
	if len(caught.Displays) == 0 || caught.Displays[0].Text != "1" {
		t.Fatalf("the cell saw %+v interrupts before the repeat stopped it", caught.Displays)
	}
}
