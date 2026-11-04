package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// constraintStart is a thread with one selected constraint bound to the check "gate", whose command
// marks that it started and then waits - for a file to appear, or a fixed time - before it passes.
type constraintStart struct {
	engine  *Engine
	id      string
	started string
	release string
	late    string
	version int64
}

func newConstraintStart(t *testing.T, command string) constraintStart {
	t.Helper()
	dir := t.TempDir()
	c := constraintStart{started: filepath.Join(dir, "started"), release: filepath.Join(dir, "release"), late: filepath.Join(dir, "late")}
	cmd := os.Expand(command, func(k string) string {
		return map[string]string{"STARTED": c.started, "RELEASE": c.release, "LATE": c.late}[k]
	})
	engine, _ := trustedProject(t, `{"gate": {"kind": "test", "command": "`+cmd+`", "required": false}}`, false)
	approveChecks(t, engine)
	bindConstraint(t, engine, testConstraintRef, testConstraintDigest, "gate", testConstraintScope)
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	selectConstraint(t, engine, handle.ID, testConstraintRef, testConstraintDigest, testConstraintScope)
	info, err := engine.TaskState(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.engine, c.id, c.version = engine, handle.ID, info.Version
	return c
}

// waitFile waits until path exists.
func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// nothingCommitted asserts that the refused Start left the thread as it was: no new Task State
// version, the constraint undecided, the session not running, no request admitted and no passing
// result of the gate a later run could take.
func (c constraintStart) nothingCommitted(t *testing.T) {
	t.Helper()
	info, err := c.engine.TaskState(context.Background(), c.id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != c.version {
		t.Fatalf("Task State version %d -> %d after a refused Start", c.version, info.Version)
	}
	if u := info.State.Constraints[0]; u.Status == taskstate.ConstraintSatisfied || u.RunID != "" || u.EpisodeID != "" {
		t.Fatalf("constraint after a refused Start: %+v", u)
	}
	row, err := c.engine.store.Get(context.Background(), c.id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Status == "running" {
		t.Fatal("the session is running after a refused Start")
	}
	if n := countRows(t, c.engine, `SELECT count(*) FROM request_envelopes WHERE session_id = ?`, c.id); n != 0 {
		t.Fatalf("%d requests admitted for a refused Start", n)
	}
}

// A Start cancelled while a check bound to a constraint runs ends with the cancellation: the check's
// process is stopped (its later write never lands), the episode is not a pass, nothing of the Start
// is committed, and the next Start decides the constraint fresh.
func TestCancelledStartDuringAConstraintCheckSavesNothing(t *testing.T) {
	c := newConstraintStart(t, `touch ${STARTED}; if [ ! -f ${RELEASE} ]; then sleep 3; touch ${LATE}; fi; true`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFile(t, c.started)
		cancel()
	}()
	began := time.Now()
	_, err := c.engine.runAgentWith(ctx, c.id, &answering{final: "done"}, AgentOptions{Input: "go", MaxSteps: 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Start: %v", err)
	}
	if took := time.Since(began); took > 3*time.Second {
		t.Fatalf("the cancelled Start waited %v for the check", took)
	}
	c.nothingCommitted(t)
	if n := passedRuntimeResults(t, c.engine, c.id, "gate"); n != 0 {
		t.Fatalf("%d passes stored by a cancelled check", n)
	}
	time.Sleep(3500 * time.Millisecond)
	if _, err := os.Stat(c.late); !os.IsNotExist(err) {
		t.Fatalf("the cancelled check kept running: %v", err)
	}

	if err := os.WriteFile(c.release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runOnce(t, c.engine, c.id); r.Status != "completed" {
		t.Fatalf("next Start: %+v", r)
	}
	if u := constraintUses(t, c.engine, c.id)[0]; u.Status != taskstate.ConstraintSatisfied || passedRuntimeResults(t, c.engine, c.id, "gate") != 1 {
		t.Fatalf("next Start did not decide the constraint fresh: %+v", u)
	}
}

// Revocation cancels a tracked preparation immediately: no constraint pass, request admission or
// running session may commit after the fence.
func TestRevokedDuringAConstraintCheckTheStartIsRefused(t *testing.T) {
	c := newConstraintStart(t, `touch ${STARTED}; while [ ! -f ${RELEASE} ]; do sleep 0.05; done; true`)
	revoked := make(chan error, 1)
	go func() {
		waitFile(t, c.started)
		_, err := c.engine.RevokeSource(context.Background(), SourceSession, c.id, "test")
		revoked <- err
		_ = os.WriteFile(c.release, nil, 0o644)
	}()
	_, err := c.engine.runAgentWith(context.Background(), c.id, &answering{final: "done"}, AgentOptions{Input: "go", MaxSteps: 2})
	if rerr := <-revoked; rerr != nil {
		t.Fatal(rerr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start of a thread revoked during its constraint check: %v", err)
	}
	c.nothingCommitted(t)
}

// A revocation of the thread committed after the Start projected the thread's fences and decided its
// constraints, but before the Start transaction, refuses the Start: the transaction is the
// linearization point, so no running session and no Task State version with the decision commit
// after the revocation.
func TestRevocationBeforeTheStartTransactionRefusesTheStart(t *testing.T) {
	c := newConstraintStart(t, `true`)
	testHookBeforeBeginRun = func() {
		if _, err := c.engine.store.RevokeSession(context.Background(), c.id, "test"); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookBeforeBeginRun = nil }()
	_, err := c.engine.runAgentWith(context.Background(), c.id, &answering{final: "done"}, AgentOptions{Input: "go", MaxSteps: 2})
	if !errors.Is(err, session.ErrFenced) {
		t.Fatalf("Start revoked before its transaction: %v", err)
	}
	c.nothingCommitted(t)
}

// A check set re-approved with the same set digest while a check bound to a constraint runs is a new
// approval epoch: the episode that straddles the change is not closed and its pass satisfies nothing
// (DATA_AND_FAILURES §3, ROADMAP §3 item 6; G-R1a R-1). The digest alone would not tell the two
// approvals apart; the epoch does.
func TestApprovalEpochChangedDuringAConstraintCheckSatisfiesNothing(t *testing.T) {
	c := newConstraintStart(t, `touch ${STARTED}; while [ ! -f ${RELEASE} ]; do sleep 0.05; done; true`)
	ctx := context.Background()
	before := approvalState(t, c.engine)
	reapproved := make(chan error, 1)
	go func() {
		waitFile(t, c.started)
		_, err := c.engine.RevokeApproval(ctx, RevokeApprovalRequest{Kind: RevokeChecks})
		if err == nil {
			_, err = c.engine.ApproveChecks(ctx, ApproveChecksRequest{SetDigest: before.SetDigest, ApprovedBy: ApprovedByUser, Actor: "u"})
		}
		reapproved <- err
		_ = os.WriteFile(c.release, nil, 0o644)
	}()
	if _, err := c.engine.runAgentWith(ctx, c.id, &answering{final: "rule:tests-pass is satisfied"}, AgentOptions{Input: "go", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	if err := <-reapproved; err != nil {
		t.Fatal(err)
	}
	after := approvalState(t, c.engine)
	if !after.Approved || after.SetDigest != before.SetDigest || after.Epoch <= before.Epoch {
		t.Fatalf("fixture: approval %+v after %+v, want the same digest under a newer epoch", after, before)
	}
	if u := constraintUses(t, c.engine, c.id)[0]; u.Status == taskstate.ConstraintSatisfied {
		t.Fatalf("a pass across an approval epoch change satisfied the constraint: %+v", u)
	}
	rows, err := c.engine.CheckResults(ctx, c.id, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Check == "gate" && r.EpisodeStatus == evaluator.EpisodeClosed {
			t.Fatalf("the episode across the epoch change is closed: %+v", r)
		}
	}
}
