//go:build unix

package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestRealLLDBExceptionStopCarriesInfo covers the exception path of a real
// adapter: stopped(reason=exception) with the adapter's exception info.
func TestRealLLDBExceptionStopCarriesInfo(t *testing.T) {
	if os.Getenv("LC_TEST_LLDB") == "" {
		t.Skip("set LC_TEST_LLDB for the real adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "abort.c")
	if err := os.WriteFile(source, []byte("#include <stdlib.h>\nint main(void) { abort(); return 0; }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, "cc", "-g", "-O0", "abort.c", "-o", "abort")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile %v %s", err, out)
	}
	adapter := testLLDB(t)
	engine := agentDAPEngine(t, root, &dapScript{t: t, facts: map[string]any{}}, &DebugOptions{Sandbox: testSandbox(t), Adapters: []DebugAdapter{{ID: "lldb", Command: adapter, ReadPaths: testReadPaths(adapter), Launch: map[string]any{"disableASLR": false}}}})
	session, err := engine.NewSession(ctx, SessionOptions{Goal: "exception info"})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	snap, err := engine.DebugStart(ctx, DebugStartOptions{SessionID: session.ID, AdapterID: "lldb", Program: filepath.Join(root, "abort")})
	if err != nil {
		t.Fatal(err)
	}
	c := DebugControl{SessionID: session.ID, DebugID: snap.ID, Epoch: snap.Epoch}
	c.Action = "configure"
	_, stop, err := engine.DebugControlAndWait(ctx, c, DebugWaitOptions{Timeout: 20 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if stop.Reason != "exception" {
		t.Fatalf("abort did not stop as an exception: %+v", stop)
	}
	if stop.Exception == nil || stop.Exception.ID == "" && stop.Exception.Description == "" {
		t.Fatalf("exception info missing: %+v", stop.Exception)
	}
	if len(stop.TopFrames) == 0 {
		t.Fatalf("exception stop without frames: %+v", stop)
	}
	final, cleanup, err := engine.DebugDisconnect(ctx, c, true)
	if err != nil {
		t.Fatal(err)
	}
	if cleanup != "disconnect" || final.State != "closed" {
		t.Fatalf("disconnect cleanup=%q state=%q", cleanup, final.State)
	}
	t.Logf("LLDB_EXCEPTION: reason=%s id=%q description=%q frames=%d cleanup=%s", stop.Reason, stop.Exception.ID, stop.Exception.Description, len(stop.TopFrames), cleanup)
}

// TestRealLLDBAdapterCrashIsLost covers an abnormal adapter exit through the
// Engine: the session is listed as lost, removed, and the event is durable.
func TestRealLLDBAdapterCrashIsLost(t *testing.T) {
	if os.Getenv("LC_TEST_LLDB") == "" {
		t.Skip("set LC_TEST_LLDB for the real adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "loop.c")
	if err := os.WriteFile(source, []byte("#include <unistd.h>\nint main(void) { for (;;) { usleep(10000); } }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	build := exec.CommandContext(ctx, "cc", "-g", "-O0", "loop.c", "-o", "loop")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("compile %v %s", err, out)
	}
	adapter := testLLDB(t)
	engine := agentDAPEngine(t, root, &dapScript{t: t, facts: map[string]any{}}, &DebugOptions{Sandbox: testSandbox(t), Adapters: []DebugAdapter{{ID: "lldb", Command: adapter, ReadPaths: testReadPaths(adapter), Launch: map[string]any{"disableASLR": false}}}})
	session, err := engine.NewSession(ctx, SessionOptions{Goal: "adapter crash"})
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	snap, err := engine.DebugStart(ctx, DebugStartOptions{SessionID: session.ID, AdapterID: "lldb", Program: filepath.Join(root, "loop")})
	if err != nil {
		t.Fatal(err)
	}
	if snap.AdapterPID <= 1 {
		t.Fatalf("adapter pid not reported: %+v", snap)
	}
	process, err := os.FindProcess(snap.AdapterPID)
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		live, listErr := engine.DebugSessions(ctx, session.ID)
		if listErr != nil {
			t.Fatal(listErr)
		}
		if len(live) == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if live, _ := engine.DebugSessions(ctx, session.ID); len(live) != 0 {
		t.Fatalf("crashed session still live: %+v", live)
	}
	if processAlive(snap.AdapterPID) {
		t.Fatalf("adapter process %d still alive", snap.AdapterPID)
	}
	page, err := engine.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: session.ID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	lost := 0
	for _, event := range page.Events {
		if event.Type == "debug.session.lost" {
			lost++
		}
	}
	if lost != 1 {
		t.Fatalf("debug.session.lost events = %d", lost)
	}
	t.Logf("LLDB_CRASH: adapter pid=%d reaped, session removed, lost event durable=%d", snap.AdapterPID, lost)
}
