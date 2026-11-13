package session

import (
	"context"
	"testing"
)

func TestCognitivePrivacyDeletionCannotReviveOldPass(t *testing.T) {
	s, dir := maintenanceStore(t)
	createSession(t, s, "old-reader", "")
	createSession(t, s, "failed-reader", "")
	pass := cognitionEval("passed")
	appendCognitionEval(t, s, "old-reader", pass)
	appendCognitionEval(t, s, "failed-reader", cognitionEval("failed"))
	if !ownCognitionResult(t, s, "old-reader", pass).Superseded {
		t.Fatal("failure did not invalidate")
	}
	d, _, err := s.BeginPrivacyDeletion(context.Background(), []string{"failed-reader"})
	if err != nil {
		t.Fatal(err)
	}
	for done := false; !done; {
		done, err = s.PurgeStep(context.Background(), d)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !ownCognitionResult(t, s, "old-reader", pass).Superseded {
		t.Fatal("deleting a failure revived old PASS")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !ownCognitionResult(t, s, "old-reader", pass).Superseded {
		t.Fatal("restart revived old PASS")
	}
	newSeq := appendCognitionEval(t, s, "old-reader", pass)
	if got := ownCognitionResult(t, s, "old-reader", pass); got.Superseded || got.Seq != newSeq {
		t.Fatalf("new independent evidence did not recover: %+v", got)
	}
	rows, err := s.EvalResults(context.Background(), "old-reader", 0, 10)
	if err != nil || len(rows) != 2 || rows[1].Status != "passed" {
		t.Fatalf("original history rewritten: %+v %v", rows, err)
	}
}

func TestCognitivePrivacyDeletionPreservesUnrelatedFailure(t *testing.T) {
	s, _ := maintenanceStore(t)
	createSession(t, s, "counterexample", "")
	createSession(t, s, "unrelated", "")
	failed := cognitionEval("failed")
	seq := appendCognitionEval(t, s, "counterexample", failed)
	d, _, err := s.BeginPrivacyDeletion(context.Background(), []string{"unrelated"})
	if err != nil {
		t.Fatal(err)
	}
	for done := false; !done; {
		done, err = s.PurgeStep(context.Background(), d)
		if err != nil {
			t.Fatal(err)
		}
	}
	got := ownCognitionResult(t, s, "counterexample", failed)
	if got.Superseded || got.Outcome != "failed" || got.Seq != seq {
		t.Fatalf("unrelated deletion erased counterexample: %+v", got)
	}
}
