package debugger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRealPythonManager(t *testing.T) {
	python, adapter := os.Getenv("LC_TEST_PYTHON"), os.Getenv("LC_TEST_DEBUGPY")
	if python == "" || adapter == "" {
		t.Skip("set LC_TEST_PYTHON and LC_TEST_DEBUGPY")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	root := t.TempDir()
	source := filepath.Join(root, "fixture.py")
	if err := os.WriteFile(source, []byte("value = 41\nvalue += 1\nassert value == 42\nprint(value)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{Adapters: []Adapter{{ID: "debugpy", Command: python, Args: []string{"-I", adapter}, ReadPaths: []string{filepath.Dir(filepath.Dir(python)), filepath.Dir(filepath.Dir(adapter))}, Loopback: true, Launch: map[string]any{"python": []string{python}, "justMyCode": true}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	snap, err := m.Start(ctx, StartOptions{SessionID: "python", AdapterID: "debugpy", Workspace: root, Program: source, Actor: "human"})
	if err != nil {
		t.Fatal(err)
	}
	c := Control{SessionID: "python", DebugID: snap.ID, Epoch: snap.Epoch, Actor: "human"}
	if _, err = m.Breakpoints(ctx, c, source, []Breakpoint{{Line: 3}}); err != nil {
		t.Fatal(err)
	}
	c.Action = "configure"
	if _, err = m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	for {
		snap, err = m.Snapshot("python", snap.ID)
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
	t.Log("DEBUGPY_REAL: sandbox line=3 initialized value=42, next, owned stop")
}
