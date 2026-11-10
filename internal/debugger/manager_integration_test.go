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

func TestRealLLDBManagerPermissionAndOwnership(t *testing.T) {
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
	events := make(chan string, 64)
	m, err := New(Options{Sandbox: testSandbox(t), Adapters: []Adapter{{ID: "lldb", Command: adapter, ReadPaths: xcodeReadPaths(adapter), Launch: map[string]any{"disableASLR": false}}}, OnEvent: func(_ string, event string, _ map[string]any) {
		select {
		case events <- event:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	snapshot, err := m.Start(ctx, StartOptions{SessionID: "owner", AdapterID: "lldb", Workspace: root, Program: program, Actor: "agent"})
	if err != nil {
		var rejection *dap.ResponseError
		if errors.As(err, &rejection) {
			t.Logf("launch rejection: %s", rejection.Body)
		}
		t.Fatal(err)
	}
	c := Control{SessionID: "owner", DebugID: snapshot.ID, Epoch: snapshot.Epoch, Actor: "agent"}
	if _, err = m.Snapshot("foreign", snapshot.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign snapshot %v", err)
	}
	if _, err = m.Breakpoints(ctx, c, source, []Breakpoint{{Line: 4, Condition: "value=99"}}); !errors.Is(err, ErrDenied) {
		t.Fatalf("conditional execution granted %v", err)
	}
	bp, err := m.Breakpoints(ctx, c, source, []Breakpoint{{Line: 4}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("breakpoint response %s", bp)
	c.Action = "configure"
	if _, err = m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			if event == "debug.stopped" {
				goto stopped
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
stopped:
	snapshot, err = m.Snapshot("owner", snapshot.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.ThreadID = snapshot.ThreadID
	stack, err := m.Inspect(ctx, c, "stackTrace", snapshot.ThreadID, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	var frames struct {
		Frames []struct {
			ID   int `json:"id"`
			Line int `json:"line"`
		} `json:"stackFrames"`
	}
	if err = json.Unmarshal(stack, &frames); err != nil {
		t.Fatal(err)
	}
	if len(frames.Frames) == 0 || frames.Frames[0].Line != 4 {
		t.Fatalf("wrong stack %s", stack)
	}
	value, err := m.Evaluate(ctx, c, frames.Frames[0].ID, "value", false)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Result string `json:"result"`
	}
	json.Unmarshal(value, &result)
	if result.Result != "42" {
		t.Fatalf("read-only value %s", value)
	}
	if _, err = m.Evaluate(ctx, c, frames.Frames[0].ID, "value=99", false); !errors.Is(err, ErrDenied) {
		t.Fatalf("write expression accepted: %v", err)
	}
	human := c
	human.Actor = "human"
	takeover, err := m.Transfer(ctx, human, "human")
	if err != nil {
		t.Fatal(err)
	}
	c.Action = "continue"
	if _, err = m.Control(ctx, c); !errors.Is(err, ErrStale) {
		t.Fatalf("agent resumed human-owned target %v", err)
	}
	human.Epoch = takeover.Epoch
	human.Action = "stop"
	closed, err := m.Control(ctx, human)
	if err != nil || closed.State != "closed" {
		t.Fatalf("close %+v %v", closed, err)
	}
	if len(m.List("owner")) != 0 {
		t.Fatal("closed session remains live")
	}
	t.Log("DAP_MANAGER_REAL: sandbox breakpoint line=4 value=42; foreign/conditional/write/stale denied; human stop cleaned")
}
