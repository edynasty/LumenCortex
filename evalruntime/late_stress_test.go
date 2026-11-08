package evalruntime

import (
	"context"
	"github.com/edynasty/LumenCortex/evalapi"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLateTargetStressDoesNotSignalSuccessor(t *testing.T) {
	rounds := 40
	if n, err := strconv.Atoi(os.Getenv("LC_LATE_ROUNDS")); err == nil && n > 0 {
		rounds = n
	}
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	first := h.execRequest(s, "owner", evalapi.Request{ID: "old", Code: "kept = 17"}, nil)
	if !first.Succeeded() {
		t.Fatal(first)
	}
	done := startBusyCell(t, h, s, "successor", `import os, time
while not os.path.exists('release'): time.sleep(.001)
kept += 1
kept`)
	for i := 0; i < rounds; i++ {
		out, err := h.m.InterruptExecution(context.Background(), s.ID, "owner", evalapi.InterruptTarget{RequestID: "old"})
		if err != nil || out.Status != evalapi.InterruptTargetFinished || out.ActiveRequestID != "successor" {
			t.Fatalf("round %d: %+v %v", i, out, err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.Workspace, "work", "release"), []byte("go"), 0600); err != nil {
		t.Fatal(err)
	}
	r := awaitResult(t, done)
	if !r.Succeeded() || r.StateStatus != evalapi.StateStatusOK || len(r.Displays) != 1 || r.Displays[0].Text != "18" {
		t.Fatalf("late target reached successor: %+v", r)
	}
	t.Logf("late target calls=%d; successor completed with retained heap", rounds)
}
