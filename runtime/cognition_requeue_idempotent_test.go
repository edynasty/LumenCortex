package runtime

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// REVIEW-F02-01: queueing a co-batched source again is idempotent. Each fence that reaches a batch's
// maintenance session used to add one more dirty row for every source the batch lost its cognition
// for, so a second revocation in the same batch, or the same revocation repeated, queued a source that
// was already waiting. One version of a source now waits on one row, whatever the number of fences;
// history (candidates, processing records) is kept as written, and a source that really needs another
// pass - a new version, or a recovery batch that is fenced in its turn - is still queued.

// pending is the number of dirty rows waiting for source id.
func (f *cognitionFixture) pending(t *testing.T, id string) int {
	t.Helper()
	return countRows(t, f.engine, `SELECT count(*) FROM maintenance_sources WHERE session_id = ?`, id)
}

func (f *cognitionFixture) revoke(t *testing.T, id string) {
	t.Helper()
	if _, err := f.engine.RevokeSource(context.Background(), SourceSession, id, "user_withdrawal"); err != nil {
		t.Fatal(err)
	}
}

// requeueBatch is one committed batch of three sources: g and x with a free-text candidate each and h
// with the supported port fact.
func requeueBatch(t *testing.T) (f *cognitionFixture, g, x, h string, batch MaintenanceReport) {
	t.Helper()
	f = newCognitionFixture(t, true)
	g = f.run(t, "find the build command", cogAnswer("make build"))
	x = f.run(t, "find the test command", cogAnswer("make test"))
	h = f.run(t, "find the service port", cogAnswer("8080"))
	f.propose(func(refs []string) []string {
		return []string{
			candidateJSON("procedure", "build", "Build with make build.", refOf(t, refs, g), nil),
			candidateJSON("procedure", "test", "Test with make test.", refOf(t, refs, x), nil),
			factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080")),
		}
	})
	batch = f.maintain(t)
	if batch.Sources != 3 || len(batch.Inserted) != 3 || batch.Qualified != 1 {
		t.Fatalf("batch %+v", batch)
	}
	return f, g, x, h, batch
}

func (f *cognitionFixture) proposePort(t *testing.T, h string) {
	t.Helper()
	f.propose(func(refs []string) []string {
		if len(refs) != 1 {
			t.Errorf("the recovery batch holds %v, want only the unrevoked source", refs)
		}
		return []string{factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080"))}
	})
}

func TestFableRequeueOfCoBatchedSourceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	t.Run("fences of one batch queue a waiting source once", func(t *testing.T) {
		f, g, x, h, batch := requeueBatch(t)
		old := f.candidate(t, batch.Inserted[2])
		f.revoke(t, g)
		if f.pending(t, h) != 1 || f.pending(t, x) != 1 {
			t.Fatalf("after the first revocation: h %d, x %d rows", f.pending(t, h), f.pending(t, x))
		}
		waiting := f.state(t)
		// Another source of the same batch, then both again: h is already waiting at this version.
		for _, id := range []string{x, g, x, g} {
			f.revoke(t, id)
			if n := f.pending(t, h); n != 1 {
				t.Fatalf("revoking %s queued the waiting source again: %d rows", id, n)
			}
			if st := f.state(t); st.Dirty != waiting.Dirty || st.Cursor != waiting.Cursor || st.Backlog != waiting.Backlog {
				t.Fatalf("a repeated fence changed the queue: %+v -> %+v", waiting, st)
			}
		}
		f.proposePort(t, h)
		recovered := f.maintain(t)
		if recovered.Sources != 1 || recovered.Qualified != 1 || recovered.Duplicates != 0 || len(recovered.Inserted) != 1 {
			t.Fatalf("the recovery pass %+v", recovered)
		}
		if st := f.state(t); st.Backlog != 0 || st.Dirty != st.Cursor {
			t.Fatalf("the queue after the recovery %+v", st)
		}
		fresh := f.candidate(t, recovered.Inserted[0])
		if fresh.SupportDigest != old.SupportDigest || fresh.MaintenanceSessionID == old.MaintenanceSessionID {
			t.Fatalf("the recovered candidate %+v (withdrawn %+v)", fresh, old)
		}
		if v := f.hostView(t, old.ID); v.Applicability != knowledge.Withdrawn || v.Claim != "" {
			t.Fatalf("the withdrawn candidate %+v", v)
		}
		if rec := f.candidate(t, old.ID); rec.Status != old.Status || rec.Claim != old.Claim || rec.SupportDigest != old.SupportDigest {
			t.Fatalf("the withdrawn candidate was rewritten %+v", rec)
		}
		want := []string{"candidates@" + batch.MaintenanceSessionID, "candidates@" + recovered.MaintenanceSessionID}
		if got := f.dedupeRows(t, h); !slices.Equal(got, want) {
			t.Fatalf("processing records of the recovered source %v, want %v", got, want)
		}
		// The recovery stands: no later fence of the first batch queues the source.
		for _, id := range []string{g, x} {
			f.revoke(t, id)
		}
		if st := f.state(t); st.Backlog != 0 || f.pending(t, h) != 0 {
			t.Fatalf("a fence after the recovery queued the source: %+v", st)
		}
		if v := f.readOnce(t, fresh.ID); v.Applicability != knowledge.Current || v.Claim != portFact("8080").Render() {
			t.Fatalf("the recovered fact to a reader %+v", v)
		}
		f.supportedOnly(t)
	})
	t.Run("a fenced recovery batch queues the source again, once", func(t *testing.T) {
		f, g, _, h, batch := requeueBatch(t)
		f.revoke(t, g)
		y := f.run(t, "find the lint command", cogAnswer("make lint"))
		f.propose(func(refs []string) []string {
			return []string{
				factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080")),
				candidateJSON("procedure", "lint", "Lint with make lint.", refOf(t, refs, y), nil),
			}
		})
		second := f.maintain(t)
		if second.Sources != 3 || second.Qualified != 1 || len(second.Inserted) != 2 {
			t.Fatalf("the second batch %+v", second)
		}
		for range 3 {
			f.revoke(t, y)
			if n := f.pending(t, h); n != 1 {
				t.Fatalf("the fenced recovery batch left %d rows for its other source", n)
			}
		}
		// The fence of the second batch reaches both of its other sources; only h is proposed for.
		f.answer = func(refs []string) string {
			var out []string
			for _, r := range refs {
				if strings.Contains(r, ":"+h+"@") {
					out = append(out, factJSON("service port", []string{r}, nil, portFact("8080")))
				}
			}
			return candidates(out...)
		}
		third := f.maintain(t)
		if third.Qualified != 1 || third.Duplicates != 0 {
			t.Fatalf("the third batch %+v", third)
		}
		got := f.dedupeRows(t, h)
		if len(got) != 3 || got[0] != "candidates@"+batch.MaintenanceSessionID || got[1] != "candidates@"+second.MaintenanceSessionID || got[2] != "candidates@"+third.MaintenanceSessionID {
			t.Fatalf("processing records %v", got)
		}
		if st := f.state(t); st.Backlog != 0 {
			t.Fatalf("the queue after the third batch %+v", st)
		}
		f.supportedOnly(t)
	})
	t.Run("a new version of a waiting source is queued and maintained", func(t *testing.T) {
		f, g, x, h, _ := requeueBatch(t)
		f.revoke(t, g)
		old, err := f.engine.store.SourceVersion(ctx, h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.runAgentWith(ctx, h, cogAnswer("still 8080"), AgentOptions{MaxSteps: 2, Policy: "workspace", Input: "check again"}); err != nil {
			t.Fatal(err)
		}
		rows := f.pending(t, h)
		if rows < 2 {
			t.Fatalf("the new version of a waiting source was not queued: %d rows", rows)
		}
		f.revoke(t, x)
		if n := f.pending(t, h); n != rows {
			t.Fatalf("a fence queued the old version of a changed source: %d -> %d rows", rows, n)
		}
		var seen []string
		f.answer = func(refs []string) string {
			seen = append(seen, refs...)
			return candidates(factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080")))
		}
		r := f.maintain(t)
		if r.Sources != 1 || r.Qualified != 1 || len(seen) != 1 || strings.HasSuffix(seen[0], "@"+old) {
			t.Fatalf("the pass of the new version %+v read %v (old version %s)", r, seen, old)
		}
		if st := f.state(t); st.Backlog != 0 {
			t.Fatalf("the queue after the new version %+v", st)
		}
	})
	// A pass in flight read its batch before the fence: a row it passed over as processed goes with its
	// commit. The source must still be waiting afterwards, on one row past that batch.
	t.Run("a pass in flight does not consume the row a fence relies on", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		g := f.run(t, "find the build command", cogAnswer("make build"))
		h := f.run(t, "find the service port", cogAnswer("8080"))
		f.propose(func(refs []string) []string {
			return []string{
				candidateJSON("procedure", "build", "Build with make build.", refOf(t, refs, g), nil),
				factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080")),
			}
		})
		if batch := f.maintain(t); batch.Sources != 2 || batch.Qualified != 1 {
			t.Fatalf("batch %+v", batch)
		}
		// h reaches its terminal state again: the same version on a new row, which a pass skips.
		completed := "completed"
		if err := f.engine.store.Update(ctx, h, session.Patch{Status: &completed}); err != nil {
			t.Fatal(err)
		}
		z := f.run(t, "find the lint command", cogAnswer("make lint"))
		if f.pending(t, h) != 1 || f.pending(t, z) != 1 {
			t.Fatalf("before the pass: h %d, z %d rows", f.pending(t, h), f.pending(t, z))
		}
		inFlight := 0
		f.propose(func(refs []string) []string {
			if len(refs) != 1 {
				t.Errorf("the pass in flight holds %v, want only the new source", refs)
			}
			for range 2 {
				f.revoke(t, g)
				if n := f.pending(t, h); n != 1 {
					t.Errorf("a fence during a pass left %d rows for the source", n)
				}
			}
			inFlight++
			return []string{candidateJSON("procedure", "lint", "Lint with make lint.", refOf(t, refs, z), nil)}
		})
		if r := f.maintain(t); r.Sources != 1 || len(r.Inserted) != 1 || inFlight != 1 {
			t.Fatalf("the pass in flight %+v (%d)", r, inFlight)
		}
		if st := f.state(t); st.Backlog != 1 || f.pending(t, h) != 1 {
			t.Fatalf("the pass in flight consumed the source's row: %+v, %d rows", st, f.pending(t, h))
		}
		f.proposePort(t, h)
		if r := f.maintain(t); r.Sources != 1 || r.Qualified != 1 || r.Duplicates != 0 {
			t.Fatalf("the recovery pass %+v", r)
		}
		if st := f.state(t); st.Backlog != 0 {
			t.Fatalf("the queue after the recovery %+v", st)
		}
		f.supportedOnly(t)
	})
	t.Run("concurrent fences queue the source once", func(t *testing.T) {
		f, g, x, h, _ := requeueBatch(t)
		var wg sync.WaitGroup
		errs := make(chan error, 8)
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := f.engine.RevokeSource(ctx, SourceSession, []string{g, x}[i%2], "user_withdrawal")
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if n := f.pending(t, h); n != 1 {
			t.Fatalf("concurrent fences left %d rows for the source", n)
		}
		f.proposePort(t, h)
		if r := f.maintain(t); r.Sources != 1 || r.Qualified != 1 || r.Duplicates != 0 {
			t.Fatalf("the recovery pass %+v", r)
		}
	})
	t.Run("the waiting row survives a restart and is not doubled after it", func(t *testing.T) {
		f, g, x, h, _ := requeueBatch(t)
		f.revoke(t, g)
		if err := f.engine.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(Options{Workspace: f.root})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reopened.Close() })
		reopened.cognition = &CognitionOptions{Maintainer: bind(reopened, f.maintainer)}
		f.engine = reopened
		if n := f.pending(t, h); n != 1 {
			t.Fatalf("the restart left %d rows for the source", n)
		}
		f.revoke(t, x)
		f.revoke(t, g)
		if n := f.pending(t, h); n != 1 {
			t.Fatalf("a fence after the restart left %d rows for the source", n)
		}
		f.proposePort(t, h)
		if r := f.maintain(t); r.Sources != 1 || r.Qualified != 1 || r.Duplicates != 0 {
			t.Fatalf("the recovery pass after the restart %+v", r)
		}
		if st := f.state(t); st.Backlog != 0 {
			t.Fatalf("the queue after the recovery %+v", st)
		}
		f.supportedOnly(t)
	})
	t.Run("a fence that cannot queue the source is not written", func(t *testing.T) {
		f, g, _, h, _ := requeueBatch(t)
		db, err := sql.Open("sqlite3", "file:"+f.engine.store.Path()+"?_busy_timeout=5000")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if _, err := db.ExecContext(ctx, `ALTER TABLE maintenance_sources RENAME TO maintenance_sources_gone`); err != nil {
			t.Fatal(err)
		}
		if _, err := f.engine.RevokeSource(ctx, SourceSession, g, "user_withdrawal"); err == nil {
			t.Fatal("a revocation that could not queue the co-batched sources reported success")
		}
		if withdrawn, err := f.engine.store.SessionWithdrawn(ctx, g); err != nil || withdrawn {
			t.Fatalf("the failed revocation left a fence: %v %v", withdrawn, err)
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE maintenance_sources_gone RENAME TO maintenance_sources`); err != nil {
			t.Fatal(err)
		}
		f.revoke(t, g)
		f.revoke(t, g)
		if n := f.pending(t, h); n != 1 {
			t.Fatalf("the repeated revocation left %d rows for the source", n)
		}
	})
}
