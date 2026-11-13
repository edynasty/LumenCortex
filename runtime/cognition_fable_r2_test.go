package runtime

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// Second review of the Fable remediation (candidate cbe3a4e): regression tests of what that review
// found, end to end through the real loop, store, checks, approvals and git with scripted models.

// crowd writes n copies of the live candidate id under other subjects, each newer than every candidate
// the passes committed: unverified model output about unrelated things, as many batches would leave it.
func (f *cognitionFixture) crowd(t *testing.T, id string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+f.engine.store.Path()+"?_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Now().UTC().Add(time.Hour)
	for i := range n {
		subject := fmt.Sprintf("unrelated note %d", i)
		digest := knowledge.Digest("crowd", subject)
		if _, err := db.ExecContext(context.Background(), `INSERT INTO cognition_candidates(candidate_id, schema_version, project_id,
			maintenance_session_id, generation, revision, source_refs_json, source_digest, source_scope, origin_run_id, origin_session_id,
			claim_type, subject, subject_key, proposed_claim, rationale, claim_digest, cited_paths_json, check_ids_json, policy_version,
			status, status_reason, created_at, assertion_json, binding_digest, support_digest)
			SELECT ?, schema_version, project_id, maintenance_session_id, generation, revision, source_refs_json, source_digest,
			source_scope, origin_run_id, origin_session_id, claim_type, ?, ?, ?, rationale, ?, cited_paths_json, check_ids_json,
			policy_version, status, status_reason, ?, assertion_json, binding_digest, ? FROM cognition_candidates WHERE candidate_id = ?`,
			fmt.Sprintf("kc_crowd_%04d", i), subject, knowledge.SubjectKey(subject), "Note "+subject+".", digest,
			at.Add(time.Duration(i)*time.Second).Format(time.RFC3339Nano), digest, id); err != nil {
			t.Fatal(err)
		}
	}
}

// FABLE-01 (evidence level x the assessment window): the conflict of a candidate read by id is decided
// against the live candidates of its own subject. It was decided against the newest
// maxAssessedCandidates live candidates of any subject, so enough newer unverified candidates about
// other things pushed an equally supported contrary fact out of the comparison, and the read reported
// "unknown" where the listing of the same reader reported a conflict: unverified model output changed
// what a host-supported candidate is to a reader, by its number alone.
func TestFableReadConflictIsDecidedWithinTheSubject(t *testing.T) {
	f := newCognitionFixture(t, true)
	a := f.run(t, "inspect the service", cogAnswer("the service listens on 8080"))
	first := f.adoptFacts(t, a, portFact("8080"))
	f.write(t, "defaults.env", "BASE=9090\n")
	b := f.run(t, "inspect the service after the change", cogAnswer("the service listens on 9090"))
	second := f.adoptFacts(t, b, portFact("9090"))
	if first.Qualified != 1 || second.Qualified != 1 {
		t.Fatalf("fixture: %+v %+v", first, second)
	}
	old, corrected := first.Inserted[0], second.Inserted[0]
	// A host read is no run: both supported facts are unknown to it, and they contradict each other.
	for _, id := range []string{old, corrected} {
		if v := f.hostView(t, id); v.Applicability != knowledge.Conflict {
			t.Fatalf("fixture: host read of %s is %s (%s)", id, v.Applicability, v.Reason)
		}
	}
	c := f.run(t, "take notes", cogAnswer("nothing about the port"))
	f.propose(func(refs []string) []string {
		return []string{candidateJSON("decision", "unrelated note", "Notes are kept short.", refOf(t, refs, c), nil)}
	})
	note := f.maintain(t)
	if len(note.Inserted) != 1 || note.CandidateOnly != 1 {
		t.Fatalf("fixture: %+v", note)
	}
	f.crowd(t, note.Inserted[0], maxAssessedCandidates)
	for _, id := range []string{old, corrected} {
		if v := f.hostView(t, id); v.Applicability != knowledge.Conflict {
			t.Errorf("after %d newer unverified candidates of other subjects, the host read of %s is %s (%s)", maxAssessedCandidates, id, v.Applicability, v.Reason)
		}
	}
	// A run decides by its own fresh results, whatever the number of unrelated candidates: the
	// corrected fact is current, the old one stale.
	p := scripted(readStep("r1", corrected), readStep("r2", old))
	f.run(t, "use the project's cognition", p)
	if v := viewIn(t, p.request(t, 1), "r1"); v.Applicability != knowledge.Current || v.Claim != portFact("9090").Render() {
		t.Errorf("the corrected fact to a run %s (%s) %q", v.Applicability, v.Reason, v.Claim)
	}
	if v := viewIn(t, p.request(t, 2), "r2"); v.Applicability != knowledge.Stale {
		t.Errorf("the old fact to a run %s (%s)", v.Applicability, v.Reason)
	}
	f.supportedOnly(t)
}
