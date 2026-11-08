package evalruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

func interruptTarget(t *testing.T, h *harness, sessionID string, target evalapi.InterruptTarget) evalapi.InterruptOutcome {
	t.Helper()
	outcome, err := h.m.InterruptExecution(context.Background(), sessionID, "owner", target)
	if err != nil {
		t.Fatalf("InterruptExecution(%+v) = %v", target, err)
	}
	return outcome
}

// An interrupt that arrives after its target finished must not stop the cell
// that runs now (R3-OBS-01): it is answered target_finished and the running
// cell completes with its kernel.
func TestLateInterruptDoesNotStopTheNextCell(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	first := h.execRequest(session, "owner", evalapi.Request{ID: "first", Code: "kept = 1"}, nil)
	if !first.Succeeded() {
		t.Fatalf("first cell: %s %s", first.Status, first.Error)
	}
	done := startBusyCell(t, h, session, "second", "import time\ntime.sleep(1.5)\nkept += 1\nkept")

	outcome := interruptTarget(t, h, session.ID, evalapi.InterruptTarget{RequestID: "first"})
	if outcome.Status != evalapi.InterruptTargetFinished || outcome.ActiveRequestID != "second" {
		t.Fatalf("late interrupt outcome = %+v", outcome)
	}
	second := awaitResult(t, done)
	if !second.Succeeded() || len(second.Displays) == 0 || second.Displays[0].Text != "2" {
		t.Fatalf("the running cell was hit by the late interrupt: %s/%s %s %+v", second.Status, second.StateStatus, second.Error, second.Displays)
	}
}

// An interrupt that names a request still waiting for the kernel cancels it:
// it returns at once without running, and the running cell is untouched.
func TestInterruptCancelsAQueuedTarget(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 'heap'")
	running := startBusyCell(t, h, session, "running", "import time\ntime.sleep(3)\nkept")

	queued := make(chan evalapi.Result, 1)
	go func() {
		result, err := h.m.Execute(context.Background(), evalapi.Request{
			ID: "queued", SessionID: session.ID, AgentID: "owner", CellID: "queued",
			Code: "open('queued.ran', 'w').write('r')",
		}, nil)
		if err != nil {
			result.Error = err.Error()
		}
		queued <- result
	}()
	// The queued request is known to the session once it is admitted.
	var outcome evalapi.InterruptOutcome
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		outcome, err = h.m.InterruptExecution(context.Background(), session.ID, "owner", evalapi.InterruptTarget{RequestID: "queued"})
		if err == nil {
			break
		}
		if !errors.Is(err, evalapi.ErrNotFound) || time.Now().After(deadline) {
			t.Fatalf("interrupt of the queued request = %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if outcome.Status != evalapi.InterruptCancelledBeforeStart || outcome.ActiveRequestID != "running" {
		t.Fatalf("queued interrupt outcome = %+v", outcome)
	}
	select {
	case result := <-queued:
		if result.Status != evalapi.ResultStatusCancelled || result.StateStatus != evalapi.StateStatusOK {
			t.Fatalf("queued result = %s/%s %s", result.Status, result.StateStatus, result.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled request kept waiting for the kernel")
	}
	result := awaitResult(t, running)
	if !result.Succeeded() || len(result.Displays) == 0 || result.Displays[0].Text != "'heap'" {
		t.Fatalf("the running cell was hit: %s/%s %s %+v", result.Status, result.StateStatus, result.Error, result.Displays)
	}
	if _, err := os.Stat(filepath.Join(session.Workspace, "work", "queued.ran")); !os.IsNotExist(err) {
		t.Fatalf("the cancelled request ran: %v", err)
	}
	// A repeat is answered from the record.
	if again := interruptTarget(t, h, session.ID, evalapi.InterruptTarget{RequestID: "queued"}); again.Status != evalapi.InterruptCancelledBeforeStart {
		t.Fatalf("repeated queued interrupt = %+v", again)
	}
}

// Interrupting a running target stops exactly it; repeating the interrupt,
// also after the target ended, returns the same outcome and signals nothing.
func TestInterruptOfARunningTargetIsIdempotent(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "kept = 3")
	done := startBusyCell(t, h, session, "loop", "while True: pass")
	view, err := h.m.Session(context.Background(), session.ID, "owner")
	if err != nil || view.ActiveRequestID != "loop" {
		t.Fatalf("session view = %+v, %v", view, err)
	}
	outcome := interruptTarget(t, h, session.ID, evalapi.InterruptTarget{RequestID: "loop", Generation: view.Generation})
	if outcome.Status != evalapi.InterruptInterrupted || outcome.ResultStatus != evalapi.ResultStatusCancelled || outcome.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("interrupt outcome = %+v", outcome)
	}
	result := awaitResult(t, done)
	if result.Status != evalapi.ResultStatusCancelled || result.StateStatus != evalapi.StateStatusOK {
		t.Fatalf("interrupted cell = %s/%s %s", result.Status, result.StateStatus, result.Error)
	}
	if again := interruptTarget(t, h, session.ID, evalapi.InterruptTarget{RequestID: "loop"}); again != outcome {
		t.Fatalf("repeated interrupt = %+v, want %+v", again, outcome)
	}
	kept := h.expectCompleted(session, "owner", "kept")
	if len(kept.Displays) == 0 || kept.Displays[0].Text != "3" {
		t.Fatalf("kernel state after the interrupt: %+v", kept.Displays)
	}
}

// A target running in another generation than the caller saw is refused, and
// an unknown target never reaches the running cell.
func TestInterruptRefusesAMismatchedOrUnknownTarget(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	if idle := interruptTarget(t, h, session.ID, evalapi.InterruptTarget{RequestID: "nothing"}); idle.Status != evalapi.InterruptNoActiveExecution {
		t.Fatalf("idle interrupt = %+v", idle)
	}
	done := startBusyCell(t, h, session, "sleeper", "import time\ntime.sleep(1.5)\n'slept'")
	view, err := h.m.Session(context.Background(), session.ID, "owner")
	if err != nil {
		t.Fatal(err)
	}
	mismatch := interruptTarget(t, h, session.ID, evalapi.InterruptTarget{RequestID: "sleeper", Generation: view.Generation + 5})
	if mismatch.Status != evalapi.InterruptGenerationMismatch || mismatch.Generation != view.Generation {
		t.Fatalf("mismatched interrupt = %+v", mismatch)
	}
	if _, err := h.m.InterruptExecution(context.Background(), session.ID, "owner", evalapi.InterruptTarget{RequestID: "never-sent"}); !errors.Is(err, evalapi.ErrNotFound) {
		t.Fatalf("unknown target while a cell runs = %v", err)
	}
	if _, err := h.m.InterruptExecution(context.Background(), session.ID, "owner", evalapi.InterruptTarget{}); !errors.Is(err, evalapi.ErrInvalid) {
		t.Fatalf("interrupt without a target = %v", err)
	}
	result := awaitResult(t, done)
	if !result.Succeeded() || len(result.Displays) == 0 || result.Displays[0].Text != "'slept'" {
		t.Fatalf("a refused interrupt reached the cell: %s/%s %s %+v", result.Status, result.StateStatus, result.Error, result.Displays)
	}
}
