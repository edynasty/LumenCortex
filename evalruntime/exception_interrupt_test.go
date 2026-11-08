package evalruntime

import (
	"github.com/edynasty/LumenCortex/evalapi"
	"testing"
)

func TestInterruptDuringExceptionStringRetainsKernel(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	s := h.create("python", "owner", "owner")
	h.expectCompleted(s, "owner", "kept = 79")
	done := startBusyCell(t, h, s, "exception-string", `class SlowError(Exception):
    def __str__(self):
        while True: pass
raise SlowError()`)
	out := interruptTarget(t, h, s.ID, evalapi.InterruptTarget{RequestID: "exception-string"})
	result := awaitResult(t, done)
	if out.StateStatus != evalapi.StateStatusOK || result.Status != evalapi.ResultStatusCancelled {
		t.Fatalf("exception string interrupt lost kernel: %+v %+v", out, result)
	}
	r := h.expectCompleted(s, "owner", "kept")
	if len(r.Displays) != 1 || r.Displays[0].Text != "79" {
		t.Fatalf("kernel state: %+v", r)
	}
}
