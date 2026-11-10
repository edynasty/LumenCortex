package debugger

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/debugger/dapfake"
)

// TestE1D2OldReferenceIsRejected asserts the target behaviour for
// E1-D2 against the pre-fix Manager: an old variablesReference must be an
// error, not an empty variable list.
func TestE1D2OldReferenceIsRejected(t *testing.T) {
	ctx := context.Background()
	m, snap, root := probeFixture(t, dapfake.Config{StopDelayMS: 200, StaleEmptyVariables: true})
	_ = root
	c := Control{SessionID: snap.SessionID, DebugID: snap.ID, Epoch: snap.Epoch, Actor: "agent"}
	c.Action = "configure"
	if _, err := m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	probeStopped(t, m, c)
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
	oldRef := sc.Scopes[0].VariablesReference
	if _, err = m.Inspect(ctx, c, "variables", oldRef, 0, 20); err != nil {
		t.Fatalf("fresh reference: %v", err)
	}
	// A resumed target is observable as running, then a new stopped state;
	// this makes the "next stop" deterministic for both behaviours.
	c.Action = "continue"
	if _, err = m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	probeState(t, m, c, "running")
	probeState(t, m, c, "stopped")
	if _, err = m.Inspect(ctx, c, "variables", oldRef, 0, 20); err == nil {
		t.Fatal("E1-D2 present: a reference from the previous stop resolved without an error")
	}
}

// TestE1D3InspectionIsBounded asserts that a 200 KiB adapter value must not
// reach the caller whole.
func TestE1D3InspectionIsBounded(t *testing.T) {
	ctx := context.Background()
	m, snap, _ := probeFixture(t, dapfake.Config{StopDelayMS: 5, LargeValueBytes: 200 << 10})
	c := Control{SessionID: snap.SessionID, DebugID: snap.ID, Epoch: snap.Epoch, Actor: "agent"}
	c.Action = "configure"
	if _, err := m.Control(ctx, c); err != nil {
		t.Fatal(err)
	}
	probeStopped(t, m, c)
	snap, _ = m.Snapshot(c.SessionID, c.DebugID)
	c.ThreadID = snap.ThreadID
	stack, _ := m.Inspect(ctx, c, "stackTrace", c.ThreadID, 0, 5)
	var frames struct {
		Frames []struct{ ID int } `json:"stackFrames"`
	}
	json.Unmarshal(stack, &frames)
	scopes, _ := m.Inspect(ctx, c, "scopes", frames.Frames[0].ID, 0, 0)
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
		t.Fatalf("E1-D3 present: inspection returned %d bytes with no bound", len(vars))
	}
	if !strings.Contains(string(vars), `"truncated":true`) {
		t.Fatalf("E1-D3 present: truncation not flagged")
	}
}

func probeFixture(t *testing.T, cfg dapfake.Config) (*Manager, Snapshot, string) {
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
	m, err := New(Options{Adapters: []Adapter{{ID: "fake", Command: exe, Args: dapfake.Args(configPath), Launch: map[string]any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	snap, err := m.Start(ctx, StartOptions{SessionID: "s", AdapterID: "fake", Workspace: root, Program: filepath.Join(root, "fixture.c"), Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Breakpoints(ctx, Control{SessionID: snap.SessionID, DebugID: snap.ID, Epoch: snap.Epoch, Actor: "agent"}, filepath.Join(root, "fixture.c"), []Breakpoint{{Line: 4}}); err != nil {
		t.Fatal(err)
	}
	return m, snap, root
}

func probeStopped(t *testing.T, m *Manager, c Control) {
	t.Helper()
	probeState(t, m, c, "stopped")
}

func probeState(t *testing.T, m *Manager, c Control, want string) {
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
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("never reached %q", want)
}
