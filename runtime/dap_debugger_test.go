package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestDebugEngineRealGrantRevocationAndDurableEvents(t *testing.T) {
	if os.Getenv("LC_TEST_LLDB") == "" {
		t.Skip("set LC_TEST_LLDB for real adapter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	adapter := testLLDB(t)
	root := t.TempDir()
	source := filepath.Join(root, "fixture.c")
	program := filepath.Join(root, "fixture")
	if err := os.WriteFile(source, []byte("int main(void) {\n int value=41;\n value++;\n return value==42?0:1;\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "cc", "-g", "-O0", source, "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile %v %s", err, out)
	}
	e, err := Open(Options{Workspace: root, Debug: &DebugOptions{Sandbox: testSandbox(t), Adapters: []DebugAdapter{{ID: "lldb", Command: adapter, ReadPaths: testReadPaths(adapter), Launch: map[string]any{"disableASLR": false}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	session, err := e.NewSession(ctx, SessionOptions{Goal: "DAP grant smoke"})
	if err != nil {
		t.Fatal(err)
	}
	opts := DebugStartOptions{SessionID: session.ID, AdapterID: "lldb", Program: program}
	if _, err = e.DebugStart(ctx, opts); !errors.Is(err, ErrDebugDisabled) {
		t.Fatalf("ungranted launch %v", err)
	}
	if err = e.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := e.DebugStart(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	c := DebugControl{SessionID: session.ID, DebugID: snapshot.ID, Epoch: snapshot.Epoch}
	if _, err = e.DebugSetBreakpoints(ctx, c, source, []DebugBreakpoint{{Line: 4}}); err != nil {
		t.Fatal(err)
	}
	if err = e.SetDebugGrant(ctx, session.ID, DebugGrant{}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	if _, err = e.DebugCommand(ctx, c); !errors.Is(err, ErrDebugDisabled) {
		t.Fatalf("revoked execution resumed %v", err)
	}
	if live := e.dapManager.List(session.ID); len(live) != 0 {
		t.Fatalf("revocation left process %+v", live)
	}
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: session.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	started, stopped := false, false
	for _, event := range page.Events {
		started = started || event.Type == "debug.session.started"
		stopped = stopped || event.Type == "debug.session.stopped"
	}
	if !started || !stopped {
		t.Fatalf("debug lifecycle absent started=%v stopped=%v", started, stopped)
	}
	t.Log("ENGINE_DAP_REAL: ungranted launch denied; configured real sandbox LLDB; revoke reaped; late control denied; lifecycle durable")
}
