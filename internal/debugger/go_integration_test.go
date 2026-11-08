package debugger

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/edynasty/LumenCortex/internal/debugger/dap"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRealDelveManager(t *testing.T) {
	dlv := os.Getenv("LC_TEST_DELVE")
	if dlv == "" {
		t.Skip("set LC_TEST_DELVE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "fixture.go")
	program := filepath.Join(root, "fixture")
	if err := os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main() {\n value := 41\n value++\n fmt.Println(value)\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "go", "build", "-gcflags=all=-N -l", "-o", program, source).CombinedOutput(); err != nil {
		t.Fatalf("compile: %v %s", err, out)
	}
	m, err := New(Options{Adapters: []Adapter{{ID: "dlv", Command: dlv, Args: []string{"dap", "--listen=unix:{socket}"}, ReadPaths: []string{"/Library/Developer/CommandLineTools/Library/PrivateFrameworks/LLDB.framework"}, Transport: "unix", Loopback: true, Launch: map[string]any{"mode": "exec"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	snap, err := m.Start(ctx, StartOptions{SessionID: "go", AdapterID: "dlv", Workspace: root, Program: program, Actor: "human"})
	if err != nil {
		var rejection *dap.ResponseError
		if errors.As(err, &rejection) {
			t.Logf("rejection %s", rejection.Body)
		}
		t.Fatal(err)
	}
	c := Control{SessionID: "go", DebugID: snap.ID, Epoch: snap.Epoch, Actor: "human"}
	if _, err = m.Breakpoints(ctx, c, source, []Breakpoint{{Line: 6}}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	if _, err = m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	for {
		snap, err = m.Snapshot("go", snap.ID)
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
	raw, err := m.Inspect(ctx, c, "stackTrace", snap.ThreadID, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	var stack struct {
		Frames []struct{ ID, Line int } `json:"stackFrames"`
	}
	if err = json.Unmarshal(raw, &stack); err != nil {
		t.Fatal(err)
	}
	if len(stack.Frames) == 0 || stack.Frames[0].Line != 6 {
		t.Fatalf("stack %s", raw)
	}
	raw, err = m.Evaluate(ctx, c, stack.Frames[0].ID, "value", false)
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Result string }
	json.Unmarshal(raw, &v)
	if v.Result != "42" {
		t.Fatalf("value %s", raw)
	}
	c.ThreadID = snap.ThreadID
	c.Action = "step_over"
	if _, err = m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.Action = "stop"
	if _, err = m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	t.Log("DELVE_REAL: sandbox line=6 initialized value=42, next, owned stop")
}
