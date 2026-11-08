//go:build darwin

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDebugEngineRealPreparedAttach(t *testing.T) {
	if os.Getenv("LC_TEST_LLDB") == "" {
		t.Skip("set LC_TEST_LLDB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	adapter, err := exec.CommandContext(ctx, "xcrun", "--find", "lldb-dap").Output()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	source := filepath.Join(root, "fixture.c")
	program := filepath.Join(root, "fixture")
	if err = os.WriteFile(source, []byte("#include <unistd.h>\nint main(void) {\n volatile int value=42;\n for (;;) {\n  usleep(10000);\n  value=42;\n }\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "cc", "-g", "-O0", source, "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile %v %s", err, out)
	}
	e, err := Open(Options{Workspace: root, Debug: &DebugOptions{Adapters: []DebugAdapter{{ID: "lldb", Command: strings.TrimSpace(string(adapter)), ReadPaths: []string{"/Applications/Xcode.app"}, AttachPIDField: "pid", Attach: map[string]any{"waitFor": false}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	session, err := e.NewSession(ctx, SessionOptions{Goal: "owned attach"})
	if err != nil {
		t.Fatal(err)
	}
	opts := DebugStartOptions{SessionID: session.ID, AdapterID: "lldb", Program: program, PrepareForAttach: true}
	if err = e.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.DebugStart(ctx, opts); !errors.Is(err, ErrDebugDisabled) {
		t.Fatalf("ungranted attach preparation: %v", err)
	}
	if err = e.SetDebugGrant(ctx, session.ID, DebugGrant{Enabled: true, AllowAttach: true}); err != nil {
		t.Fatal(err)
	}
	snap, err := e.DebugStart(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	if snap.State != "prepared" || snap.TargetPID <= 1 {
		t.Fatalf("unprepared target %+v", snap)
	}
	c := DebugControl{SessionID: session.ID, DebugID: snap.ID, Epoch: snap.Epoch}
	if _, err = e.DebugAttach(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err = e.DebugSetBreakpoints(ctx, c, source, []DebugBreakpoint{{Line: 6}}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	if _, err = e.DebugCommand(ctx, c); err != nil {
		t.Fatal(err)
	}
	for {
		snap, err = e.dapManager.Snapshot(session.ID, snap.ID)
		if err != nil {
			t.Fatal(err)
		}
		if snap.State == "stopped" {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	raw, err := e.DebugInspect(ctx, c, "stackTrace", snap.ThreadID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var stack struct {
		Frames []struct {
			ID   int
			Name string
		} `json:"stackFrames"`
	}
	if err = json.Unmarshal(raw, &stack); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range stack.Frames {
		if f.Name == "main" {
			raw, err = e.DebugEvaluate(ctx, c, f.ID, "value", false)
			if err != nil {
				t.Fatal(err)
			}
			var value struct{ Result string }
			if err = json.Unmarshal(raw, &value); err != nil {
				t.Fatal(err)
			}
			if value.Result != "42" {
				t.Fatalf("value %s", raw)
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("main absent: %s", raw)
	}
	if err = e.SetDebugGrant(ctx, session.ID, DebugGrant{}); err != nil {
		t.Fatal(err)
	}
	if _, err = e.DebugAttach(ctx, c); !errors.Is(err, ErrDebugDisabled) {
		t.Fatalf("revoked attach: %v", err)
	}
	t.Log("ATTACH_REAL: separate grant, prepared pinned PID, same-sandbox LLDB attach, main value=42, revoke cleaned")
}
