package runtime

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// Review and remediation of T5a R2 (candidate f98ea7c): one regression test per finding of that review,
// end to end through the real loop, store, checks, approvals and git with scripted models.

// dedupeRows are the processing records of one source session, oldest first, as "outcome@maintenance".
func (f *cognitionFixture) dedupeRows(t *testing.T, sourceID string) []string {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+f.engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `SELECT outcome, maintenance_session_id FROM maintenance_dedupe
		WHERE source_ref LIKE ? ORDER BY created_at, rowid`, "%:"+sourceID+"@%")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var outcome, maintenance string
		if err := rows.Scan(&outcome, &maintenance); err != nil {
			t.Fatal(err)
		}
		out = append(out, outcome+"@"+maintenance)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// FABLE-01: unverified model output never changes what host-supported knowledge is to a reader. A
// candidate is contested only by a candidate with at least its level of host evidence: a fact whose
// bound check passed freshly for the reader stays current whatever unverified claims say about its
// subject, and the Start index and read_knowledge agree on it. The unverified claims read as a conflict.
// Once the fact's own check fails it is stale and contests nothing: the correction can be adopted.
func TestFableVerifiedFactIsNotContestedByUnverifiedClaims(t *testing.T) {
	t.Run("a claim whose bound check failed", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "inspect the service", cogAnswer("the service listens on 8080"))
		good := f.adoptFacts(t, a, portFact("8080"))
		b := f.run(t, "inspect the service again", cogAnswer("I think it is 9090"))
		bad := f.adoptFacts(t, b, portFact("9090"))
		if good.Qualified != 1 || bad.Qualified != 0 || bad.CandidateOnly != 1 || !slices.Equal(bad.Checks, []string{"port9090"}) {
			t.Fatalf("fixture: %+v %+v", good, bad)
		}
		verified, unverified := good.Inserted[0], bad.Inserted[0]
		p := scripted(readStep("r1", verified), readStep("r2", unverified))
		f.run(t, "use the project's cognition", p)
		index := startApplicability(p.request(t, 0), verified)
		view := viewIn(t, p.request(t, 1), "r1")
		if index != knowledge.Current || view.Applicability != knowledge.Current || view.Claim != portFact("8080").Render() {
			t.Errorf("the verified fact: index %s, read %s (%s) %q", index, view.Applicability, view.Reason, view.Claim)
		}
		if strings.Contains(indexRow(p.request(t, 0)), unverified) {
			t.Errorf("the index names an unverified candidate: %q", indexRow(p.request(t, 0)))
		}
		if v := viewIn(t, p.request(t, 2), "r2"); v.Applicability != knowledge.Conflict || v.Status != knowledge.StatusCandidateOnly {
			t.Errorf("the unverified contrary claim reads %s (%s)", v.Applicability, v.Reason)
		}
		// A host read is no run: nothing is fresh for it. The once-qualified fact is unknown there, and
		// still not contested by a claim no check ever supported.
		if v := f.hostView(t, verified); v.Applicability != knowledge.Unknown {
			t.Errorf("host read of the verified fact %s (%s)", v.Applicability, v.Reason)
		}
		if v := f.hostView(t, unverified); v.Applicability != knowledge.Conflict {
			t.Errorf("host read of the unverified claim %s (%s)", v.Applicability, v.Reason)
		}

		// The port changes. The fact's own check now fails for the next reader: it is stale, contests
		// nothing, and the claim it contradicted is a plain unverified candidate again.
		f.write(t, "defaults.env", "BASE=9090\n")
		q := scripted(readStep("r1", verified), readStep("r2", unverified))
		c := f.run(t, "use the project's cognition after the change", q)
		if v := viewIn(t, q.request(t, 1), "r1"); v.Applicability != knowledge.Stale {
			t.Errorf("the old fact after the change %s (%s)", v.Applicability, v.Reason)
		}
		if v := viewIn(t, q.request(t, 2), "r2"); v.Applicability != knowledge.CandidateOnly {
			t.Errorf("the unverified claim after the change %s (%s)", v.Applicability, v.Reason)
		}
		// The correction: a new source states 9090, the host-bound check of 9090 passes, and the next
		// reader inherits it as current while the old fact stays stale with its history.
		corrected := f.adoptFacts(t, c, portFact("9090"))
		if corrected.Qualified != 1 || len(corrected.Inserted) != 1 {
			t.Fatalf("the correction %+v", corrected)
		}
		r := scripted(readStep("r1", corrected.Inserted[0]), readStep("r2", verified))
		f.run(t, "use the corrected cognition", r)
		if index := startApplicability(r.request(t, 0), corrected.Inserted[0]); index != knowledge.Current {
			t.Errorf("the corrected fact in the index: %s (%q)", index, indexRow(r.request(t, 0)))
		}
		if v := viewIn(t, r.request(t, 1), "r1"); v.Applicability != knowledge.Current || v.Claim != portFact("9090").Render() {
			t.Errorf("the corrected fact %s (%s) %q", v.Applicability, v.Reason, v.Claim)
		}
		if v := viewIn(t, r.request(t, 2), "r2"); v.Applicability != knowledge.Stale {
			t.Errorf("the old fact after the correction %s (%s)", v.Applicability, v.Reason)
		}
		if obs := f.observations(t, verified); len(obs) < 3 {
			t.Errorf("the old fact lost observations: %+v", obs)
		}
		f.supportedOnly(t)
	})
	t.Run("claims no binding covers", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "inspect the service", cogAnswer("the service listens on 8080"))
		good := f.adoptFacts(t, a, portFact("8080"))
		b := f.run(t, "guess the port", cogAnswer("7070? 6060?"))
		// The same subject as the verified fact: an unbound value, and a free-text claim under the
		// subject key of the assertion itself.
		f.propose(func(refs []string) []string {
			rb := refOf(t, refs, b)
			return []string{
				factJSON("port", []string{rb}, nil, portFact("7070")),
				candidateJSON("fact", portFact("8080").Ref(), "The port is 6060.", rb, nil),
			}
		})
		bad := f.maintain(t)
		if good.Qualified != 1 || bad.Qualified != 0 || bad.CandidateOnly != 2 || len(bad.Checks) != 0 {
			t.Fatalf("fixture: %+v %+v", good, bad)
		}
		verified := good.Inserted[0]
		p := scripted(readStep("r1", verified), readStep("r2", bad.Inserted[0]), readStep("r3", bad.Inserted[1]))
		f.run(t, "use the project's cognition", p)
		index := startApplicability(p.request(t, 0), verified)
		if view := viewIn(t, p.request(t, 1), "r1"); index != knowledge.Current || view.Applicability != knowledge.Current {
			t.Errorf("the verified fact: index %s, read %s (%s)", index, view.Applicability, view.Reason)
		}
		for i, call := range []string{"r2", "r3"} {
			if v := viewIn(t, p.request(t, i+2), call); v.Applicability != knowledge.Conflict {
				t.Errorf("unverified claim %s reads %s (%s)", bad.Inserted[i], v.Applicability, v.Reason)
			}
		}
		f.supportedOnly(t)
	})
}

// FABLE-02: a revocation or a privacy deletion that reaches a maintenance session withdraws everything
// that session proposed, because its answer is not separable from the source it lost. The other sources
// of its batch are not lost with it: each one the fence does not reach, still at the version the batch
// read, is queued again in the transaction of the fence and maintained by the next pass, alone. The
// withdrawn candidates and the earlier processing records stay as they are.
func TestFableCoBatchedSourceRecoversAfterWithdrawal(t *testing.T) {
	ctx := context.Background()
	t.Run("a revoked source of the batch", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		g := f.run(t, "find the build command", cogAnswer("make build"))
		h := f.run(t, "find the service port", cogAnswer("8080"))
		gone := f.run(t, "a source deleted before the revocation", cogAnswer("nothing"))
		f.propose(func(refs []string) []string {
			return []string{
				candidateJSON("procedure", "build", "Build with make build.", refOf(t, refs, g), nil),
				factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080")),
			}
		})
		batch := f.maintain(t)
		if batch.Sources != 3 || len(batch.Inserted) != 2 || batch.Qualified != 1 {
			t.Fatalf("batch %+v", batch)
		}
		if _, err := f.engine.DeleteSession(ctx, gone, WorktreeCleanup{}); err != nil {
			t.Fatal(err)
		}
		// The deletion reached the batch: its candidates are purged tombstones, and g and h, which the
		// deletion does not cover, are queued again by it. That pass is drained first, so that the
		// revocation below fences a batch whose candidates are withdrawn, not purged.
		if st := f.state(t); st.Backlog != 2 {
			t.Fatalf("after deleting one source of the batch, the others are not queued: %+v", st)
		}
		again := f.maintain(t)
		if again.Sources != 2 || again.Qualified != 1 || again.Duplicates != 0 || len(again.Inserted) != 2 {
			t.Fatalf("the pass after the deletion %+v", again)
		}
		old := f.candidate(t, again.Inserted[1])
		if _, err := f.engine.RevokeSource(ctx, SourceSession, g, "user_withdrawal"); err != nil {
			t.Fatal(err)
		}
		for _, id := range append(slices.Clone(batch.Inserted), again.Inserted...) {
			if v := f.hostView(t, id); v.Applicability != knowledge.Withdrawn || v.Claim != "" {
				t.Fatalf("candidate %s of a batch that read the revoked source %+v", id, v)
			}
		}
		st := f.state(t)
		if st.Backlog != 1 || st.Dirty <= st.Cursor {
			t.Fatalf("the unrevoked source is not queued again: %+v", st)
		}
		f.propose(func(refs []string) []string {
			if len(refs) != 1 {
				t.Errorf("the recovery batch holds %v, want only the unrevoked source", refs)
			}
			return []string{factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080"))}
		})
		recovered := f.maintain(t)
		if recovered.Sources != 1 || recovered.Qualified != 1 || recovered.Duplicates != 0 || len(recovered.Inserted) != 1 {
			t.Fatalf("the recovery pass %+v", recovered)
		}
		for _, input := range f.batchInputs()[2:] {
			if strings.Contains(input, g) || strings.Contains(input, "make build") {
				t.Fatalf("the recovery batch holds the revoked source: %.300q", input)
			}
		}
		fresh := f.candidate(t, recovered.Inserted[0])
		if fresh.ClaimDigest != old.ClaimDigest || fresh.SupportDigest != old.SupportDigest || fresh.MaintenanceSessionID == old.MaintenanceSessionID {
			t.Fatalf("the recovered candidate %+v (withdrawn %+v)", fresh, old)
		}
		if v := f.readOnce(t, fresh.ID); v.Applicability != knowledge.Current || v.Claim != portFact("8080").Render() {
			t.Fatalf("the recovered fact to a reader %+v", v)
		}
		// History: the withdrawn candidate and every processing record of h are kept as written.
		if rec := f.candidate(t, old.ID); rec.Status != old.Status || rec.Claim != old.Claim || rec.SupportDigest != old.SupportDigest {
			t.Fatalf("the withdrawn candidate was rewritten %+v", rec)
		}
		want := []string{"candidates@" + batch.MaintenanceSessionID, "candidates@" + again.MaintenanceSessionID, "candidates@" + recovered.MaintenanceSessionID}
		if got := f.dedupeRows(t, h); !slices.Equal(got, want) {
			t.Fatalf("processing records of the recovered source %v, want %v", got, want)
		}
		if got := f.dedupeRows(t, g); len(got) != 2 {
			t.Fatalf("processing records of the revoked source %v", got)
		}
		// Nothing loops: the recovery stands, so neither an idle pass nor another fence on the same
		// maintenance sessions queues the source again. The reader of the recovered fact is one source.
		f.answer = func([]string) string { return `{"candidates":[]}` }
		if r := f.maintain(t); r.Sources != 1 {
			t.Fatalf("the pass after the recovery %+v", r)
		}
		if _, err := f.engine.RevokeSource(ctx, SourceSession, g, "user_withdrawal"); err != nil {
			t.Fatal(err)
		}
		if st := f.state(t); st.Backlog != 0 || st.Dirty != st.Cursor {
			t.Fatalf("a repeated revocation queued a recovered source again: %+v", st)
		}
		if v := f.hostView(t, fresh.ID); v.Applicability == knowledge.Withdrawn {
			t.Fatalf("the recovered fact was withdrawn by a repeated revocation: %+v", v)
		}
		f.supportedOnly(t)
	})
	t.Run("a source changed since the batch is maintained at its new version only", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		g := f.run(t, "find the build command", cogAnswer("make build"))
		h := f.run(t, "find the service port", cogAnswer("8080"))
		f.propose(func(refs []string) []string {
			return []string{factJSON("service port", []string{refOf(t, refs, h)}, nil, portFact("8080"))}
		})
		if batch := f.maintain(t); batch.Sources != 2 || batch.Qualified != 1 {
			t.Fatalf("batch %+v", batch)
		}
		// A new run in h: its new version has its own dirty row.
		if _, err := f.engine.runAgentWith(ctx, h, cogAnswer("still 8080"), AgentOptions{MaxSteps: 2, Policy: "workspace", Input: "check again"}); err != nil {
			t.Fatal(err)
		}
		before := f.state(t)
		if _, err := f.engine.RevokeSource(ctx, SourceSession, g, "user_withdrawal"); err != nil {
			t.Fatal(err)
		}
		if st := f.state(t); st.Backlog != before.Backlog || st.Dirty != before.Dirty {
			t.Fatalf("the old version of a changed source was queued: %+v -> %+v", before, st)
		}
		if r := f.maintain(t); r.Sources != 1 || r.Qualified != 1 || r.Duplicates != 0 {
			t.Fatalf("the pass of the new version %+v", r)
		}
	})
	t.Run("a reader of a withdrawn candidate stays withdrawn, its batch neighbour recovers", func(t *testing.T) {
		f := newCognitionFixture(t, true)
		a := f.run(t, "find the port", cogAnswer("8080"))
		first := f.adoptFacts(t, a, portFact("8080"))
		reader := f.readOnceIn(t, first.Inserted[0])
		x := f.run(t, "find the test command", cogAnswer("make test"))
		f.propose(func(refs []string) []string {
			return []string{candidateJSON("procedure", "test", "Test with make test.", refOf(t, refs, x), nil)}
		})
		second := f.maintain(t)
		if second.Sources != 2 || len(second.Inserted) != 1 {
			t.Fatalf("the batch of the reader and its neighbour %+v", second)
		}
		rev, err := f.engine.RevokeSource(ctx, SourceSession, a, "user_withdrawal")
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(rev.Sessions, reader) || !slices.Contains(rev.Sessions, second.MaintenanceSessionID) || slices.Contains(rev.Sessions, x) {
			t.Fatalf("revocation reached %v (reader %s, batch %s, neighbour %s)", rev.Sessions, reader, second.MaintenanceSessionID, x)
		}
		if v := f.hostView(t, second.Inserted[0]); v.Applicability != knowledge.Withdrawn {
			t.Fatalf("the neighbour's candidate of the fenced batch %+v", v)
		}
		recovered := f.maintain(t)
		if recovered.Sources != 1 || len(recovered.Inserted) != 1 || recovered.Duplicates != 0 {
			t.Fatalf("the recovery pass %+v", recovered)
		}
		if v := f.hostView(t, recovered.Inserted[0]); v.Applicability != knowledge.CandidateOnly || v.Claim != "Test with make test." {
			t.Fatalf("the neighbour's recovered candidate %+v", v)
		}
		if got := f.dedupeRows(t, reader); len(got) != 1 {
			t.Fatalf("the fenced reader was processed again: %v", got)
		}
	})
	t.Run("a privacy deletion", func(t *testing.T) {
		w := newPrivacyWorld(t)
		f := w.f
		if _, err := f.engine.DeleteSession(ctx, w.a, WorktreeCleanup{}); err != nil {
			t.Fatal(err)
		}
		w.assertPurged(t)
		// B was read together with the deleted A: its candidate is purged with the batch, and B itself,
		// which the deletion does not cover, is queued again. The reader of A's candidate is fenced.
		st := f.state(t)
		if st.Backlog != 1 {
			t.Fatalf("after the deletion %+v", st)
		}
		inputs := len(f.batchInputs())
		f.propose(func(refs []string) []string {
			if len(refs) != 1 {
				t.Errorf("the recovery batch holds %v, want only B", refs)
			}
			return []string{candidateJSON("procedure", "restart", "Restart the service after a configuration change.", refOf(t, refs, w.b), nil)}
		})
		recovered := f.maintain(t)
		if recovered.Sources != 1 || len(recovered.Inserted) != 1 || recovered.Duplicates != 0 {
			t.Fatalf("the recovery pass %+v", recovered)
		}
		for _, input := range f.batchInputs()[inputs:] {
			if strings.Contains(input, w.sentinel) || strings.Contains(input, w.a) {
				t.Fatalf("the recovery batch holds deleted content: %.300q", input)
			}
		}
		if v := f.hostView(t, recovered.Inserted[0]); v.Applicability != knowledge.CandidateOnly || v.Claim != "Restart the service after a configuration change." {
			t.Fatalf("B's recovered candidate %+v", v)
		}
		if found := textAtRest(t, f.engine.store.Path(), w.sentinel); len(found) != 0 {
			t.Fatalf("the sentinel is back in %v", found)
		}
		w.readsBlocked(t)
		if rec := f.candidate(t, w.bOnly); rec.PurgedAt.IsZero() || rec.Claim != "" {
			t.Fatalf("the purged tombstone was rewritten %+v", rec)
		}
		if got := f.dedupeRows(t, w.b); len(got) != 2 || got[0] != "candidates@"+w.m || got[1] != "candidates@"+recovered.MaintenanceSessionID {
			t.Fatalf("processing records of B %v", got)
		}
	})
	t.Run("an interrupted privacy deletion queues the source once", func(t *testing.T) {
		w := newPrivacyWorld(t)
		f := w.f
		cctx, cancel := context.WithCancel(ctx)
		testHookPurgeStep = cancel
		t.Cleanup(func() { testHookPurgeStep = nil })
		if _, err := f.engine.DeleteSession(cctx, w.a, WorktreeCleanup{}); err == nil {
			t.Fatal("the interrupted deletion reported success")
		}
		testHookPurgeStep = nil
		if _, err := f.engine.DeleteSession(ctx, w.a, WorktreeCleanup{}); err != nil {
			t.Fatalf("resumed delete: %v", err)
		}
		w.assertPurged(t)
		if st := f.state(t); st.Backlog != 1 {
			t.Fatalf("after the resumed deletion %+v", st)
		}
		if n := countRows(t, f.engine, `SELECT count(*) FROM maintenance_sources WHERE session_id = ?`, w.b); n != 1 {
			t.Fatalf("B is queued %d times", n)
		}
	})
}

// readOnceIn reads candidate id in a new run and returns that run's session.
func (f *cognitionFixture) readOnceIn(t *testing.T, id string) string {
	t.Helper()
	p := scripted(readStep("r1", id))
	sessionID := f.run(t, "use the project's cognition", p)
	if v := viewIn(t, p.request(t, 1), "r1"); v.Claim == "" {
		t.Fatalf("the reader did not receive the claim of %s: %+v", id, v)
	}
	return sessionID
}
