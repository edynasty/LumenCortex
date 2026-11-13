package session

import (
	"context"
	"errors"
	"testing"
	"time"
)

// These Store contract tests vary one field at a time through AppendEvalResult;
// real Work Unit/Evaluator/read_knowledge paths live in runtime's regression.
func cognitionEval(status string) EvalRow {
	return EvalRow{CheckID: "port", Kind: "test", Status: status, Provenance: "runtime",
		Fingerprint: "workspace@local#v2", SpecDigest: "approved-definition", ApprovalEpoch: 7,
		EpisodeID: "episode", EpisodeStatus: EpisodeClosed, PreFingerprint: "workspace@local#v2", PostFingerprint: "workspace@local#v2", At: t0}
}

func appendCognitionEval(t *testing.T, s *Store, owner string, row EvalRow) int64 {
	t.Helper()
	seq, err := s.AppendEvalResult(context.Background(), owner, row)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func ownCognitionResult(t *testing.T, s *Store, owner string, key EvalRow) CognitionResult {
	t.Helper()
	rows, err := s.CognitionResults(context.Background(), "kc_unused", "workspace", owner, key.CheckID, key.Fingerprint, key.SpecDigest, key.ApprovalEpoch)
	if err != nil || len(rows) != 1 || rows[0].SessionID != owner {
		t.Fatalf("own result: %+v %v", rows, err)
	}
	return rows[0]
}

func TestCognitionResultsFailureIsolation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*EvalRow)
		invalidates bool
	}{
		{"same_state", func(*EvalRow) {}, true},
		{"workspace", func(r *EvalRow) {
			r.Fingerprint = "workspace@other#v2"
			r.PreFingerprint = r.Fingerprint
			r.PostFingerprint = r.Fingerprint
		}, false},
		{"spec_digest", func(r *EvalRow) { r.SpecDigest = "other-definition" }, false},
		{"approval_epoch", func(r *EvalRow) { r.ApprovalEpoch++ }, false},
		{"check_id", func(r *EvalRow) { r.CheckID = "different-check" }, false},
		{"definition_input_version", func(r *EvalRow) {
			r.Fingerprint = "workspace@local#v3"
			r.PreFingerprint = r.Fingerprint
			r.PostFingerprint = r.Fingerprint
		}, false},
		{"open", func(r *EvalRow) { r.EpisodeStatus = EpisodeOpen; r.PostFingerprint = "" }, false},
		{"interrupted", func(r *EvalRow) { r.EpisodeStatus = EpisodeInterrupted; r.PostFingerprint = "" }, false},
		{"scope_changed", func(r *EvalRow) { r.EpisodeStatus = EpisodeScopeChanged }, false},
		{"spec_unapproved", func(r *EvalRow) { r.EpisodeStatus = EpisodeSpecUnapproved }, false},
		{"error", func(r *EvalRow) { r.Status = "error" }, false},
		{"model_provenance", func(r *EvalRow) { r.Provenance = "llm:test" }, false},
		{"human_provenance", func(r *EvalRow) { r.Provenance = "human:test" }, false},
		{"no_approval", func(r *EvalRow) { r.ApprovalEpoch = 0 }, false},
		{"no_episode", func(r *EvalRow) { r.EpisodeStatus = ""; r.EpisodeID = "" }, false},
		{"unstable_fingerprint", func(r *EvalRow) { r.PreFingerprint = "previous" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := maintenanceStore(t)
			createSession(t, s, "x", "")
			createSession(t, s, "y", "")
			pass := cognitionEval("passed")
			seq := appendCognitionEval(t, s, "x", pass)
			failure := cognitionEval("failed")
			// Neither equal session-local sequence numbers nor a backwards wall
			// clock may make later appended evidence older than X's PASS.
			failure.At = pass.At.Add(-time.Hour)
			tc.change(&failure)
			appendCognitionEval(t, s, "y", failure)
			got := ownCognitionResult(t, s, "x", pass)
			if got.Seq != seq || got.Outcome != "passed" || got.Superseded != tc.invalidates {
				t.Fatalf("state isolation: %+v; invalidates=%v", got, tc.invalidates)
			}
			rows, err := s.EvalResults(context.Background(), "x", 0, 10)
			if err != nil || len(rows) != 1 || rows[0].Status != "passed" {
				t.Fatalf("freshness rewrote evidence: %+v %v", rows, err)
			}
		})
	}
}

func TestCognitionResultsOnlyNewOwnPassRecovers(t *testing.T) {
	s, _ := maintenanceStore(t)
	createSession(t, s, "x", "")
	createSession(t, s, "y", "")
	pass := cognitionEval("passed")
	appendCognitionEval(t, s, "x", pass)
	appendCognitionEval(t, s, "y", cognitionEval("failed"))
	appendCognitionEval(t, s, "y", pass)
	if got := ownCognitionResult(t, s, "x", pass); !got.Superseded {
		t.Fatalf("foreign PASS revived X's old PASS: %+v", got)
	}
	newSeq := appendCognitionEval(t, s, "x", pass)
	if got := ownCognitionResult(t, s, "x", pass); got.Seq != newSeq || got.Superseded {
		t.Fatalf("X's later PASS did not recover: %+v", got)
	}
	rows, err := s.EvalResults(context.Background(), "y", 0, 10)
	if err != nil || len(rows) != 2 || rows[0].Status != "passed" || rows[1].Status != "failed" {
		t.Fatalf("failure history was lost: %+v %v", rows, err)
	}
}

func TestCognitionResultsEpisodeCloseBoundary(t *testing.T) {
	s, _ := maintenanceStore(t)
	createSession(t, s, "x", "")
	createSession(t, s, "y", "")
	pass := cognitionEval("passed")
	appendCognitionEval(t, s, "x", pass)
	failure := cognitionEval("failed")
	failure.EpisodeStatus, failure.PostFingerprint = EpisodeOpen, ""
	seq := appendCognitionEval(t, s, "y", failure)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.CloseEpisode(ctx, "y", failure.EpisodeID, EpisodeClosed, map[int64]string{seq: failure.Fingerprint}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled close: %v", err)
	}
	if got := ownCognitionResult(t, s, "x", pass); got.Superseded {
		t.Fatalf("uncommitted close invalidated PASS: %+v", got)
	}
	if err := s.CloseEpisode(context.Background(), "y", failure.EpisodeID, EpisodeClosed, map[int64]string{seq: failure.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if got := ownCognitionResult(t, s, "x", pass); !got.Superseded {
		t.Fatalf("committed close was not visible: %+v", got)
	}
}

func TestCognitionResultsOrderSurvivesVacuumAndReopen(t *testing.T) {
	s, dir := maintenanceStore(t)
	createSession(t, s, "z-first", "")
	createSession(t, s, "a-second", "")
	pass := cognitionEval("passed")
	appendCognitionEval(t, s, "z-first", pass)
	appendCognitionEval(t, s, "a-second", cognitionEval("failed"))
	if _, err := s.db.ExecContext(context.Background(), "VACUUM"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	if got := ownCognitionResult(t, reopened, "z-first", pass); !got.Superseded {
		t.Fatalf("rebuild/reopen lost append ordering: %+v", got)
	}
	appendCognitionEval(t, reopened, "z-first", pass)
	if got := ownCognitionResult(t, reopened, "z-first", pass); got.Superseded {
		t.Fatalf("fresh PASS did not recover after rebuild: %+v", got)
	}
}
