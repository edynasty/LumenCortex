package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/debugger/dapfake"
	"github.com/edynasty/LumenCortex/internal/toolset"
)

// TestFakeDAPAdapterHelper is the fake adapter process of this package.
func TestFakeDAPAdapterHelper(t *testing.T) {
	served, err := dapfake.ServeIfSelected()
	if err != nil {
		t.Fatal(err)
	}
	if !served {
		t.Skip("only runs as a fake DAP adapter child")
	}
	os.Exit(0)
}

func fakeDebugEngine(t *testing.T, cfg dapfake.Config) (*Engine, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.c"), []byte("int main(void){return 0;}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configPath, err := dapfake.WriteConfig(root, cfg)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := filepath.Abs(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	e, err := Open(Options{Workspace: root, Debug: &DebugOptions{Sandbox: testSandbox(t), Adapters: []DebugAdapter{{ID: "fake", Command: exe, Args: dapfake.Args(configPath)}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	session, err := e.NewSession(ctx, SessionOptions{Goal: "dap fake"})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return e, session.ID, root
}

// TestE1D4EvaluateGrantIsDistinct asserts the target behaviour for E1-D4:
// a missing evaluation grant is not "debugging is disabled".
func TestE1D4EvaluateGrantIsDistinct(t *testing.T) {
	ctx := context.Background()
	e, sessionID, root := fakeDebugEngine(t, dapfake.Config{StopDelayMS: 5})
	snap, err := e.DebugStart(ctx, DebugStartOptions{SessionID: sessionID, AdapterID: "fake", Program: filepath.Join(root, "fixture.c")})
	if err != nil {
		t.Fatal(err)
	}
	c := DebugControl{SessionID: sessionID, DebugID: snap.ID, Epoch: snap.Epoch}
	if _, err = e.DebugSetBreakpoints(ctx, c, filepath.Join(root, "fixture.c"), []DebugBreakpoint{{Line: 4}}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	if _, err = e.DebugCommand(ctx, c); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		snap, err = e.dapManager.Snapshot(sessionID, snap.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snap.State == "stopped" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	raw, err := e.DebugInspect(ctx, c, "stackTrace", snap.ThreadID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	var frames struct {
		Frames []struct{ ID int } `json:"stackFrames"`
	}
	json.Unmarshal(raw, &frames)
	if len(frames.Frames) == 0 {
		t.Fatalf("no frames: %s", raw)
	}
	_, err = e.DebugEvaluate(ctx, c, frames.Frames[0].ID, "count * 14 + 1", true)
	if err == nil {
		t.Fatal("ungranted side-effecting evaluation succeeded")
	}
	if errors.Is(err, ErrDebugDisabled) || strings.Contains(err.Error(), "program debugging is disabled") {
		t.Fatalf("E1-D4 present: evaluation grant refusal reported as debugging disabled: %v", err)
	}
}

// TestE1D5ModuleEventsStayLive asserts that adapter module events never
// reach durable runtime history.
func TestE1D5ModuleEventsStayLive(t *testing.T) {
	ctx := context.Background()
	e, sessionID, root := fakeDebugEngine(t, dapfake.Config{StopDelayMS: 5, Modules: 25})
	// Subscribe before start: module events arrive during launch.
	live, stop := e.events.subscribeSession(256, sessionID)
	defer stop()
	snap, err := e.DebugStart(ctx, DebugStartOptions{SessionID: sessionID, AdapterID: "fake", Program: filepath.Join(root, "fixture.c")})
	if err != nil {
		t.Fatal(err)
	}
	c := DebugControl{SessionID: sessionID, DebugID: snap.ID, Epoch: snap.Epoch}
	c.Action = "configure"
	if _, err = e.DebugCommand(ctx, c); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(8 * time.Second)
	liveModules := 0
	for {
		select {
		case event := <-live:
			if event.Type == "debug.module" {
				liveModules++
			}
			if event.Type == "debug.stopped" {
				goto stopped
			}
		case <-deadline:
			t.Fatal("no stop")
		}
	}
stopped:
	if liveModules == 0 {
		t.Fatal("live bus received no module events; they must stay live even though they are not durable")
	}
	if _, err = e.DebugCommand(ctx, DebugControl{SessionID: sessionID, DebugID: snap.ID, Epoch: snap.Epoch, Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: sessionID, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	modules := 0
	for _, event := range page.Events {
		if event.Type == "debug.module" {
			modules++
		}
	}
	if modules != 0 {
		t.Fatalf("E1-D5 present: %d debug.module events in durable history", modules)
	}
}

// TestE1D1ContinueReturnsStopInOneCall asserts the target behaviour of the
// Agent-facing continue tool: one call returns the stop and its frames.
func TestE1D1ContinueReturnsStopInOneCall(t *testing.T) {
	ctx := context.Background()
	e, sessionID, root := fakeDebugEngine(t, dapfake.Config{StopDelayMS: 40})
	snap, err := e.startProgramDebug(ctx, e.dapContext, DebugStartOptions{SessionID: sessionID, AdapterID: "fake", Program: filepath.Join(root, "fixture.c")}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	set, err := toolset.New(toolset.Options{Workspace: root, Policy: toolset.PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.debugTools(ctx, sessionID, "", toolset.PolicyFull, true, set); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) string {
		t.Helper()
		result, err := set.Execute(ctx, name, args, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result.Content
	}
	call("debug_set_breakpoint", map[string]any{"debug_id": snap.ID, "epoch": snap.Epoch, "path": filepath.Join(root, "fixture.c"), "breakpoints": []any{map[string]any{"line": 4}}})
	raw := call("debug_continue", map[string]any{"debug_id": snap.ID, "epoch": snap.Epoch, "action": "configure"})
	var stop struct {
		Reason    string `json:"reason"`
		ThreadID  int    `json:"threadId"`
		StopEpoch uint64 `json:"stopEpoch"`
		TopFrames []struct {
			Name string `json:"name"`
			Line int    `json:"line"`
		} `json:"topFrames"`
	}
	if err = json.Unmarshal([]byte(raw), &stop); err != nil {
		t.Fatalf("one call must return stop info, got %s", raw)
	}
	if stop.Reason != "breakpoint" || len(stop.TopFrames) == 0 || stop.TopFrames[0].Line != 4 {
		t.Fatalf("E1-D1 present: %s", raw)
	}
}

// TestDebugToolsExceptionFiltersAndStopCleanup covers the tool surface added
// with the Manager contract: exception_filters on debug_set_breakpoint and the
// cleanup method in debug_stop.
func TestDebugToolsExceptionFiltersAndStopCleanup(t *testing.T) {
	ctx := context.Background()
	e, sessionID, root := fakeDebugEngine(t, dapfake.Config{StopDelayMS: 5})
	snap, err := e.startProgramDebug(ctx, e.dapContext, DebugStartOptions{SessionID: sessionID, AdapterID: "fake", Program: filepath.Join(root, "fixture.c")}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	set, err := toolset.New(toolset.Options{Workspace: root, Policy: toolset.PolicyFull})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.debugTools(ctx, sessionID, "", toolset.PolicyFull, true, set); err != nil {
		t.Fatal(err)
	}
	call := func(name string, args map[string]any) string {
		t.Helper()
		result, err := set.Execute(ctx, name, args, nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return result.Content
	}
	base := map[string]any{"debug_id": snap.ID, "epoch": snap.Epoch, "path": filepath.Join(root, "fixture.c"), "breakpoints": []any{map[string]any{"line": 4}}, "exception_filters": []any{"all"}}
	out := call("debug_set_breakpoint", base)
	if !strings.Contains(out, `"exceptionFilters":["all","uncaught"]`) {
		t.Fatalf("supported exception filters not reported: %s", out)
	}
	res, err := set.Execute(ctx, "debug_set_breakpoint", map[string]any{"debug_id": snap.ID, "epoch": snap.Epoch, "path": filepath.Join(root, "fixture.c"), "breakpoints": []any{map[string]any{"line": 4}}, "exception_filters": []any{"not-a-filter"}}, nil)
	if err == nil {
		t.Fatalf("unknown exception filter accepted: %s", res.Content)
	}
	stop := call("debug_stop", map[string]any{"debug_id": snap.ID, "epoch": snap.Epoch, "terminate_debuggee": true})
	if !strings.Contains(stop, `"cleanup":"disconnect"`) || !strings.Contains(stop, `"state":"closed"`) {
		t.Fatalf("stop result missing cleanup: %s", stop)
	}
}
