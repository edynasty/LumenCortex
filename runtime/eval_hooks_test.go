package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
)

type denyHostEval struct{}

func (denyHostEval) ID() string                       { return "deny-host-eval" }
func (denyHostEval) Version() int                     { return 1 }
func (denyHostEval) Events() []LifecycleEvent         { return []LifecycleEvent{LifecyclePreEval} }
func (denyHostEval) Priority() int                    { return 0 }
func (denyHostEval) Timeout() time.Duration           { return time.Second }
func (denyHostEval) FailurePolicy() HookFailurePolicy { return HookFailClosed }
func (denyHostEval) MutableArgs() bool                { return false }
func (denyHostEval) Safety() bool                     { return true }
func (denyHostEval) Handle(context.Context, LifecycleRequest) (LifecycleResult, error) {
	return LifecycleResult{Decision: HookDeny, Reason: "host_policy"}, nil
}

func TestHostEvalCannotBypassSafetyHook(t *testing.T) {
	e, owner := executionTestEngine(t)
	ctx := context.Background()
	kernel, err := e.CreateEvalSession(ctx, owner, executionRuntimeID(t, e, "python"))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.RegisterLifecycleHook(denyHostEval{}); err != nil {
		t.Fatal(err)
	}
	result, err := e.ExecuteEval(ctx, owner, evalapi.Request{SessionID: kernel.ID, CellID: "must-not-run", Code: "raise RuntimeError('cell executed despite deny')"})
	if err == nil && result.Succeeded() {
		t.Fatal("host direct execution bypassed configured deny")
	}
	records, err := e.EvalResults(ctx, owner, kernel.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if record.CellID == "must-not-run" {
			t.Fatalf("denied code reached worker: %#v", record)
		}
	}
}
