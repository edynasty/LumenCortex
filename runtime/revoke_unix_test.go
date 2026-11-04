//go:build unix

package runtime

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// Plan T3-S1, two OS processes: a worker process runs a session that had read the source with
// read_thread; this process revokes the source while the worker admits requests and tool calls. By
// SQLite commit order: the revocation reaches the worker's session through the stored edge; every
// request that existed at the commit is in the report exactly once (answered counted, admitted listed
// in flight, unknown listed); at most one request carries the fence; no execution was admitted after
// the commit; the worker's run ends fenced; a worker on an unrelated session completes.
func TestCrossProcessRevocationIsLinearizedWithAdmission(t *testing.T) {
	workspace := xpWorkspace(t)
	engine := openEngineAt(t, workspace)
	db := stateDB(t, engine)
	ctx := context.Background()
	g := newTestSession(t, engine, "unaffected")
	other := xpStart(t, xpEnv("worker", workspace, g.ID, "g", 300)...)
	for trial := 0; trial < 4; trial++ {
		x := newTestSession(t, engine, fmt.Sprintf("the source %d", trial))
		s := newTestSession(t, engine, fmt.Sprintf("the reader %d", trial))
		seed := &threadProvider{responses: []protocol.ProviderResponse{toolCalls(tc("r1", "read_thread", `{"id":"`+x.ID+`"}`))}}
		if r, err := engine.runAgentWith(ctx, s.ID, seed, AgentOptions{Input: "read", MaxSteps: 3}); err != nil || r.Status != "completed" {
			t.Fatalf("trial %d: seed run %+v %v", trial, r, err)
		}
		var seeded int
		if err := db.QueryRow(`SELECT count(*) FROM request_envelopes WHERE session_id = ?`, s.ID).Scan(&seeded); err != nil {
			t.Fatal(err)
		}
		pause := "0"
		if trial%2 == 0 {
			pause = "10"
		}
		worker := xpStart(t, append(xpEnv("worker", workspace, s.ID, fmt.Sprintf("s%d", trial), 300), xpPauseEnv+"="+pause)...)
		deadline := time.Now().Add(time.Minute)
		for {
			var n int
			if err := db.QueryRow(`SELECT count(*) FROM request_envelopes WHERE session_id = ?`, s.ID).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n >= seeded+4+trial {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("trial %d: the worker made no progress", trial)
			}
			time.Sleep(time.Millisecond)
		}
		rep, err := engine.RevokeSource(ctx, SourceSession, x.ID, "user_revoked")
		if err != nil {
			t.Fatal(err)
		}
		rw := worker.wait(t, 2*time.Minute)
		if rw.Status == "completed" {
			t.Fatalf("trial %d: the worker completed after the revocation: %+v", trial, rw)
		}
		if _, cause := sessionState(t, engine, s.ID); cause == nil || cause.Kind != "fenced" {
			t.Fatalf("trial %d: worker session cause %+v", trial, cause)
		}
		if !slices.Equal(rep.Sessions, []string{x.ID, s.ID}) {
			t.Fatalf("trial %d: report sessions %v, want the source and its reader", trial, rep.Sessions)
		}

		rows, err := db.Query(`SELECT request_id, state, COALESCE(reason, '') FROM request_envelopes WHERE session_id IN (?, ?)`, x.ID, s.ID)
		if err != nil {
			t.Fatal(err)
		}
		existing, fenced := 0, 0
		ids := map[string]bool{}
		for rows.Next() {
			var id, state, reason string
			if err := rows.Scan(&id, &state, &reason); err != nil {
				t.Fatal(err)
			}
			if state == "failed" && reason == "fenced" {
				fenced++ // refused by the fence: created at or after the commit
				continue
			}
			existing++
			ids[id] = true
		}
		rows.Close()
		if fenced > 1 {
			t.Fatalf("trial %d: %d requests carry the fence, want at most one", trial, fenced)
		}
		// Every request that existed at the commit is in the report once; no other one was admitted.
		if got := rep.Answered + rep.InFlightCount + rep.UnknownCount; got != existing {
			t.Fatalf("trial %d: report covers %d requests (answered %d, in flight %v, unknown %v), %d exist", trial, got, rep.Answered, rep.InFlight, rep.Unknown, existing)
		}
		for _, id := range append(append([]string(nil), rep.InFlight...), rep.Unknown...) {
			if !ids[id] {
				t.Fatalf("trial %d: report names %s, which is not a request of the revoked sessions", trial, id)
			}
		}
		if len(rep.InFlight) > 1 {
			t.Fatalf("trial %d: %d requests in flight in one run", trial, len(rep.InFlight))
		}
		// The report read at the revocation may show the request still admitted: its run is in another
		// process. Read again after that run ended, the request was settled unknown, never answered.
		after, err := engine.ExposureReport(ctx, rep.Epoch)
		if err != nil || !slices.Equal(after.InFlight, rep.InFlight) {
			t.Fatalf("trial %d: report read back %+v %v", trial, after.Revocation, err)
		}
		for _, id := range after.InFlight {
			if st := after.InFlightNow[id]; st != "unknown" {
				t.Fatalf("trial %d: in-flight request %s is %q after the run ended", trial, id, st)
			}
		}
		// An execution row exists only if its admission committed, and none committed after the revocation.
		var late int
		if err := db.QueryRow(`SELECT count(*) FROM derived_origins WHERE session_id = ? AND kind = 'execution' AND created_at > ?`,
			s.ID, rep.At.UTC().Format(time.RFC3339Nano)).Scan(&late); err != nil {
			t.Fatal(err)
		}
		if late != 0 {
			t.Fatalf("trial %d: %d executions admitted after the revocation", trial, late)
		}
		t.Logf("trial %d: in flight %d, unknown %d, answered %d, refused %d, executions %d", trial, rep.InFlightCount, rep.UnknownCount, rep.Answered, fenced, rep.ExecutionCount)
	}
	if r := other.wait(t, 3*time.Minute); r.Status != "completed" {
		t.Fatalf("the unrelated worker: %+v", r)
	}
}
