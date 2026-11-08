package debugger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealNodeManager(t *testing.T) {
	node, adapter := os.Getenv("LC_TEST_NODE"), os.Getenv("LC_TEST_JS_DEBUG")
	if node == "" || adapter == "" {
		t.Skip("set LC_TEST_NODE and LC_TEST_JS_DEBUG")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "fixture.js")
	if err := os.WriteFile(source, []byte("let value = 41;\nvalue++;\nconsole.log(value);\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Adapters: []Adapter{{ID: "js-debug", Language: "javascript", Command: node, Args: []string{adapter, "{socket}"}, ReadPaths: []string{filepath.Dir(filepath.Dir(adapter)), filepath.Dir(filepath.Dir(node))}, Loopback: true, Transport: "unix", Launch: map[string]any{"type": "pwa-node", "request": "launch", "runtimeExecutable": node, "autoAttachChildProcesses": false, "sourceMaps": false, "outputCapture": "std", "skipFiles": []string{"<node_internals>/**"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	snap, err := m.Start(ctx, StartOptions{SessionID: "node", AdapterID: "js-debug", Workspace: root, Program: source, Actor: "human"})
	if err != nil {
		t.Fatal(err)
	}
	internal, err := m.lookup("node", snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("adapter stderr: %s", internal.process.Stderr.Snapshot().Head)
		}
	})
	c := Control{SessionID: "node", DebugID: snap.ID, Epoch: snap.Epoch, Actor: "human"}
	if _, err = m.Breakpoints(ctx, c, source, []Breakpoint{{Line: 3}}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	if _, err = m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	for {
		snap, err = m.Snapshot("node", snap.ID)
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
	if len(stack.Frames) == 0 || stack.Frames[0].Line != 3 {
		t.Fatalf("stack %s", raw)
	}
	raw, err = m.Evaluate(ctx, c, stack.Frames[0].ID, "value", false)
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Result string }
	if err = json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
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
	t.Log("NODE_REAL: private Unix socket, sandbox line=3 value=42, next, owned stop")
}
