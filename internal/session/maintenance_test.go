package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

const testPolicy = "test-policy"

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func maintenanceStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.EnsureMaintenanceState(context.Background(), "project", testPolicy, t0); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func finish(t *testing.T, s *Store, id, status string) {
	t.Helper()
	if err := s.Update(context.Background(), id, Patch{Status: &status}); err != nil {
		t.Fatal(err)
	}
}

func createSession(t *testing.T, s *Store, id string, metadata string) {
	t.Helper()
	if metadata == "" {
		metadata = "{}"
	}
	if err := s.Create(context.Background(), Session{ID: id, Goal: "goal of " + id, Metadata: json.RawMessage(metadata)}); err != nil {
		t.Fatal(err)
	}
}

func state(t *testing.T, s *Store) MaintenanceState {
	t.Helper()
	st, err := s.ReadMaintenanceState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The dirty revision advances only for a terminal top-level session that is not Core's maintenance
// session, and only with the maintenance state on.
func TestTerminalStatusAdvancesDirtyRevisionForSourcesOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	createSession(t, s, "parent", "")
	createSession(t, s, "child", "")
	if err := s.AddRelation(ctx, Relation{ParentSessionID: "parent", ChildSessionID: "child", Kind: "subagent"}); err != nil {
		t.Fatal(err)
	}
	finish(t, s, "a", "running")
	finish(t, s, "a", "interrupted")
	finish(t, s, "m", "completed")
	finish(t, s, "child", "completed")
	if st := state(t, s); st.Dirty != 0 {
		t.Fatalf("non-sources advanced the dirty revision: %+v", st)
	}
	finish(t, s, "a", "completed")
	finish(t, s, "parent", "failed")
	st := state(t, s)
	if st.Dirty != 2 || st.Cursor != 0 || st.Backlog != 2 {
		t.Fatalf("state after two sources: %+v", st)
	}
	src, err := s.MaintenanceSources(ctx, 0, st.Dirty, 10)
	if err != nil || len(src) != 2 || src[0].Ref.ID != "a" || src[1].Ref.ID != "parent" || src[0].Ref.Validate() != nil {
		t.Fatalf("sources: %+v %v", src, err)
	}
	// The same session reaching the same state again is the same version: same digest, new revision.
	finish(t, s, "a", "completed")
	again, _ := s.MaintenanceSources(ctx, 2, 3, 10)
	if len(again) != 1 || again[0].Ref.Digest != src[0].Ref.Digest {
		t.Fatalf("a repeated terminal write changed the source version: %+v vs %+v", again, src[0])
	}
}

// Without maintenance state nothing is tracked.
func TestNoTrackingWithoutMaintenanceState(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	createSession(t, s, "a", "")
	finish(t, s, "a", "completed")
	if _, err := s.ReadMaintenanceState(context.Background()); !errors.Is(err, ErrMaintenanceOff) {
		t.Fatalf("state without enablement: %v", err)
	}
}

// Dirty revision loss: the terminal status and its dirty mark commit together. When the mark cannot be
// written the status is not written either, so no terminal source exists without its mark.
func TestTerminalStatusAndDirtyMarkAreAtomic(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	finish(t, s, "a", "running")
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE maintenance_sources RENAME TO maintenance_sources_gone`); err != nil {
		t.Fatal(err)
	}
	status := "completed"
	if err := s.Update(ctx, "a", Patch{Status: &status}); err == nil {
		t.Fatal("the terminal write succeeded without its dirty mark")
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE maintenance_sources_gone RENAME TO maintenance_sources`); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, "a")
	if st := state(t, s); got.Status != "running" || st.Dirty != 0 {
		t.Fatalf("partial commit: status %s, %+v", got.Status, st)
	}
	finish(t, s, "a", "completed")
	if st := state(t, s); st.Dirty != 1 {
		t.Fatalf("retry: %+v", st)
	}
}

func candidate(id, claim string) CandidateRecord {
	p := knowledge.Proposal{Subject: "build mode", Claim: claim}
	return CandidateRecord{ID: id, ProjectID: "project", SourceRefs: []string{"session:a@x"}, SourceDigest: knowledge.Digest("x"),
		SourceScope: SourceScopeProject, ClaimType: knowledge.ClaimFact, Subject: p.Subject, Claim: claim,
		ClaimDigest: knowledge.ClaimDigest(p), CheckIDs: []string{"build"}, Status: knowledge.StatusCandidateOnly}
}

func commitFor(lease MaintenanceLease, through int64, ms string, cands ...CandidateRecord) MaintenanceCommit {
	return MaintenanceCommit{Lease: lease, Through: through, MaintenanceSessionID: ms,
		Dedupe:     []DedupeRecord{{Key: knowledge.Digest("key", fmt.Sprint(through), lease.LeaseID), SourceRef: "session:a@x", Revision: through, Outcome: OutcomeCandidates}},
		Candidates: cands}
}

// Lease takeover: once a lease expires and another worker takes it, the old holder's commit is refused
// even though the cursor never moved; the new holder's commit lands once.
func TestLeaseTakeoverRefusesTheOldGeneration(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m1", `{"purpose":"maintenance"}`)
	createSession(t, s, "m2", `{"purpose":"maintenance"}`)
	finish(t, s, "a", "completed")
	old, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if err != nil || old.Generation != 1 || old.Cursor != 0 || old.Dirty != 1 {
		t.Fatalf("first lease %+v %v", old, err)
	}
	if _, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0.Add(30*time.Second), time.Minute); !errors.Is(err, ErrMaintenanceLeaseHeld) {
		t.Fatalf("a live lease was taken: %v", err)
	}
	fresh, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0.Add(2*time.Minute), time.Minute)
	if err != nil || fresh.Generation != 2 || fresh.Cursor != old.Cursor {
		t.Fatalf("takeover %+v %v", fresh, err)
	}
	if _, err := s.CommitMaintenance(ctx, commitFor(old, 1, "m1", candidate("kc_old", "old worker")), t0.Add(2*time.Minute)); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("old generation committed: %v", err)
	}
	if err := s.RenewMaintenanceLease(ctx, &old, t0.Add(2*time.Minute), time.Minute); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("old generation renewed: %v", err)
	}
	res, err := s.CommitMaintenance(ctx, commitFor(fresh, 1, "m2", candidate("kc_new", "new worker")), t0.Add(2*time.Minute))
	if err != nil || len(res.Inserted) != 1 {
		t.Fatalf("new generation: %+v %v", res, err)
	}
	if _, err := s.Candidate(ctx, "kc_old"); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("the old worker's candidate exists: %v", err)
	}
	st := state(t, s)
	if st.Cursor != 1 || st.LeaseID != "" || st.Pending != "idle" || st.Backlog != 0 {
		t.Fatalf("after commit: %+v", st)
	}
	// The lease was released with the commit: committing it again is refused, nothing doubles.
	if _, err := s.CommitMaintenance(ctx, commitFor(fresh, 1, "m2", candidate("kc_twice", "again")), t0.Add(2*time.Minute)); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("a released lease committed again: %v", err)
	}
	if _, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0.Add(3*time.Minute), time.Minute); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("nothing pending, lease taken: %v", err)
	}
}

// Cursor jumps are refused: a batch may end only inside (cursor, dirty], the cursor never passes a
// revision nobody processed, and an expired lease cannot commit.
func TestCursorCannotJump(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	finish(t, s, "a", "completed")
	finish(t, s, "a", "failed")
	lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	for _, through := range []int64{0, 3, 99} {
		if _, err := s.CommitMaintenance(ctx, commitFor(lease, through, "m"), t0); !errors.Is(err, ErrStaleLease) {
			t.Fatalf("through %d: %v", through, err)
		}
	}
	if _, err := s.CommitMaintenance(ctx, commitFor(lease, 1, "m"), t0.Add(time.Minute)); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("expired lease committed: %v", err)
	}
	other := lease
	other.Policy = "another policy"
	if _, err := s.CommitMaintenance(ctx, commitFor(other, 1, "m"), t0); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("a commit under another policy landed: %v", err)
	}
	if _, err := s.CommitMaintenance(ctx, commitFor(lease, 1, "m"), t0); err != nil {
		t.Fatal(err)
	}
	st := state(t, s)
	if st.Cursor != 1 || st.Dirty != 2 || st.Backlog != 1 {
		t.Fatalf("the cursor passed revision 2 or lost it: %+v", st)
	}
	rest, _ := s.MaintenanceSources(ctx, st.Cursor, st.Dirty, 10)
	if len(rest) != 1 || rest[0].Revision != 2 {
		t.Fatalf("revision 2 is not pending: %+v", rest)
	}
}

// A crash between the candidates and the cursor leaves nothing: candidates, dedupe records, observations
// and the cursor commit in one transaction. The retry commits once.
func TestCommitIsAllOrNothing(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	finish(t, s, "a", "completed")
	lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	testHookMaintenanceCommit = func() error { return errors.New("crash between candidates and cursor") }
	_, err = s.CommitMaintenance(ctx, commitFor(lease, 1, "m", candidate("kc_1", "one")), t0)
	testHookMaintenanceCommit = nil
	if err == nil {
		t.Fatal("injected crash did not fail the commit")
	}
	if _, err := s.Candidate(ctx, "kc_1"); !errors.Is(err, ErrCandidateNotFound) {
		t.Fatalf("candidate survived the rollback: %v", err)
	}
	if st := state(t, s); st.Cursor != 0 || st.Backlog != 1 || st.LeaseID != lease.LeaseID {
		t.Fatalf("partial commit: %+v", st)
	}
	var dedupe int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM maintenance_dedupe`).Scan(&dedupe)
	if dedupe != 0 {
		t.Fatalf("dedupe rows survived the rollback: %d", dedupe)
	}
	res, err := s.CommitMaintenance(ctx, commitFor(lease, 1, "m", candidate("kc_1", "one")), t0)
	if err != nil || len(res.Inserted) != 1 {
		t.Fatalf("retry: %+v %v", res, err)
	}
	key := commitFor(lease, 1, "m").Dedupe[0].Key
	resolved, processed, err := s.ProcessingKeys(ctx, []string{key, "sha256:unknown"})
	if err != nil || len(processed) != 1 || !processed[key] || resolved[0] != key || resolved[1] != "sha256:unknown" {
		t.Fatalf("processed keys: %v %v %v", resolved, processed, err)
	}
}

// The same claim from another batch is a duplicate, not a second candidate; a rejected entry is kept for
// audit and never counts as a live candidate.
func TestDuplicateClaimsAreNotInsertedTwice(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	for round := 1; round <= 2; round++ {
		finish(t, s, "a", map[int]string{1: "completed", 2: "failed"}[round])
		lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		rejected := candidate(fmt.Sprintf("kc_rej%d", round), "same claim")
		rejected.Status = knowledge.StatusRejected
		res, err := s.CommitMaintenance(ctx, commitFor(lease, int64(round), "m", candidate(fmt.Sprintf("kc_%d", round), "same claim"), rejected), t0)
		if err != nil {
			t.Fatal(err)
		}
		if round == 2 && (res.Duplicates != 1 || len(res.Inserted) != 1 || res.Inserted[0] != "kc_rej2") {
			t.Fatalf("second round: %+v", res)
		}
	}
	live, _ := s.Candidates(ctx, CandidateFilter{Statuses: []string{knowledge.StatusCandidateOnly, knowledge.StatusQualified}})
	if len(live) != 1 {
		t.Fatalf("live candidates: %+v", live)
	}
}

func putEvalRow(t *testing.T, s *Store, sessionID string, seq int64, check, status, episodeStatus string, epoch int64, pre, post string) {
	t.Helper()
	_, err := s.db.Exec(`INSERT INTO eval_results(session_id, seq, check_id, kind, status, provenance, fingerprint, artifacts_json, at,
		episode_id, pre_fingerprint, post_fingerprint, spec_digest, approval_epoch, episode_status)
		VALUES(?, ?, ?, 'test', ?, 'runtime', ?, '[]', ?, 'ep_1', ?, ?, 'sha256:spec', ?, ?)`,
		sessionID, seq, check, status, post, t0.Format(time.RFC3339Nano), pre, post, epoch, episodeStatus)
	if err != nil {
		t.Fatal(err)
	}
}

// Only a runtime result of an approved check, in a closed episode with a stable fingerprint, of a check
// the candidate names, becomes an observation; observations are appended, never overwritten.
func TestObservationsAreDerivedFromQualifiedResultsOnly(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	finish(t, s, "a", "completed")
	lease, _ := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if _, err := s.CommitMaintenance(ctx, commitFor(lease, 1, "m", candidate("kc_1", "claim")), t0); err != nil {
		t.Fatal(err)
	}
	putEvalRow(t, s, "a", 1, "build", "failed", "closed", 1, "f1", "f1")
	putEvalRow(t, s, "a", 2, "build", "passed", "open", 1, "f1", "f1")
	putEvalRow(t, s, "a", 3, "build", "passed", "closed", 0, "f1", "f1")
	putEvalRow(t, s, "a", 4, "build", "passed", "closed", 1, "f1", "f2")
	putEvalRow(t, s, "a", 5, "lint", "passed", "closed", 1, "f1", "f1")
	putEvalRow(t, s, "a", 6, "build", "passed", "closed", 1, "f2", "f2")
	link := func(seq int64) ObservationLink {
		return ObservationLink{CandidateID: "kc_1", SessionID: "a", ResultSeq: seq, Scope: "w", Purpose: ObservationRevalidation}
	}
	for seq, why := range map[int64]string{2: "open episode", 3: "unapproved", 4: "fingerprint moved", 5: "check not named", 99: "no row"} {
		if err := s.AppendObservation(ctx, link(seq), t0); !errors.Is(err, ErrNotQualified) {
			t.Errorf("%s: %v", why, err)
		}
	}
	if err := s.AppendObservation(ctx, link(1), t0); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendObservation(ctx, link(6), t0); err != nil {
		t.Fatal(err)
	}
	obs, err := s.Observations(ctx, "kc_1")
	if err != nil || len(obs) != 2 || obs[0].Outcome != "passed" || obs[1].Outcome != "failed" || obs[0].Fingerprint != "f2" {
		t.Fatalf("observations: %+v %v", obs, err)
	}
	// Deleting the session holding a result removes its observation with it.
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE id = 'a'`); err != nil {
		t.Fatal(err)
	}
	if obs, _ := s.Observations(ctx, "kc_1"); len(obs) != 0 {
		t.Fatalf("observations of a deleted session survived: %+v", obs)
	}
}

// Two Store handles (two connection pools, as two Engines in one process) race for the lease: exactly
// one wins each round, and every source is committed exactly once.
func TestConcurrentWorkersCommitEachSourceOnce(t *testing.T) {
	ctx := context.Background()
	s, dir := maintenanceStore(t)
	other, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	for i := range 6 {
		id := fmt.Sprintf("s%d", i)
		createSession(t, s, id, "")
		finish(t, s, id, "completed")
	}
	var mu sync.Mutex
	commits := 0
	var wg sync.WaitGroup
	for _, store := range []*Store{s, other, s, other} {
		wg.Add(1)
		go func(store *Store) {
			defer wg.Done()
			for {
				lease, err := store.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
				if errors.Is(err, ErrNothingPending) {
					return
				}
				if errors.Is(err, ErrMaintenanceLeaseHeld) {
					time.Sleep(time.Millisecond)
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
				through := lease.Cursor + 1
				if _, err := store.CommitMaintenance(ctx, commitFor(lease, through, "m"), t0); err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				commits++
				mu.Unlock()
			}
		}(store)
	}
	wg.Wait()
	st := state(t, s)
	var dedupe int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM maintenance_dedupe`).Scan(&dedupe)
	if commits != 6 || st.Cursor != 6 || dedupe != 6 || st.Generation != 6 {
		t.Fatalf("commits %d, dedupe %d, state %+v", commits, dedupe, st)
	}
}

// The lease is a database row, so it holds across processes: a child process cannot take a live lease,
// takes an expired one, and the parent's commit under the old generation is then refused.
func TestMaintenanceLeaseAcrossProcesses(t *testing.T) {
	ctx := context.Background()
	s, dir := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	finish(t, s, "a", "completed")
	lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	child := func(now time.Time, action string) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestMaintenanceHelperProcess$")
		cmd.Env = append(os.Environ(), "LC_MAINT_HELPER=1", "LC_MAINT_DIR="+dir, "LC_MAINT_NOW="+now.Format(time.RFC3339Nano), "LC_MAINT_ACTION="+action)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("helper: %v\n%s", err, out)
		}
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "RESULT ") {
				return strings.TrimPrefix(line, "RESULT ")
			}
		}
		t.Fatalf("helper printed no result:\n%s", out)
		return ""
	}
	if got := child(t0.Add(10*time.Second), "acquire+commit"); got != "held" {
		t.Fatalf("child took a live lease: %s", got)
	}
	if got := child(t0.Add(2*time.Minute), "acquire+commit"); got != "committed generation 2" {
		t.Fatalf("child after expiry: %s", got)
	}
	if _, err := s.CommitMaintenance(ctx, commitFor(lease, 1, "m", candidate("kc_parent", "parent")), t0.Add(2*time.Minute)); !errors.Is(err, ErrStaleLease) {
		t.Fatalf("parent committed under a generation another process replaced: %v", err)
	}
	if st := state(t, s); st.Cursor != 1 || st.Generation != 2 {
		t.Fatalf("state: %+v", st)
	}
}

// TestMaintenanceHelperProcess is the child of TestMaintenanceLeaseAcrossProcesses; it does nothing in
// a normal run.
func TestMaintenanceHelperProcess(t *testing.T) {
	if os.Getenv("LC_MAINT_HELPER") != "1" {
		t.Skip("helper process only")
	}
	ctx := context.Background()
	s, err := Open(os.Getenv("LC_MAINT_DIR"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("LC_MAINT_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, now, time.Minute)
	switch {
	case errors.Is(err, ErrMaintenanceLeaseHeld):
		fmt.Println("RESULT held")
		return
	case err != nil:
		t.Fatal(err)
	}
	if _, err := s.CommitMaintenance(ctx, commitFor(lease, 1, "m", candidate("kc_child", "child")), now); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("RESULT committed generation %d\n", lease.Generation)
}

// A failed batch releases the lease and keeps the dirty revision; a host cannot claim the core purpose.
func TestFailMaintenanceKeepsDirtyAndPurposeIsCoreOwned(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	finish(t, s, "a", "completed")
	lease, _ := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
	if err := s.FailMaintenance(ctx, lease, "maintainer budget exhausted", t0); err != nil {
		t.Fatal(err)
	}
	st := state(t, s)
	if st.Cursor != 0 || st.Dirty != 1 || st.LeaseID != "" || st.Pending != "failed" || st.LastError == "" {
		t.Fatalf("after failure: %+v", st)
	}
	if err := CheckHostMetadata(json.RawMessage(`{"purpose":"maintenance"}`)); !errors.Is(err, ErrCoreMetadataKey) {
		t.Fatalf("host claimed the maintenance purpose: %v", err)
	}
	if next, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute); err != nil || next.Generation != 2 {
		t.Fatalf("retry lease: %+v %v", next, err)
	}
}

// sourceRefOf is the ref of session id at its version now, as a batch hands it to a maintainer.
func sourceRefOf(t *testing.T, s *Store, id string) knowledge.SourceRef {
	t.Helper()
	digest, err := s.SourceVersion(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return knowledge.SourceRef{Kind: knowledge.KindSession, ID: id, SessionID: id, Digest: digest, Scope: SourceScopeProject,
		Sensitivity: knowledge.SensitivitySession, Provenance: knowledge.ProvenanceRuntime}
}

// P1-03: a commit is a compare-and-set on the source versions its maintainer read. A source whose final
// answer, transcript or status changed after its batch was read refuses the whole commit with
// ErrSourceChanged: nothing is written, the cursor stays and the dirty revision still covers the
// source, whose new version the next pass commits.
func TestCommitRefusesASourceChangedAfterItsBatchWasRead(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, s *Store)
	}{
		{"final answer", func(t *testing.T, s *Store) {
			final := "corrected: the user now selected 9090"
			if err := s.Update(ctx, "a", Patch{Final: &final}); err != nil {
				t.Fatal(err)
			}
		}},
		{"new message", func(t *testing.T, s *Store) {
			if _, err := s.AppendMessage(ctx, "a", "user", map[string]any{"role": "user", "content": "use 9090 instead"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"status", func(t *testing.T, s *Store) { finish(t, s, "a", "failed") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := maintenanceStore(t)
			createSession(t, s, "a", "")
			createSession(t, s, "m", `{"purpose":"maintenance"}`)
			finish(t, s, "a", "completed")
			lease, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			read := sourceRefOf(t, s, "a")
			tc.change(t, s)
			if now := sourceRefOf(t, s, "a"); now.Digest == read.Digest {
				t.Fatalf("the change kept the version %s", read.Digest)
			}
			c := commitFor(lease, lease.Dirty, "m", candidate("kc_old", "claim about the old version"))
			c.Sources = []knowledge.SourceRef{read}
			if _, err := s.CommitMaintenance(ctx, c, t0); !errors.Is(err, ErrSourceChanged) {
				t.Fatalf("commit over a changed source: %v", err)
			}
			if live, _ := s.Candidates(ctx, CandidateFilter{Limit: 10}); len(live) != 0 {
				t.Fatalf("a refused commit wrote candidates %+v", live)
			}
			if st := state(t, s); st.Cursor != 0 || st.Dirty < 1 {
				t.Fatalf("state after the refused commit %+v", st)
			}
			if err := s.FailMaintenance(ctx, lease, "source changed", t0); err != nil {
				t.Fatal(err)
			}
			next, err := s.AcquireMaintenanceLease(ctx, testPolicy, t0, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			c = commitFor(next, next.Dirty, "m", candidate("kc_new", "claim about the new version"))
			c.Sources = []knowledge.SourceRef{sourceRefOf(t, s, "a")}
			if res, err := s.CommitMaintenance(ctx, c, t0); err != nil || len(res.Inserted) != 1 {
				t.Fatalf("commit of the new version %+v %v", res, err)
			}
			if st := state(t, s); st.Cursor != next.Dirty {
				t.Fatalf("cursor %d after the commit, want %d", st.Cursor, next.Dirty)
			}
		})
	}
}

// P1-04: dedupe compares the support identity, not the claim text. The same claim from the same source
// versions, scope, policy and binding is a duplicate; another source version, scope, policy or binding
// is new support and is recorded next to the earlier row, which stays as it was.
func TestSupportIdentityDecidesDuplicates(t *testing.T) {
	ctx := context.Background()
	s, _ := maintenanceStore(t)
	createSession(t, s, "a", "")
	createSession(t, s, "m", `{"purpose":"maintenance"}`)
	rounds := []struct {
		name      string
		policy    string
		edit      func(*CandidateRecord)
		duplicate bool
	}{
		{"first", testPolicy, nil, false},
		{"same support", testPolicy, nil, true},
		{"new policy version", "test-policy-2", nil, false},
		{"different scope", testPolicy, func(c *CandidateRecord) { c.SourceScope = "another-scope" }, false},
		{"new source version", testPolicy, func(c *CandidateRecord) { c.SourceDigest = knowledge.Digest("y") }, false},
		{"other binding", testPolicy, func(c *CandidateRecord) { c.BindingDigest = "sha256:binding-y" }, false},
		{"same support again", testPolicy, nil, true},
	}
	statuses := []string{"completed", "failed"}
	for i, r := range rounds {
		finish(t, s, "a", statuses[i%2])
		lease, err := s.AcquireMaintenanceLease(ctx, r.policy, t0, time.Minute)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		c := candidate(fmt.Sprintf("kc_%d", i), "same claim")
		assertion := knowledge.Assertion{Kind: knowledge.AssertPortValue, Target: "service", Value: "8080"}
		c.Assertion, c.CheckIDs = &assertion, []string{"port"}
		c.Status, c.BindingDigest = knowledge.StatusQualified, "sha256:binding-x"
		if r.edit != nil {
			r.edit(&c)
		}
		epoch, err := s.ApproveCheckSet(ctx, CheckSetApproval{ProjectID: "project", SetDigest: "set", Specs: []ApprovedSpec{{CheckID: "port", SpecDigest: "sha256:spec"}}, ApprovedBy: ApprovedByUser, Actor: "host"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApproveConstraintBinding(ctx, BindingApproval{ProjectID: "project", ConstraintRef: assertion.Ref(), ConstraintDigest: assertion.ValueDigest(), CheckID: "port", SpecDigest: "sha256:spec", BindingDigest: c.BindingDigest, ApprovedBy: ApprovedByUser, Actor: "host"}); err != nil {
			t.Fatal(err)
		}
		seq := int64(i + 1)
		putEvalRow(t, s, "m", seq, "port", "passed", "closed", epoch, "f1", "f1")
		commit := commitFor(lease, lease.Dirty, "m", c)
		commit.Observations = []ObservationLink{{CandidateID: c.ID, SessionID: "m", ResultSeq: seq, Purpose: ObservationAdoption, Scope: "/project"}}
		res, err := s.CommitMaintenance(ctx, commit, t0)
		if err != nil {
			t.Fatalf("%s: %v", r.name, err)
		}
		if dup := res.Duplicates == 1 && len(res.Inserted) == 0; dup != r.duplicate || (!r.duplicate && len(res.Inserted) != 1) {
			t.Fatalf("%s: %+v, want duplicate=%v", r.name, res, r.duplicate)
		}
	}
	live, err := s.Candidates(ctx, CandidateFilter{Statuses: []string{knowledge.StatusQualified}, Limit: 20})
	if err != nil || len(live) != 5 {
		t.Fatalf("live candidates %d %v", len(live), err)
	}
	first, err := s.Candidate(ctx, "kc_0")
	if err != nil || first.PolicyVersion != testPolicy || first.SupportDigest == "" {
		t.Fatalf("the first row %+v %v", first, err)
	}
}
