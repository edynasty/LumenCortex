package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/debugger/dapfake"
)

type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(_ string, event string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}
func (l *eventLog) count(prefix string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}

// fakeFixture starts the deterministic adapter in root and returns the manager,
// the live snapshot and the workspace.
func fakeFixture(t *testing.T, cfg dapfake.Config) (*Manager, Snapshot, string, *eventLog) {
	t.Helper()
	return fakeFixtureWith(t, cfg, 0, "")
}

// fakeFixtureWith is fakeFixture with a session lifetime (0 keeps the default)
// and the Agent run the session belongs to.
func fakeFixtureWith(t *testing.T, cfg dapfake.Config, lifetime time.Duration, runID string) (*Manager, Snapshot, string, *eventLog) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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
	log := &eventLog{}
	m, err := New(Options{Sandbox: testSandbox(t), Adapters: []Adapter{{ID: "fake", Command: exe, Args: dapfake.Args(configPath), Launch: map[string]any{}}}, Lifetime: lifetime, OnEvent: log.add})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	snap, err := m.Start(ctx, StartOptions{SessionID: "s", RunID: runID, AdapterID: "fake", Workspace: root, Program: filepath.Join(root, "fixture.c"), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Breakpoints(ctx, control(snap), filepath.Join(root, "fixture.c"), []Breakpoint{{Line: 4}}); err != nil {
		t.Fatal(err)
	}
	return m, snap, root, log
}

func control(s Snapshot) Control {
	return Control{SessionID: s.SessionID, DebugID: s.ID, Epoch: s.Epoch, Actor: "agent"}
}

// TestStepInvalidatesReferences is E1-D2: the adapter answers an old
// variablesReference with an empty list; the Manager must reject it instead.
func TestStepInvalidatesReferences(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5, StaleEmptyVariables: true})
	c := control(snap)
	c.Action = "configure"
	if _, err := m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, c, "stopped")
	snap, _ = m.Snapshot(c.SessionID, c.DebugID)
	c.ThreadID = snap.ThreadID

	stack, err := m.Inspect(ctx, c, "stackTrace", c.ThreadID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	var frames struct {
		Frames []struct{ ID int } `json:"stackFrames"`
	}
	if err = json.Unmarshal(stack, &frames); err != nil || len(frames.Frames) == 0 {
		t.Fatalf("frames %s %v", stack, err)
	}
	scopes, err := m.Inspect(ctx, c, "scopes", frames.Frames[0].ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sc struct {
		Scopes []struct{ Ref int } `json:"scopes"`
	}
	var rawScopes struct {
		Scopes []struct {
			VariablesReference int `json:"variablesReference"`
		} `json:"scopes"`
	}
	if err = json.Unmarshal(scopes, &rawScopes); err != nil || len(rawScopes.Scopes) == 0 {
		t.Fatalf("scopes %s %v", scopes, err)
	}
	oldRef := rawScopes.Scopes[0].VariablesReference
	oldFrame := frames.Frames[0].ID
	vars, err := m.Inspect(ctx, c, "variables", oldRef, 0, 20)
	if err != nil {
		t.Fatalf("fresh reference failed: %v", err)
	}
	var v struct {
		Variables []struct{ Name, Value string } `json:"variables"`
	}
	if err = json.Unmarshal(vars, &v); err != nil || len(v.Variables) == 0 {
		t.Fatalf("fresh variables %s", vars)
	}
	_ = sc

	c.Action = "step_over"
	c.ThreadID = snap.ThreadID
	if _, _, err = m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Inspect(ctx, c, "variables", oldRef, 0, 20); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("old variables reference accepted: %v", err)
	}
	if _, err = m.Inspect(ctx, c, "scopes", oldFrame, 0, 0); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("old frame accepted: %v", err)
	}
	// A fresh read of the stack mints current references that do resolve.
	stack, err = m.Inspect(ctx, c, "stackTrace", c.ThreadID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(stack, &frames); err != nil || len(frames.Frames) == 0 {
		t.Fatalf("frames after step %s", stack)
	}
	scopes, err = m.Inspect(ctx, c, "scopes", frames.Frames[0].ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(scopes, &rawScopes); err != nil || len(rawScopes.Scopes) == 0 {
		t.Fatalf("scopes after step %s", scopes)
	}
	vars, err = m.Inspect(ctx, c, "variables", rawScopes.Scopes[0].VariablesReference, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(vars, &v); err != nil || len(v.Variables) == 0 {
		t.Fatalf("fresh variables after step %s", vars)
	}
}

// TestInspectAndEvaluateAreTruncated is E1-D3: the Manager, not only the
// Desktop view, bounds every value and the whole result.
func TestInspectAndEvaluateAreTruncated(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5, LargeValueBytes: 200 << 10})
	c := control(snap)
	c.Action = "configure"
	if _, err := m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, c, "stopped")
	snap, _ = m.Snapshot(c.SessionID, c.DebugID)
	c.ThreadID = snap.ThreadID
	stack, err := m.Inspect(ctx, c, "stackTrace", c.ThreadID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	var frames struct {
		Frames []struct{ ID int } `json:"stackFrames"`
	}
	json.Unmarshal(stack, &frames)
	scopes, err := m.Inspect(ctx, c, "scopes", frames.Frames[0].ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sc struct {
		Scopes []struct {
			VariablesReference int `json:"variablesReference"`
		} `json:"scopes"`
	}
	json.Unmarshal(scopes, &sc)
	vars, err := m.Inspect(ctx, c, "variables", sc.Scopes[0].VariablesReference, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(vars) > 32<<10 {
		t.Fatalf("inspection result not bounded: %d bytes", len(vars))
	}
	var body struct {
		Variables []struct{ Name, Value string } `json:"variables"`
		Truncated bool                           `json:"truncated"`
	}
	if err = json.Unmarshal(vars, &body); err != nil {
		t.Fatal(err)
	}
	if !body.Truncated {
		t.Fatalf("truncation not flagged: %s", vars[:min(len(vars), 200)])
	}
	for _, v := range body.Variables {
		if len(v.Value) > 3<<10 {
			t.Fatalf("value %s is %d bytes", v.Name, len(v.Value))
		}
	}
	value, err := m.Evaluate(ctx, c, frames.Frames[0].ID, "blob", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(value) > 32<<10 {
		t.Fatalf("evaluate result not bounded: %d bytes", len(value))
	}
	var ev struct {
		Result    string `json:"result"`
		Truncated bool   `json:"truncated"`
	}
	if err = json.Unmarshal(value, &ev); err != nil {
		t.Fatal(err)
	}
	if !ev.Truncated || len(ev.Result) > 3<<10 {
		t.Fatalf("evaluate not truncated: truncated=%v len=%d", ev.Truncated, len(ev.Result))
	}
}

// TestEvaluateGrantIsDistinctFromDebuggingDisabled is E1-D4.
func TestEvaluateGrantIsDistinctFromDebuggingDisabled(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5})
	c := control(snap)
	c.Action = "configure"
	if _, err := m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	waitState(t, m, c, "stopped")
	snap, _ = m.Snapshot(c.SessionID, c.DebugID)
	c.ThreadID = snap.ThreadID
	stack, err := m.Inspect(ctx, c, "stackTrace", c.ThreadID, 0, 5)
	if err != nil {
		t.Fatal(err)
	}
	var frames struct {
		Frames []struct{ ID int } `json:"stackFrames"`
	}
	json.Unmarshal(stack, &frames)
	_, err = m.Evaluate(ctx, c, frames.Frames[0].ID, "count * 14 + 1", true)
	if !errors.Is(err, ErrEvaluateNotGranted) {
		t.Fatalf("ungranted evaluation = %v, want ErrEvaluateNotGranted", err)
	}
	if errors.Is(err, ErrDenied) {
		t.Fatalf("ungranted evaluation is conflated with a denied operation: %v", err)
	}
	// The rest of the session still works under the same grant.
	if _, err = m.Inspect(ctx, c, "threads", 0, 0, 0); err != nil {
		t.Fatalf("session unusable after evaluation refusal: %v", err)
	}
}

// TestContinueWaitsForStopAndReturnsFrames is E1-D1.
func TestContinueWaitsForStopAndReturnsFrames(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 60})
	c := control(snap)
	c.Action = "configure"
	start := time.Now()
	got, stop, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if stop.Reason != "breakpoint" || stop.ThreadID == 0 || stop.StopEpoch == 0 {
		t.Fatalf("no stop info: %+v", stop)
	}
	if len(stop.TopFrames) == 0 || stop.TopFrames[0].Line != 4 || stop.TopFrames[0].Name != "scale" {
		t.Fatalf("frames %+v", stop.TopFrames)
	}
	if got.State != "stopped" {
		t.Fatalf("snapshot %+v", got)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatal("returned before the stop")
	}
	// Step waits for the next stop and reports fresh frames.
	c.Action = "step_over"
	c.ThreadID = stop.ThreadID
	_, step, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if step.Reason != "step" || step.StopEpoch <= stop.StopEpoch {
		t.Fatalf("step stop %+v", step)
	}
	if len(step.TopFrames) == 0 || step.TopFrames[0].Line != 5 {
		t.Fatalf("step frames %+v", step.TopFrames)
	}
}

// TestStopReportFramesBecomeStale is E1-D2 for the frames a continue/step call
// returns with its stop: they are references like stackTrace frames, so after
// the next stop scopes and evaluate must reject them instead of forwarding them
// to an adapter that answers an unknown frame with an empty list.
func TestStopReportFramesBecomeStale(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5})
	c := control(snap)
	c.Action = "configure"
	_, first, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil || len(first.TopFrames) == 0 {
		t.Fatalf("first stop %+v %v", first, err)
	}
	frame := first.TopFrames[0].ID
	scopes, err := m.Inspect(ctx, c, "scopes", frame, 0, 0)
	if err != nil || !strings.Contains(string(scopes), `"Locals"`) {
		t.Fatalf("a frame of the current stop report did not resolve: %s %v", scopes, err)
	}
	c.ThreadID = first.ThreadID
	c.Action = "step_over"
	if _, _, err = m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if raw, err := m.Inspect(ctx, c, "scopes", frame, 0, 0); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("scopes of a frame from the previous stop report = %s, %v; want ErrStaleReference", raw, err)
	}
	if raw, err := m.Evaluate(ctx, c, frame, "count", false); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("evaluate in a frame from the previous stop report = %s, %v; want ErrStaleReference", raw, err)
	}
}

// TestReusedNumbersKeepTheirOwnEpochs is E1-D2 with Delve's numbering: frame ids
// and variables references both start at 1000 at every stop. Re-reading the
// stack after a step, as debug_step tells the Agent to, mints frame 1000 again;
// that must not make the previous stop's variables reference 1000 current,
// which the adapter would resolve against the new stop.
func TestReusedNumbersKeepTheirOwnEpochs(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5, ReuseReferences: true})
	c := control(snap)
	c.Action = "configure"
	_, first, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil || len(first.TopFrames) == 0 {
		t.Fatalf("first stop %+v %v", first, err)
	}
	c.ThreadID = first.ThreadID
	scopes, err := m.Inspect(ctx, c, "scopes", first.TopFrames[0].ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sc struct {
		Scopes []struct {
			VariablesReference int `json:"variablesReference"`
		} `json:"scopes"`
	}
	if err = json.Unmarshal(scopes, &sc); err != nil || len(sc.Scopes) == 0 {
		t.Fatalf("scopes %s %v", scopes, err)
	}
	locals := sc.Scopes[0].VariablesReference
	if locals != first.TopFrames[0].ID {
		t.Fatalf("fixture must reuse one number for frame and locals: frame %d, locals %d", first.TopFrames[0].ID, locals)
	}
	if _, err = m.Inspect(ctx, c, "variables", locals, 0, 20); err != nil {
		t.Fatalf("fresh locals: %v", err)
	}
	c.Action = "step_over"
	if _, _, err = m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Inspect(ctx, c, "stackTrace", c.ThreadID, 0, 5); err != nil {
		t.Fatal(err)
	}
	if raw, err := m.Inspect(ctx, c, "variables", locals, 0, 20); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("the previous stop's locals reference %d = %s, %v; want ErrStaleReference", locals, raw, err)
	}
}

// TestWaitTimeoutReturnsAsyncState covers the explicit asynchronous result.
func TestWaitTimeoutReturnsAsyncState(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 1500})
	c := control(snap)
	c.Action = "configure"
	got, stop, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if stop.Reason != "timeout" || got.State == "stopped" {
		t.Fatalf("timeout state %+v %+v", got, stop)
	}
	// The later stop is not lost: the next inspection sees it.
	waitState(t, m, c, "stopped")
}

// TestExceptionStopCarriesInfoAndFilters is the exception-breakpoint contract.
func TestExceptionStopCarriesInfoAndFilters(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5, Exception: true})
	c := control(snap)
	applied, err := m.SetExceptionBreakpoints(ctx, c, []string{"all"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(applied), `"all"`) {
		t.Fatalf("filters %s", applied)
	}
	if _, err = m.SetExceptionBreakpoints(ctx, c, []string{"nonsense"}); err == nil {
		t.Fatal("unknown filter accepted")
	}
	c.Action = "configure"
	_, stop, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if stop.Reason != "exception" || stop.Exception == nil {
		t.Fatalf("exception stop %+v", stop)
	}
	if stop.Exception.ID != "SIGABRT" || stop.Exception.Description == "" {
		t.Fatalf("exception info %+v", stop.Exception)
	}
}

// TestExceptionFiltersCanBeCleared replaces enabled exception breakpoints with
// an empty list. The fake target throws whenever it resumes, and its adapter
// stops on that only while it holds an exception filter, so the stop reason
// shows which filters the adapter really has, not what the Manager reported.
func TestExceptionFiltersCanBeCleared(t *testing.T) {
	ctx := context.Background()
	m, snap, _, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5, ThrowOnResume: true})
	c := control(snap)
	if _, err := m.SetExceptionBreakpoints(ctx, c, []string{"all"}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	_, stop, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil || stop.Reason != "exception" {
		t.Fatalf("enabled filter: stop %+v %v", stop, err)
	}
	applied, err := m.SetExceptionBreakpoints(ctx, c, []string{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(applied), `"filters":[]`) {
		t.Fatalf("cleared filters reported as %s", applied)
	}
	c.ThreadID = stop.ThreadID
	c.Action = "continue"
	_, stop, err = m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if stop.Reason != "breakpoint" || stop.Exception != nil {
		t.Fatalf("the adapter still stops on exceptions after the filters were cleared: %+v", stop)
	}
}

// TestBreakpointsReportAdapterVerifiedState checks the response mirrors the
// adapter's verified flag and line instead of echoing the request.
func TestBreakpointsReportAdapterVerifiedState(t *testing.T) {
	ctx := context.Background()
	m, snap, root, _ := fakeFixture(t, dapfake.Config{StopDelayMS: 5, Unverified: true})
	raw, err := m.Breakpoints(ctx, control(snap), filepath.Join(root, "fixture.c"), []Breakpoint{{Line: 7}})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Breakpoints []struct {
			Verified *bool `json:"verified"`
			Line     int   `json:"line"`
		} `json:"breakpoints"`
	}
	if err = json.Unmarshal(raw, &out); err != nil || len(out.Breakpoints) == 0 {
		t.Fatalf("breakpoints %s %v", raw, err)
	}
	if out.Breakpoints[0].Verified == nil || *out.Breakpoints[0].Verified {
		t.Fatalf("adapter unverified state lost: %s", raw)
	}
	if out.Breakpoints[0].Line != 7 {
		t.Fatalf("adapter line lost: %s", raw)
	}
}

// TestDisconnectIsGracefulThenFallsBack covers debug_stop cleanup reporting.
func TestDisconnectIsGracefulThenFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     dapfake.Config
		cleanup string
	}{
		{"graceful", dapfake.Config{StopDelayMS: 5}, "disconnect"},
		{"fallback", dapfake.Config{StopDelayMS: 5, IgnoreDisconnect: true}, "kill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, snap, _, _ := fakeFixture(t, tc.cfg)
			start := time.Now()
			final, cleanup, err := m.Disconnect(ctx, control(snap), true)
			if err != nil {
				t.Fatal(err)
			}
			if cleanup != tc.cleanup {
				t.Fatalf("cleanup %q, want %q", cleanup, tc.cleanup)
			}
			if final.State != "closed" {
				t.Fatalf("final %+v", final)
			}
			if len(m.List(snap.SessionID)) != 0 {
				t.Fatal("session still live")
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Fatalf("stop took %v", elapsed)
			}
			if _, err = m.Snapshot(snap.SessionID, snap.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("session still readable: %v", err)
			}
		})
	}
}

// TestAdapterCrashPublishesLost covers abnormal adapter exit.
func TestAdapterCrashPublishesLost(t *testing.T) {
	ctx := context.Background()
	// Requests before the crash: initialize, launch, setBreakpoints. The
	// configurationDone request is answered by the process dying, not by a reply.
	m, snap, _, log := fakeFixture(t, dapfake.Config{StopDelayMS: 5, CrashAfter: 4})
	c := control(snap)
	c.Action = "configure"
	if _, err := m.Control(ctx, c); err == nil {
		t.Fatal("a crashed adapter acknowledged configurationDone")
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(m.List(snap.SessionID)) == 0 && log.count("debug.session.lost") == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if log.count("debug.session.lost") != 1 {
		t.Fatalf("lost event missing: %v", log.events)
	}
	if len(m.List(snap.SessionID)) != 0 {
		t.Fatal("crashed session still listed")
	}
	if snap.AdapterPID <= 1 {
		t.Fatalf("adapter pid not reported: %+v", snap)
	}
	if err := sendSignal0(snap.AdapterPID); err == nil {
		t.Fatalf("adapter process %d still alive", snap.AdapterPID)
	}
}

// TestHostCloseIsNotReportedAsLost ends a live, stopped session the ways the
// host does: session revocation, run end, Manager shutdown and lifetime expiry.
// Each is a stop the host chose; "debug.session.lost" is reserved for an
// adapter that exits on its own (TestAdapterCrashPublishesLost).
func TestHostCloseIsNotReportedAsLost(t *testing.T) {
	cases := []struct {
		name     string
		lifetime time.Duration
		close    func(*Manager, Snapshot) error
	}{
		{name: "close owner", close: func(m *Manager, s Snapshot) error { return m.CloseOwner(s.SessionID) }},
		{name: "close run", close: func(m *Manager, s Snapshot) error { return m.CloseRun(s.SessionID, s.RunID) }},
		{name: "manager close", close: func(m *Manager, _ Snapshot) error { return m.Close() }},
		{name: "lifetime expiry", lifetime: time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			m, snap, _, log := fakeFixtureWith(t, dapfake.Config{StopDelayMS: 5}, tc.lifetime, "run-1")
			c := control(snap)
			c.Action = "configure"
			if _, _, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second}); err != nil {
				t.Fatal(err)
			}
			if tc.close != nil {
				if err := tc.close(m, snap); err != nil {
					t.Fatalf("close: %v", err)
				}
			}
			deadline := time.Now().Add(5 * time.Second)
			for log.count("debug.session.stopped") == 0 && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if log.count("debug.session.stopped") != 1 {
				t.Fatalf("session never stopped: %v", log.events)
			}
			// The adapter's transport ends asynchronously after the stop; give
			// its classification time to happen before asserting it did not.
			time.Sleep(500 * time.Millisecond)
			if n := log.count("debug.session.lost"); n != 0 {
				t.Fatalf("host close reported as %d lost adapter(s): %v", n, log.events)
			}
		})
	}
}

func waitState(t *testing.T, m *Manager, c Control, want string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		s, err := m.Snapshot(c.SessionID, c.DebugID)
		if err != nil {
			t.Fatal(err)
		}
		if s.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session never reached %q", want)
}
