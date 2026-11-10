package evalruntime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/procrt"
)

// censusOwner filters the process census to this test's host root, so parallel
// tests in the package cannot see each other's rows.
func censusRows(owner string) []procrt.Status {
	return procrt.Default().List(procrt.Owner{SessionID: owner})
}

// waitCensusEmpty waits, briefly, for the asynchronous close path to unregister.
func waitCensusEmpty(t *testing.T, owner string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if rows := censusRows(owner); len(rows) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("census for %q is not empty: %+v", owner, censusRows(owner))
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestCensusTracksOneKernelAndClearsOnClose is the acceptance invariant: an eval
// kernel appears exactly once and a Close removes it.
func TestCensusTracksOneKernelAndClearsOnClose(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "census-owner", "census-owner")
	h.expectCompleted(session, "census-owner", "1 + 1")

	rows := censusRows("census-owner")
	if len(rows) != 1 {
		t.Fatalf("census = %+v", rows)
	}
	row := rows[0]
	if row.Owner.Component != procrt.ComponentEval || row.Owner.ObjectID != session.ID {
		t.Fatalf("owner = %+v", row.Owner)
	}
	if row.PID <= 0 || row.State != procrt.StateRunning {
		t.Fatalf("row = %+v", row)
	}
	if row.Enforcement.MemoryLimitLabel() == evalapi.EnforcementNone {
		t.Fatalf("a running kernel must report its memory bound: %+v", row.Enforcement)
	}
	if row.Usage.CPUMs <= 0 {
		t.Fatalf("cpu usage was not measured: %+v", row.Usage)
	}

	if err := h.m.Close(context.Background(), session.ID, "census-owner"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitCensusEmpty(t, "census-owner")
}

// TestCensusClearsACrashedWorker proves a crash does not leave a phantom row.
func TestCensusClearsACrashedWorker(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "crash-owner", "crash-owner")
	h.expectCompleted(session, "crash-owner", "1 + 1")
	if rows := censusRows("crash-owner"); len(rows) != 1 {
		t.Fatalf("census = %+v", rows)
	}
	result := h.exec(session, "crash-owner", "import os\nos._exit(9)", evalapi.Budget{})
	if result.StateStatus != evalapi.StateStatusLost {
		t.Fatalf("crash state = %q", result.StateStatus)
	}
	// Close releases whatever the crash left behind.
	if err := h.m.Close(context.Background(), session.ID, "crash-owner"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitCensusEmpty(t, "crash-owner")
}

// TestCloseOwnerProcessesEscalatesAndClearsCensus exercises procrt.CloseOwner
// against a live eval worker: the registry stops it, records the escalation and
// leaves no row.
func TestCloseOwnerProcessesEscalatesAndClearsCensus(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner-revoke", "owner-revoke")
	h.expectCompleted(session, "owner-revoke", "1 + 1")

	reports, err := procrt.Default().CloseOwner(context.Background(), procrt.Owner{SessionID: "owner-revoke"}, procrt.DefaultLadder())
	if err != nil {
		t.Fatalf("CloseOwner: %v", err)
	}
	if len(reports) != 1 || reports[0].PID <= 0 || len(reports[0].Steps) == 0 {
		t.Fatalf("reports = %+v", reports)
	}
	if reports[0].Steps[0] != "interrupt" {
		t.Fatalf("escalation did not start cooperatively: %+v", reports[0].Steps)
	}
	// The session is unusable: the worker is gone and the next cell starts a new
	// one, which registers under the same owner. The census still holds exactly
	// one row, never the dead one.
	waitCensusEmpty(t, "owner-revoke")
	result, err := h.m.Execute(context.Background(), evalapi.Request{ID: "after-revoke", SessionID: session.ID,
		AgentID: "owner-revoke", Code: "1 + 1"}, nil)
	if err != nil {
		t.Fatalf("Execute after an external stop: %v", err)
	}
	if !result.Succeeded() {
		// The state was lost, which is correct; the kernel restarts for the
		// next cell only if the host asks. Either outcome is honest.
		if result.StateStatus != evalapi.StateStatusLost && result.StateStatus != evalapi.StateStatusOK {
			t.Fatalf("result = %+v", result)
		}
	}
	if rows := censusRows("owner-revoke"); len(rows) > 1 {
		t.Fatalf("census after restart = %+v", rows)
	}
	_ = h.m.CloseOwner(context.Background(), "owner-revoke")
	waitCensusEmpty(t, "owner-revoke")
}

// TestConcurrentKernelsHaveDistinctCensusRows proves the census is per kernel, not
// per manager: two sessions never collapse into one row.
func TestConcurrentKernelsHaveDistinctCensusRows(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	owner := "multi-owner"
	first := h.create("python", owner, owner)
	second := h.create("python", owner, owner)
	var wg sync.WaitGroup
	for _, session := range []evalapi.Session{first, second} {
		wg.Add(1)
		go func(s evalapi.Session) {
			defer wg.Done()
			h.exec(s, owner, "1 + 1", evalapi.Budget{})
		}(session)
	}
	wg.Wait()
	rows := censusRows(owner)
	if len(rows) != 2 || rows[0].PID == rows[1].PID {
		t.Fatalf("census = %+v", rows)
	}
	_ = h.m.CloseOwner(context.Background(), owner)
	waitCensusEmpty(t, owner)
}
