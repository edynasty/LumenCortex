package evalruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

// pythonCellAsync proves a cell can await at module level in an assignment, not
// only as a trailing expression, and that an asyncio task created in one cell is
// driven by the next: the kernel keeps one event loop for its lifetime.
func TestPythonAsyncStateSurvivesCells(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "import asyncio\nasync def add(a, b):\n    return a + b")

	// Top-level await in an assignment: the previous parser rejected this cell
	// with a SyntaxError before any await support existed.
	result := h.expectCompleted(session, "owner", "value = await add(20, 22)\nvalue")
	if len(result.Displays) != 1 || result.Displays[0].Text != "42" {
		t.Fatalf("top-level await in an assignment: %+v", result.Displays)
	}
	// A task created in one cell is completed by a later cell on the same loop.
	h.expectCompleted(session, "owner", "task = asyncio.ensure_future(add(1, 2))")
	awaited := h.expectCompleted(session, "owner", "await task")
	if len(awaited.Displays) != 1 || awaited.Displays[0].Text != "3" {
		t.Fatalf("asyncio task did not survive the cell: %+v", awaited.Displays)
	}
	// A genuinely invalid cell is still an error, not a silently async one.
	bad := h.exec(session, "owner", "def broken(:\n    pass", evalapi.Budget{})
	if bad.Succeeded() || !strings.Contains(bad.Error, "SyntaxError") {
		t.Fatalf("invalid code = %+v", bad)
	}
}

// TestPythonInspectListsNamesWithoutRenderingValues covers the inspection gap: the
// host can ask which names a kernel holds, and inspection never runs user code.
func TestPythonInspectListsNamesWithoutRenderingValues(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", `import json
def double(x):
    return x * 2
config = {"k": 1}
class Explosive:
    def __repr__(self):
        raise RuntimeError("repr must not run during inspection")
boom = Explosive()`)

	view, err := h.m.Inspect(context.Background(), session.ID, "owner", evalapi.InspectOptions{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !view.Alive || view.PID <= 0 {
		t.Fatalf("inspection = %+v", view)
	}
	if view.Generation != session.Generation {
		t.Fatalf("generation = %d, session %d", view.Generation, session.Generation)
	}
	types := map[string]string{}
	for _, entry := range view.Entries {
		types[entry.Name] = entry.Type
	}
	for name, want := range map[string]string{
		"json":   "module",
		"double": "function",
		"config": "dict",
		"boom":   "Explosive",
	} {
		if types[name] != want {
			t.Fatalf("%s = %q, want %q (entries %+v)", name, types[name], want, view.Entries)
		}
	}
	if _, ok := types["lc_tool"]; ok {
		t.Fatalf("host helpers must not be reported as user state: %+v", view.Entries)
	}
	// A repr that raises did not break the kernel and did not run.
	h.expectCompleted(session, "owner", "1 + 1")

	filtered, err := h.m.Inspect(context.Background(), session.ID, "owner", evalapi.InspectOptions{Names: []string{"config"}, Limit: 10})
	if err != nil {
		t.Fatalf("Inspect(names): %v", err)
	}
	if len(filtered.Entries) != 1 || filtered.Entries[0].Name != "config" {
		t.Fatalf("names filter = %+v", filtered.Entries)
	}
	limited, err := h.m.Inspect(context.Background(), session.ID, "owner", evalapi.InspectOptions{Limit: 2})
	if err != nil {
		t.Fatalf("Inspect(limit): %v", err)
	}
	if len(limited.Entries) != 2 || !limited.Truncated || limited.Total <= 2 {
		t.Fatalf("limit = %+v", limited)
	}
	// A reader without access cannot inspect another actor's kernel.
	if _, err := h.m.Inspect(context.Background(), session.ID, "stranger", evalapi.InspectOptions{}); err == nil {
		t.Fatalf("an unrelated actor inspected the kernel")
	}
	// Closing the kernel ends the census row and the liveness claim.
	if err := h.m.Close(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed, err := h.m.Inspect(context.Background(), session.ID, "owner", evalapi.InspectOptions{})
	if err != nil {
		t.Fatalf("Inspect(closed): %v", err)
	}
	if closed.Alive || len(closed.Entries) != 0 {
		t.Fatalf("closed kernel inspection = %+v", closed)
	}
}

// TestRestartKeepsFilesAndDiscardsState distinguishes restart from reset: a
// restart replaces the process (new generation, no variables) but keeps what the
// kernel wrote; a reset wipes the workspace too.
func TestRestartKeepsFilesAndDiscardsState(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	session := h.create("python", "owner", "owner")
	before := h.expectCompleted(session, "owner", "value = 7\nopen('kept.txt', 'w').write('kept')")

	if err := h.m.Restart(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	lost := h.exec(session, "owner", "value", evalapi.Budget{})
	if lost.Succeeded() || !strings.Contains(lost.Error, "NameError") {
		t.Fatalf("variable survived restart: %+v", lost)
	}
	kept := h.expectCompleted(session, "owner", "print(open('kept.txt').read())")
	if strings.TrimSpace(kept.Stdout) != "kept" {
		t.Fatalf("restart discarded workspace files: %q", kept.Stdout)
	}
	view, err := h.m.Session(context.Background(), session.ID, "owner")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if view.Generation <= before.Generation {
		t.Fatalf("restart did not advance the generation: %d -> %d", before.Generation, view.Generation)
	}
	if !view.Alive() {
		t.Fatalf("restart left the kernel unusable: %+v", view)
	}

	if err := h.m.Reset(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	gone := h.exec(session, "owner", "print(open('kept.txt').read())", evalapi.Budget{})
	if gone.Succeeded() || !strings.Contains(gone.Error, "No such file") {
		t.Fatalf("reset kept workspace files: %+v", gone)
	}
}

// TestJavaScriptInspectAndModulePersistence covers the same inventory for the JS
// kernel: imported modules and async values survive a cell, and inspection lists
// names and kinds without rendering them.
func TestJavaScriptInspectAndModulePersistence(t *testing.T) {
	h := newJavaScriptHarness(t, nil)
	session := h.create("javascript", "owner", "owner")
	h.expectCompleted(session, "owner", "const os = await import('node:os')\nasync function add(a, b) { return a + b }\nconst config = { k: 1 }")
	result := h.expectCompleted(session, "owner", "await add(20, 22)")
	if len(result.Displays) != 1 || result.Displays[0].Text != "42" {
		t.Fatalf("async state: %+v", result.Displays)
	}

	view, err := h.m.Inspect(context.Background(), session.ID, "owner", evalapi.InspectOptions{})
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !view.Alive {
		t.Fatalf("inspection = %+v", view)
	}
	types := map[string]string{}
	for _, entry := range view.Entries {
		types[entry.Name] = entry.Type
	}
	if types["os"] != "object" || types["add"] != "function" || types["config"] != "object" {
		t.Fatalf("entries = %+v", view.Entries)
	}
	// The imported module is still usable in a later cell.
	h.expectCompleted(session, "owner", "os.platform().length > 0")

	if err := h.m.Restart(context.Background(), session.ID, "owner"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	after := h.exec(session, "owner", "add(1, 2)", evalapi.Budget{})
	if after.Succeeded() {
		t.Fatalf("function survived restart: %+v", after)
	}
}

// TestInspectReportsOnlyItsOwnKernel proves inspection is scoped to one kernel:
// two sessions never see each other's names.
func TestInspectReportsOnlyItsOwnKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	first := h.create("python", "owner", "owner")
	second := h.create("python", "owner", "owner")
	h.expectCompleted(first, "owner", "first_only = 1")
	h.expectCompleted(second, "owner", "second_only = 2")

	one, err := h.m.Inspect(context.Background(), first.ID, "owner", evalapi.InspectOptions{})
	if err != nil {
		t.Fatalf("Inspect(first): %v", err)
	}
	two, err := h.m.Inspect(context.Background(), second.ID, "owner", evalapi.InspectOptions{})
	if err != nil {
		t.Fatalf("Inspect(second): %v", err)
	}
	names := func(v evalapi.Inspection) map[string]bool {
		out := map[string]bool{}
		for _, entry := range v.Entries {
			out[entry.Name] = true
		}
		return out
	}
	if !names(one)["first_only"] || names(one)["second_only"] {
		t.Fatalf("first kernel = %+v", one.Entries)
	}
	if !names(two)["second_only"] || names(two)["first_only"] {
		t.Fatalf("second kernel = %+v", two.Entries)
	}
}
