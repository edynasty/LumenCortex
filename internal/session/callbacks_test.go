package session

import (
	"context"
	"errors"
	"testing"
)

func TestCallbackSettlementPreservesPairingAndTerminalState(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))
	if err := store.AdmitExecution(ctx, execAdmission("s", "callback-1", "r")); err != nil {
		t.Fatal(err)
	}
	rows := count(t, store, `SELECT count(*) FROM session_messages`)
	result := ExecutionResult{ExecutionID: "callback-1", Outcome: OutcomeComplete, Coverage: CoverageComplete, PostRef: "git:def456"}
	if err := store.SettleExecution(ctx, "wrong-session", result); err == nil {
		t.Fatal("another session settled effect")
	}
	if err := store.SettleExecution(ctx, "s", result, fileEdge("data.json")); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Execution(ctx, "callback-1")
	if err != nil || rec.Outcome != OutcomeComplete || rec.PostRef != result.PostRef {
		t.Fatalf("settlement: %#v %v", rec, err)
	}
	if count(t, store, `SELECT count(*) FROM session_messages`) != rows {
		t.Fatal("callback appended unmatched tool row")
	}
	if count(t, store, `SELECT count(*) FROM derived_origins WHERE kind='file' AND execution_id='callback-1'`) != 1 {
		t.Fatal("callback lost file provenance")
	}
	if err := store.SettleExecution(ctx, "s", result); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("terminal effect rewritten: %v", err)
	}
}

func TestCallbackSettlementRollsBackOnProvenanceFailure(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))
	if err := store.AdmitExecution(ctx, execAdmission("s", "callback-2", "r")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TRIGGER callback_boom BEFORE INSERT ON derived_origins WHEN NEW.kind='file' BEGIN SELECT RAISE(ABORT,'callback_boom'); END`); err != nil {
		t.Fatal(err)
	}
	result := ExecutionResult{ExecutionID: "callback-2", Outcome: OutcomeComplete, Coverage: CoverageComplete}
	if err := store.SettleExecution(ctx, "s", result, fileEdge("data.json")); err == nil {
		t.Fatal("failed provenance insert ignored")
	}
	rec, err := store.Execution(ctx, "callback-2")
	if err != nil || rec.Outcome != OutcomeAdmitted {
		t.Fatalf("partial transaction persisted: %#v %v", rec, err)
	}
}

func TestCallbackSettlementHonorsFence(t *testing.T) {
	ctx := context.Background()
	store := provStore(t)
	gapSession(t, store, "s", "idle", "")
	admit(t, store, mainAdmission("s", "r"))
	if err := store.AdmitExecution(ctx, execAdmission("s", "callback-fenced", "r")); err != nil {
		t.Fatal(err)
	}
	addFence(t, store, "session", "s")
	rows := count(t, store, `SELECT count(*) FROM session_messages`)
	result := ExecutionResult{ExecutionID: "callback-fenced", Outcome: OutcomeComplete, Coverage: CoverageComplete}
	if err := store.SettleExecution(ctx, "s", result, fileEdge("data.json")); !errors.Is(err, ErrFenced) {
		t.Fatalf("fence ignored: %v", err)
	}
	rec, err := store.Execution(ctx, result.ExecutionID)
	if err != nil || rec.Outcome != OutcomeComplete || rec.LineageComplete {
		t.Fatalf("fenced result not settled: %#v %v", rec, err)
	}
	if count(t, store, `SELECT count(*) FROM derived_origins WHERE kind='file' AND execution_id='callback-fenced'`) != 0 {
		t.Fatal("fenced provenance committed")
	}
	if count(t, store, `SELECT count(*) FROM session_messages`) != rows {
		t.Fatal("fenced callback added a transcript row")
	}
}
