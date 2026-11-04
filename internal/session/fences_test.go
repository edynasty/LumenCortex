package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
)

func insertEdge(t *testing.T, store *Store, consumer, kind, target, sourceKind, sourceRef string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO derived_origins(session_id, kind, target_ref, source_kind, source_ref, origin_class, lineage_complete, created_at, updated_at)
		VALUES(?, ?, ?, ?, ?, 'agent_generated', 1, 't', 't')`, consumer, kind, target, sourceKind, sourceRef); err != nil {
		t.Fatal(err)
	}
}

func sorted(v []string) []string {
	out := append([]string(nil), v...)
	sort.Strings(out)
	return out
}

// Plan T3-S1 at the Store: one transaction walks the reverse index from the revoked session - the
// sessions whose edges cite a reached one, the children a reached one started by relation or by a
// child_session edge - fences them all with one epoch and stores the exposure it found. After it, no
// fenced session admits a request or an execution, and an edge citing any fenced session, committed
// in a session the walk did not reach, is refused.
func TestRevokeSessionFencesWhatItReachesAndReports(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	for _, id := range []string{"x", "s", "t2", "c", "d", "u"} {
		gapSession(t, store, id, "idle", "")
	}
	insertEdge(t, store, "s", "message", "tool:r:c1", SourceSession, "x")        // s read x
	insertEdge(t, store, "t2", "message", "tool:r:c2", SourceSession, "s")       // t2 read s
	insertEdge(t, store, "s", "child_session", "session:d", SourceRequest, "s1") // s started d
	insertEdge(t, store, "u", "file", "a.go@sha1:1", SourceRequest, "x")         // a request named x: not a session source
	if err := store.AddRelation(ctx, Relation{ParentSessionID: "x", ChildSessionID: "c", Kind: "subagent"}); err != nil {
		t.Fatal(err)
	}
	reach(t, store, "x", "x1", PurposeMain, RequestAnswered)
	reach(t, store, "s", "s1", PurposeMain, RequestAdmitted)
	reach(t, store, "s", "s2", PurposeMain, RequestUnknown)
	reach(t, store, "s", "s3", PurposeAutoEffort, RequestAnswered)
	reach(t, store, "s", "s4", PurposeMain, RequestFailed)
	reach(t, store, "t2", "t1", PurposeMain, RequestAdmitted)
	reach(t, store, "u", "u1", PurposeMain, RequestAdmitted)
	if err := store.AdmitExecution(ctx, execAdmission("s", "e1", "s1")); err != nil {
		t.Fatal(err)
	}
	if err := store.AdmitExecution(ctx, execAdmission("u", "eu", "u1")); err != nil {
		t.Fatal(err)
	}

	if _, err := store.RevokeSession(ctx, "x", "Not A Code"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("free-text reason: %v", err)
	}
	if _, err := store.RevokeSession(ctx, "nope", "user_revoked"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown session: %v", err)
	}
	if n := count(t, store, `SELECT count(*) FROM source_fences`); n != 0 {
		t.Fatalf("a refused revocation wrote %d fences", n)
	}

	rev, err := store.RevokeSession(ctx, "x", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := sorted(rev.Sessions), []string{"c", "d", "s", "t2", "x"}; !reflect.DeepEqual(got, want) || rev.Sessions[0] != "x" || rev.SessionCount != 5 || rev.Expanded {
		t.Fatalf("sessions %v (count %d, expanded %v), want %v source first", rev.Sessions, rev.SessionCount, rev.Expanded, want)
	}
	if !reflect.DeepEqual(sorted(rev.InFlight), []string{"s1", "t1"}) || rev.InFlightCount != 2 ||
		!reflect.DeepEqual(rev.Unknown, []string{"s2"}) || rev.UnknownCount != 1 || rev.Answered != 2 ||
		!reflect.DeepEqual(rev.Executions, []string{"e1"}) || rev.ExecutionCount != 1 {
		t.Fatalf("exposure %+v", rev)
	}
	if n := count(t, store, `SELECT count(*) FROM source_fences WHERE scope_kind = 'session' AND state = 'active' AND epoch = ? AND reason = 'user_revoked'`, rev.Epoch); n != 5 {
		t.Fatalf("%d fences with the epoch, want 5", n)
	}
	back, err := store.Revocation(ctx, rev.Epoch)
	if err != nil || !reflect.DeepEqual(back, rev) {
		t.Fatalf("read back %+v %v, want %+v", back, err, rev)
	}
	if _, err := store.Revocation(ctx, rev.Epoch+1); !errors.Is(err, ErrNoRevocation) {
		t.Fatalf("unknown epoch: %v", err)
	}

	// After the commit: every fenced session refuses admission, the unreached one does not.
	for _, id := range []string{"x", "s", "t2", "c", "d"} {
		if _, err := store.AdmitRequest(ctx, mainAdmission(id, "after-"+id)); !errors.Is(err, ErrFenced) {
			t.Errorf("request admission in %s: %v, want ErrFenced", id, err)
		}
	}
	if err := store.AdmitExecution(ctx, execAdmission("s", "e2", "s1")); !errors.Is(err, ErrFenced) {
		t.Errorf("execution admission in s: %v, want ErrFenced", err)
	}
	admit(t, store, mainAdmission("u", "u2"))
	// A late edge: u read t2 before the revocation and commits after it. t2 was reached through s, so
	// citing it is refused and nothing of the result is stored.
	edge := DerivedEdge{Kind: "message", TargetRef: "tool:u1:c9", SourceKind: SourceSession, SourceRef: "t2", OriginClass: OriginAgentGen, LineageComplete: true}
	rows := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 'u'`)
	if _, _, err := store.CommitToolResult(ctx, toolCommit("u", "eu", OutcomeComplete, CoverageComplete, edge)); !errors.Is(err, ErrFenced) {
		t.Fatalf("late edge citing a reached session: %v, want ErrFenced", err)
	}
	if n := count(t, store, `SELECT count(*) FROM session_messages WHERE session_id = 'u'`); n != rows {
		t.Fatal("the late result was stored")
	}
	if n := count(t, store, `SELECT count(*) FROM derived_origins WHERE session_id = 'u' AND source_ref = 't2'`); n != 0 {
		t.Fatal("the late edge was stored")
	}

	// A second revocation is a new epoch.
	again, err := store.RevokeSession(ctx, "u", "user_revoked")
	if err != nil || again.Epoch != rev.Epoch+1 || !reflect.DeepEqual(again.Sessions, []string{"u"}) {
		t.Fatalf("second revocation %+v %v", again, err)
	}
}

// Past MaxRevokeSessions the walk stops and every session of the database is fenced, the unrelated
// ones included: wider, never narrower.
func TestRevokeSessionExpandsPastTheBound(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "x", "idle", "")
	gapSession(t, store, "unrelated", "idle", "")
	for i := 0; i < MaxRevokeSessions; i++ {
		id := fmt.Sprintf("r%03d", i)
		gapSession(t, store, id, "idle", "")
		insertEdge(t, store, id, "message", "tool:r:c", SourceSession, "x")
	}
	rev, err := store.RevokeSession(ctx, "x", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	total := MaxRevokeSessions + 2
	if !rev.Expanded || rev.SessionCount != total || len(rev.Sessions) != MaxReportIDs || rev.Sessions[0] != "x" {
		t.Fatalf("expanded=%v count=%d listed=%d first=%q", rev.Expanded, rev.SessionCount, len(rev.Sessions), rev.Sessions[0])
	}
	if n := count(t, store, `SELECT count(DISTINCT scope_ref) FROM source_fences WHERE epoch = ?`, rev.Epoch); n != total {
		t.Fatalf("%d sessions fenced, want %d", n, total)
	}
	if _, err := store.AdmitRequest(ctx, mainAdmission("unrelated", "q")); !errors.Is(err, ErrFenced) {
		t.Fatalf("an expanded revocation left a session open: %v", err)
	}
}

// Exactly MaxRevokeSessions reached sessions do not expand.
func TestRevokeSessionAtTheBoundDoesNotExpand(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "x", "idle", "")
	gapSession(t, store, "unrelated", "idle", "")
	for i := 0; i < MaxRevokeSessions-1; i++ {
		id := fmt.Sprintf("r%03d", i)
		gapSession(t, store, id, "idle", "")
		insertEdge(t, store, id, "message", "tool:r:c", SourceSession, "x")
	}
	rev, err := store.RevokeSession(ctx, "x", "user_revoked")
	if err != nil {
		t.Fatal(err)
	}
	if rev.Expanded || rev.SessionCount != MaxRevokeSessions {
		t.Fatalf("expanded=%v count=%d, want %d without expansion", rev.Expanded, rev.SessionCount, MaxRevokeSessions)
	}
	admit(t, store, mainAdmission("unrelated", "q"))
}
