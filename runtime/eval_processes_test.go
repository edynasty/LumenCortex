package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

// TestProcessesReportsEvalCensus proves the Hub-facing census: one row per eval
// kernel, with the enforcement label the session reports and no invented usage.
func TestProcessesReportsEvalCensus(t *testing.T) {
	engine, owner := executionTestEngine(t)
	runtimeID := executionRuntimeID(t, engine, "python")
	kernel, err := engine.CreateEvalSession(context.Background(), owner, runtimeID)
	if err != nil {
		t.Fatalf("CreateEvalSession: %v", err)
	}
	result := executeHostCell(t, engine, owner, kernel.ID, "cell-1", "value = 41\nvalue + 1")
	if !result.Succeeded() {
		t.Fatalf("cell = %+v", result)
	}

	rows, err := engine.Processes(context.Background(), owner)
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("census = %+v", rows)
	}
	row := rows[0]
	if row.Owner.Component != "eval" || row.Owner.ObjectID != kernel.ID || row.Owner.SessionID != owner {
		t.Fatalf("owner = %+v", row.Owner)
	}
	if row.PID <= 0 || row.State != "running" {
		t.Fatalf("row = %+v", row)
	}
	if row.MemoryEnforcement != kernel.Enforcement.Memory || row.MemoryEnforcement == "" || row.MemoryEnforcement == "none" {
		t.Fatalf("memory enforcement = %q, session %q", row.MemoryEnforcement, kernel.Enforcement.Memory)
	}
	if row.CPUEnforcement != evalapi.EnforcementHard {
		t.Fatalf("cpu enforcement = %q", row.CPUEnforcement)
	}
	if row.MemoryLimitBytes != kernel.Budget.MemoryBytes {
		t.Fatalf("memory limit = %d, budget %d", row.MemoryLimitBytes, kernel.Budget.MemoryBytes)
	}

	// Another Core session sees none of it: the census is per session.
	other, err := engine.NewSession(context.Background(), SessionOptions{Goal: "unrelated"})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	otherRows, err := engine.Processes(context.Background(), other.ID)
	if err != nil {
		t.Fatalf("Processes(other): %v", err)
	}
	if len(otherRows) != 0 {
		t.Fatalf("an unrelated session listed %+v", otherRows)
	}

	if err := engine.EvalClose(context.Background(), owner, kernel.ID); err != nil {
		t.Fatalf("EvalClose: %v", err)
	}
	after, err := engine.Processes(context.Background(), owner)
	if err != nil {
		t.Fatalf("Processes(after close): %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("census after close = %+v", after)
	}
}

// TestEvalInspectAndRestartThroughTheEngine covers the host API the Desktop and
// the Hub call: inspection without executing a cell, and restart keeping files.
func TestEvalInspectAndRestartThroughTheEngine(t *testing.T) {
	engine, owner := executionTestEngine(t)
	runtimeID := executionRuntimeID(t, engine, "python")
	kernel, err := engine.CreateEvalSession(context.Background(), owner, runtimeID)
	if err != nil {
		t.Fatalf("CreateEvalSession: %v", err)
	}
	if result := executeHostCell(t, engine, owner, kernel.ID, "cell-1",
		"answer = 42\nopen('notes.txt', 'w').write('kept')"); !result.Succeeded() {
		t.Fatalf("cell = %+v", result)
	}

	view, err := engine.EvalInspect(context.Background(), owner, kernel.ID, evalapi.InspectOptions{})
	if err != nil {
		t.Fatalf("EvalInspect: %v", err)
	}
	found := false
	for _, entry := range view.Entries {
		if entry.Name == "answer" && entry.Type == "int" {
			found = true
		}
	}
	if !found {
		t.Fatalf("inspection = %+v", view.Entries)
	}

	if err := engine.EvalRestart(context.Background(), owner, kernel.ID); err != nil {
		t.Fatalf("EvalRestart: %v", err)
	}
	lost, err := engine.ExecuteEval(context.Background(), owner, evalapi.Request{SessionID: kernel.ID, CellID: "cell-2", Code: "answer"})
	if err != nil {
		t.Fatalf("ExecuteEval after restart: %v", err)
	}
	if lost.Succeeded() || !strings.Contains(lost.Error, "NameError") {
		t.Fatalf("variable survived restart: %+v", lost)
	}
	kept := executeHostCell(t, engine, owner, kernel.ID, "cell-3", "print(open('notes.txt').read())")
	if strings.TrimSpace(kept.Stdout) != "kept" {
		t.Fatalf("restart discarded workspace files: %q", kept.Stdout)
	}
}
