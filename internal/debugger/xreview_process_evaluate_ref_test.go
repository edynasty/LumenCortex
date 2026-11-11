package debugger

// Cross-review (B2 process/eval/DAP), PED-7 residual: a variables reference an
// evaluate response hands to the Agent is a reference like a scopes/variables
// one. After the next stop it must be rejected as stale even when the adapter
// reuses the number (Delve restarts its reference counter at every stop).
// Before the fix, evaluate's reference was never recorded, so it was forwarded
// unchecked and the adapter resolved it against the new stop.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/debugger/dapfake"
)

// xreviewEvaluateFixture is fakeFixture with the evaluate grant enabled, so the
// adapter's own evaluate response is the one the Manager hands to the caller.
func xreviewEvaluateFixture(t *testing.T, cfg dapfake.Config) (*Manager, Snapshot) {
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
	m, err := New(Options{Sandbox: testSandbox(t), Adapters: []Adapter{{ID: "fake", Command: exe, Args: dapfake.Args(configPath)}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	snap, err := m.Start(ctx, StartOptions{SessionID: "s", AdapterID: "fake", Workspace: root, Program: filepath.Join(root, "fixture.c"), Actor: "agent", AllowEvaluate: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Breakpoints(ctx, control(snap), filepath.Join(root, "fixture.c"), []Breakpoint{{Line: 4}}); err != nil {
		t.Fatal(err)
	}
	return m, snap
}

func TestXReviewEvaluateReferenceBecomesStale(t *testing.T) {
	ctx := context.Background()
	m, snap := xreviewEvaluateFixture(t, dapfake.Config{StopDelayMS: 5, ReuseReferences: true, EvaluateReference: true})
	c := control(snap)
	c.Action = "configure"
	_, first, err := m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second})
	if err != nil || len(first.TopFrames) == 0 {
		t.Fatalf("first stop %+v %v", first, err)
	}
	c.ThreadID = first.ThreadID
	raw, err := m.Evaluate(ctx, c, first.TopFrames[0].ID, "count", true)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	var evaluated struct {
		Reference int `json:"variablesReference"`
	}
	if err = json.Unmarshal(raw, &evaluated); err != nil || evaluated.Reference <= 0 {
		t.Fatalf("evaluate did not hand out a reference: %s %v", raw, err)
	}
	if _, err = m.Inspect(ctx, c, "variables", evaluated.Reference, 0, 20); err != nil {
		t.Fatalf("fresh evaluate reference: %v", err)
	}

	c.Action = "step_over"
	if _, _, err = m.ControlAndWait(ctx, c, WaitOptions{Timeout: 5 * time.Second}); err != nil {
		t.Fatal(err)
	}
	// The adapter reuses the same number at this stop; the Manager must still
	// reject the reference it minted at the previous one.
	if raw, err = m.Inspect(ctx, c, "variables", evaluated.Reference, 0, 20); !errors.Is(err, ErrStaleReference) {
		t.Fatalf("previous stop's evaluate reference %d = %s, %v; want ErrStaleReference", evaluated.Reference, raw, err)
	}
}
