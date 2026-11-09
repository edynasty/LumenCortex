package runtime

import (
	"context"
	"regexp"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
)

var runIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Every Start draws a fresh 128-bit run id after Reconcile and hands it to the loop; it is not a
// session column.
func TestPrepareRunDrawsAFreshRunIDAfterReconcile(t *testing.T) {
	ctx := context.Background()
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "run identity")

	// A request a previous process left admitted: prepareRun must settle it before the run starts.
	if _, err := engine.store.AdmitRequest(ctx, session.RequestAdmission{
		RequestID: "left-over", SessionID: handle.ID, Purpose: session.PurposeAutoEffort, LineageComplete: true,
	}); err != nil {
		t.Fatal(err)
	}

	opts := AgentOptions{ModelRef: bind(engine, completingProvider("ok")), MaxSteps: 2}
	ctx, finishFirst := engine.trackRun(ctx, handle.ID)
	defer func() {
		if finishFirst != nil {
			finishFirst()
		}
	}()
	first, err := engine.prepareRun(ctx, handle.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !runIDPattern.MatchString(first.loopOpts.RunID) {
		t.Fatalf("RunID %q is not 128 bits of lower-case hex", first.loopOpts.RunID)
	}
	left, err := engine.store.Request(ctx, "left-over")
	if err != nil || left.State != session.RequestUnknown {
		t.Fatalf("left-over request after prepareRun: %+v (%v), want unknown", left, err)
	}
	finishFirst()
	finishFirst = nil

	secondCtx, finishSecond := engine.trackRun(context.Background(), handle.ID)
	defer finishSecond()
	second, err := engine.prepareRun(secondCtx, handle.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.loopOpts.RunID == second.loopOpts.RunID {
		t.Fatalf("two Starts share the run id %q", first.loopOpts.RunID)
	}

}

// A Start that fails at Reconcile (no such session) builds no run.
func TestPrepareRunOfAMissingSessionFailsBeforeAnyRun(t *testing.T) {
	engine := openTestEngine(t)
	if run, err := engine.prepareRun(context.Background(), "ghost", AgentOptions{}); err == nil || run != nil {
		t.Fatalf("prepareRun of a missing session = %v, %v", run, err)
	}
}
