package runtime

import (
	"context"
	"database/sql"
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

	second, err := engine.prepareRun(ctx, handle.ID, opts)
	if err != nil {
		t.Fatal(err)
	}
	if first.loopOpts.RunID == second.loopOpts.RunID {
		t.Fatalf("two Starts share the run id %q", first.loopOpts.RunID)
	}

	side, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer side.Close()
	var n int
	if err := side.QueryRow(`SELECT count(*) FROM pragma_table_info('sessions') WHERE lower(name) LIKE '%run%'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("sessions has %d run-related columns; the run id is not a session field", n)
	}
}

// A Start that fails at Reconcile (no such session) builds no run.
func TestPrepareRunOfAMissingSessionFailsBeforeAnyRun(t *testing.T) {
	engine := openTestEngine(t)
	if run, err := engine.prepareRun(context.Background(), "ghost", AgentOptions{}); err == nil || run != nil {
		t.Fatalf("prepareRun of a missing session = %v, %v", run, err)
	}
}
