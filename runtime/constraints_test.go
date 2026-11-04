package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

const (
	testConstraintRef    = "rule:tests-pass"
	testConstraintDigest = "sha256:1111"
	testConstraintScope  = "project"
)

// selectConstraint selects a constraint for the thread as the user.
func selectConstraint(t *testing.T, engine *Engine, sessionID, ref, digest, scope string) {
	t.Helper()
	edit := TaskStateEdit{Constraint: &taskstate.ConstraintEdit{Action: taskstate.ConstraintSelect, Ref: ref, Digest: digest, Scope: scope, Actor: "u"}}
	if _, err := engine.EditTaskState(context.Background(), sessionID, edit); err != nil {
		t.Fatal(err)
	}
}

// bindConstraint approves the binding of a constraint to check as the user, for the check as it is now.
func bindConstraint(t *testing.T, engine *Engine, ref, digest, check, scope string) ApprovalRecord {
	t.Helper()
	specs, _, err := engine.effectiveChecks()
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := findSpec(specs, check)
	if !ok {
		t.Fatalf("no check %s", check)
	}
	r, err := engine.ApproveConstraintBinding(context.Background(), ApproveBindingRequest{ConstraintRef: ref, ConstraintDigest: digest,
		CheckID: check, SpecDigest: evaluator.SpecDigest(spec), Scope: scope, ApprovedBy: ApprovedByUser, Actor: "u"})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func constraintUses(t *testing.T, engine *Engine, sessionID string) []taskstate.ConstraintUse {
	t.Helper()
	info, err := engine.TaskState(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Constraints
}

// runOnce runs one Start on the thread with a model that claims, in its final answer, that every
// constraint is met.
func runOnce(t *testing.T, engine *Engine, sessionID string) AgentResult {
	t.Helper()
	r, err := engine.runAgentWith(context.Background(), sessionID, &answering{final: "rule:tests-pass is satisfied; all constraints are met"}, AgentOptions{Input: "go", MaxSteps: 2})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// A constraint is satisfied only through a binding the user approved for its ref, digest and scope, to
// a check approved as it is now, passing in a closed episode run at the Start; the model's word counts
// for nothing. A loose check that passes but is not bound does not satisfy it, and nothing is run for a
// constraint without an approved binding or with an unapproved check.
func TestConstraintIsSatisfiedOnlyThroughAnApprovedBinding(t *testing.T) {
	const checks = `{"loose": {"kind": "test", "command": "true", "required": false}, "strict": {"kind": "test", "command": "true", "required": false}, "red": {"kind": "test", "command": "exit 1", "required": false}}`
	for _, c := range []struct {
		name    string
		setup   func(t *testing.T, engine *Engine, root string)
		status  string
		reason  string
		ran     string // the check that ran, "" for none
		episode bool
	}{
		{"no binding", func(t *testing.T, e *Engine, _ string) { approveChecks(t, e) }, taskstate.ConstraintUnbound, "no approved binding", "", false},
		{"binding of another constraint digest", func(t *testing.T, e *Engine, _ string) {
			approveChecks(t, e)
			bindConstraint(t, e, testConstraintRef, "sha256:2222", "loose", testConstraintScope)
		}, taskstate.ConstraintUnbound, "no approved binding", "", false},
		{"binding for another scope", func(t *testing.T, e *Engine, _ string) {
			approveChecks(t, e)
			bindConstraint(t, e, testConstraintRef, testConstraintDigest, "loose", "worktree:other")
		}, taskstate.ConstraintUnbound, "no approved binding", "", false},
		{"bound check redefined since", func(t *testing.T, e *Engine, root string) {
			bindConstraint(t, e, testConstraintRef, testConstraintDigest, "strict", testConstraintScope)
			setProjectChecks(t, root, strings.Replace(checks, `"strict": {"kind": "test", "command": "true"`, `"strict": {"kind": "test", "command": "exit 0"`, 1))
			approveChecks(t, e)
		}, taskstate.ConstraintUnbound, "definition changed", "", false},
		{"bound check not approved", func(t *testing.T, e *Engine, _ string) {
			bindConstraint(t, e, testConstraintRef, testConstraintDigest, "strict", testConstraintScope)
		}, taskstate.ConstraintSpecUnapproved, "", "", false},
		{"bound, approved, failing", func(t *testing.T, e *Engine, _ string) {
			approveChecks(t, e)
			bindConstraint(t, e, testConstraintRef, testConstraintDigest, "red", testConstraintScope)
		}, taskstate.ConstraintFailed, "did not pass", "red", true},
		{"bound, approved, passing", func(t *testing.T, e *Engine, _ string) {
			approveChecks(t, e)
			bindConstraint(t, e, testConstraintRef, testConstraintDigest, "strict", testConstraintScope)
		}, taskstate.ConstraintSatisfied, "", "strict", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			engine, root := trustedProject(t, checks, false)
			ctx := context.Background()
			handle, err := engine.NewSession(ctx, SessionOptions{Goal: "g"})
			if err != nil {
				t.Fatal(err)
			}
			c.setup(t, engine, root)
			selectConstraint(t, engine, handle.ID, testConstraintRef, testConstraintDigest, testConstraintScope)
			if r := runOnce(t, engine, handle.ID); r.Status != "completed" {
				t.Fatalf("the run did not start and complete: %+v", r)
			}
			uses := constraintUses(t, engine, handle.ID)
			if len(uses) != 1 {
				t.Fatalf("uses %+v", uses)
			}
			u := uses[0]
			if u.Status != c.status || !strings.Contains(u.Reason, c.reason) || u.RunID == "" || u.DecidedAt == nil {
				t.Fatalf("use %+v, want %s (%q)", u, c.status, c.reason)
			}
			rows, err := engine.CheckResults(ctx, handle.ID, -1, 0)
			if err != nil {
				t.Fatal(err)
			}
			var ran []string
			for _, r := range rows {
				ran = append(ran, r.Check)
			}
			if want := []string{c.ran}; c.ran == "" && len(ran) != 0 || c.ran != "" && strings.Join(ran, ",") != strings.Join(want, ",") {
				t.Fatalf("checks run %v, want %q", ran, c.ran)
			}
			if !c.episode {
				if u.EpisodeID != "" || u.ResultSeq != 0 {
					t.Fatalf("an episode is named without a run: %+v", u)
				}
				return
			}
			r := rows[0]
			if u.EpisodeID == "" || u.EpisodeID != r.EpisodeID || u.ResultSeq != r.Seq || r.EpisodeStatus != evaluator.EpisodeClosed ||
				u.PreFingerprint == "" || u.PreFingerprint != u.PostFingerprint || u.Check != c.ran || u.ApprovalEpoch == 0 || u.BindingDigest == "" {
				t.Fatalf("use %+v does not name the closed episode %+v", u, r)
			}
		})
	}
}

// A check passing for a constraint at the Start counts for the rest of that run: the completion gate
// takes it without running it again. The next Start decides the constraint again, fresh, in its own
// run; it never takes the earlier run's pass.
func TestConstraintIsDecidedFreshByEveryStart(t *testing.T) {
	engine, _ := trustedProject(t, `{"gate": {"kind": "test", "command": "true"}}`, false)
	ctx := context.Background()
	approveChecks(t, engine)
	bindConstraint(t, engine, testConstraintRef, testConstraintDigest, "gate", testConstraintScope)
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	selectConstraint(t, engine, handle.ID, testConstraintRef, testConstraintDigest, testConstraintScope)
	if r := runOnce(t, engine, handle.ID); r.Status != "completed" {
		t.Fatalf("run 1: %+v", r)
	}
	first := constraintUses(t, engine, handle.ID)[0]
	if first.Status != taskstate.ConstraintSatisfied || passedRuntimeResults(t, engine, handle.ID, "gate") != 1 {
		t.Fatalf("run 1: %+v, %d passes", first, passedRuntimeResults(t, engine, handle.ID, "gate"))
	}
	if r := runOnce(t, engine, handle.ID); r.Status != "completed" {
		t.Fatalf("run 2: %+v", r)
	}
	second := constraintUses(t, engine, handle.ID)[0]
	if second.Status != taskstate.ConstraintSatisfied || second.RunID == first.RunID || second.EpisodeID == first.EpisodeID || second.ResultSeq <= first.ResultSeq {
		t.Fatalf("run 2 took run 1's pass: %+v then %+v", first, second)
	}
	if n := passedRuntimeResults(t, engine, handle.ID, "gate"); n != 2 {
		t.Fatalf("%d passes stored, want one per run", n)
	}
}

// CHECKS-001 end to end, through a constraint: a run that rewrites the check bound to its constraint
// cannot make the constraint pass. The rewrite revokes the set approval and the binding no longer names
// the definition; approving the rewritten set does not approve the binding; only a binding approved for
// the new definition makes it count again. A write that restores the bound definition byte for byte
// still leaves it unapproved until the user approves the set again.
func TestRewritingTheBoundCheckCannotSatisfyTheConstraint(t *testing.T) {
	original := `{"gate": {"kind": "test", "command": "exit 1", "required": false}}`
	rewritten := `{"gate": {"kind": "test", "command": "exit 0", "required": false}}`
	engine, _ := trustedProject(t, original, false)
	ctx := context.Background()
	approveChecks(t, engine)
	bindConstraint(t, engine, testConstraintRef, testConstraintDigest, "gate", testConstraintScope)
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "make the gate pass"})
	if err != nil {
		t.Fatal(err)
	}
	selectConstraint(t, engine, handle.ID, testConstraintRef, testConstraintDigest, testConstraintScope)
	start := func(step string, p *exitProvider, want, reason string) {
		t.Helper()
		if p == nil {
			p = writingProvider()
		}
		_, _ = engine.runAgentWith(ctx, handle.ID, p, AgentOptions{Input: step, Policy: "workspace", MaxSteps: 3})
		u := constraintUses(t, engine, handle.ID)[0]
		if u.Status != want || !strings.Contains(u.Reason, reason) {
			t.Fatalf("%s: %+v, want %s (%q)", step, u, want, reason)
		}
	}
	start("failing check", writingProvider(`{"checks": `+rewritten+`}`), taskstate.ConstraintFailed, "did not pass")
	start("after the rewrite", nil, taskstate.ConstraintUnbound, "definition changed")
	approveChecks(t, engine)
	start("rewritten set approved", nil, taskstate.ConstraintUnbound, "definition changed")
	if n := passedRuntimeResults(t, engine, handle.ID, "gate"); n != 0 {
		t.Fatalf("the rewritten check ran for the constraint: %d passes", n)
	}
	start("revert", writingProvider(`{"checks": `+original+`}`), taskstate.ConstraintUnbound, "definition changed")
	start("after the revert", nil, taskstate.ConstraintSpecUnapproved, "")
	approveChecks(t, engine)
	start("original set approved again", nil, taskstate.ConstraintFailed, "did not pass")

	setProjectChecks(t, engine.workspace, rewritten)
	approveChecks(t, engine)
	bindConstraint(t, engine, testConstraintRef, testConstraintDigest, "gate", testConstraintScope)
	start("binding approved for the new definition", nil, taskstate.ConstraintSatisfied, "")
}

// markerCheck is a check that appends a line to a file outside the workspace each time it runs, so it
// leaves the workspace (and its episode) unchanged.
func markerCheck(id, marker, extra string) string {
	return `"` + id + `": {"kind": "test", "command": "` + extra + `echo ` + id + ` >> ` + marker + `", "required": false}`
}

func markerLines(t *testing.T, marker string) []string {
	t.Helper()
	raw, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// A Start runs at most two checks for its constraints. A third is unknown and not run; a check bound to
// two constraints runs once for both; the user's deviation is kept and runs nothing; the run starts.
func TestConstraintRevalidationIsCapped(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	checks := "{" + markerCheck("a", marker, "") + ", " + markerCheck("b", marker, "") + ", " + markerCheck("c", marker, "") + ", " + markerCheck("d", marker, "") + "}"
	engine, _ := trustedProject(t, checks, false)
	ctx := context.Background()
	approveChecks(t, engine)
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct{ ref, check string }{{"r1", "a"}, {"r2", "a"}, {"r3", "b"}, {"r4", "c"}, {"r5", "d"}} {
		bindConstraint(t, engine, b.ref, testConstraintDigest, b.check, testConstraintScope)
		selectConstraint(t, engine, handle.ID, b.ref, testConstraintDigest, testConstraintScope)
	}
	deviate := TaskStateEdit{Constraint: &taskstate.ConstraintEdit{Action: taskstate.ConstraintDeviate, Ref: "r5", Digest: testConstraintDigest, Scope: testConstraintScope, Reason: "not this task", Actor: "u"}}
	if _, err := engine.EditTaskState(ctx, handle.ID, deviate); err != nil {
		t.Fatal(err)
	}
	if r := runOnce(t, engine, handle.ID); r.Status != "completed" {
		t.Fatalf("run: %+v", r)
	}
	if got := strings.Join(markerLines(t, marker), ","); got != "a,b" {
		t.Fatalf("checks run %q, want a,b", got)
	}
	want := map[string]string{"r1": taskstate.ConstraintSatisfied, "r2": taskstate.ConstraintSatisfied, "r3": taskstate.ConstraintSatisfied,
		"r4": taskstate.ConstraintUnknown, "r5": taskstate.ConstraintDeviation}
	for _, u := range constraintUses(t, engine, handle.ID) {
		if u.Status != want[u.Ref] {
			t.Fatalf("%s: %+v, want %s", u.Ref, u, want[u.Ref])
		}
	}
	uses := constraintUses(t, engine, handle.ID)
	if uses[0].EpisodeID != uses[1].EpisodeID || !strings.Contains(uses[3].Reason, "at most 2") || uses[3].EpisodeID != "" {
		t.Fatalf("shared or capped uses: %+v", uses)
	}
	if d := uses[4]; d.Deviation == nil || d.Deviation.Actor != "u" || d.RunID != "" {
		t.Fatalf("the deviation was decided over: %+v", d)
	}
}

// The checks a Start runs for its constraints share one time budget. A check still running when it runs
// out is stopped and its constraint is unknown; a check not yet started is not run; the run starts
// anyway, without waiting for the slow check.
func TestConstraintRevalidationBudgetDegradesToUnknown(t *testing.T) {
	saved := constraintRevalidation
	constraintRevalidation.budget = 500 * time.Millisecond
	t.Cleanup(func() { constraintRevalidation = saved })
	marker := filepath.Join(t.TempDir(), "ran")
	checks := "{" + markerCheck("slow", marker, "sleep 5; ") + ", " + markerCheck("next", marker, "") + "}"
	engine, _ := trustedProject(t, checks, false)
	ctx := context.Background()
	approveChecks(t, engine)
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	bindConstraint(t, engine, "r1", testConstraintDigest, "slow", testConstraintScope)
	bindConstraint(t, engine, "r2", testConstraintDigest, "next", testConstraintScope)
	selectConstraint(t, engine, handle.ID, "r1", testConstraintDigest, testConstraintScope)
	selectConstraint(t, engine, handle.ID, "r2", testConstraintDigest, testConstraintScope)
	began := time.Now()
	if r := runOnce(t, engine, handle.ID); r.Status != "completed" {
		t.Fatalf("run: %+v", r)
	}
	if took := time.Since(began); took > 4*time.Second {
		t.Fatalf("the run waited %s for the slow check", took)
	}
	uses := constraintUses(t, engine, handle.ID)
	for _, u := range uses {
		if u.Status != taskstate.ConstraintUnknown || !strings.Contains(u.Reason, "ran out") {
			t.Fatalf("%s: %+v", u.Ref, u)
		}
	}
	if lines := markerLines(t, marker); len(lines) != 0 {
		t.Fatalf("checks completed past the budget: %v", lines)
	}
}

// Only a selected constraint can be removed or deviated from, and the list is bounded. No edit marks a
// constraint satisfied: an edit naming another action is refused.
func TestConstraintEdits(t *testing.T) {
	engine, _ := trustedProject(t, `{}`, false)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	edit := func(action, ref, reason string) error {
		_, err := engine.EditTaskState(ctx, handle.ID, TaskStateEdit{Constraint: &taskstate.ConstraintEdit{Action: action, Ref: ref,
			Digest: testConstraintDigest, Scope: testConstraintScope, Reason: reason, Actor: "u"}})
		return err
	}
	if err := edit(taskstate.ConstraintRemove, "r1", ""); !errors.Is(err, taskstate.ErrUnknownConstraint) {
		t.Fatalf("removing an unselected constraint: %v", err)
	}
	if err := edit(taskstate.ConstraintDeviate, "r1", "x"); !errors.Is(err, taskstate.ErrUnknownConstraint) {
		t.Fatalf("deviating from an unselected constraint: %v", err)
	}
	if err := edit("satisfied", "r1", ""); err == nil {
		t.Fatal("an edit marked a constraint satisfied")
	}
	for i := range taskstate.MaxConstraints {
		if err := edit(taskstate.ConstraintSelect, "r"+strings.Repeat("x", i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := edit(taskstate.ConstraintSelect, "one-more", ""); err == nil {
		t.Fatal("selected past the bound")
	}
	if err := edit(taskstate.ConstraintDeviate, "r", ""); err == nil {
		t.Fatal("a deviation without a reason")
	}
	if err := edit(taskstate.ConstraintRemove, "r", ""); err != nil {
		t.Fatal(err)
	}
	uses := constraintUses(t, engine, handle.ID)
	if len(uses) != taskstate.MaxConstraints-1 || uses[0].Ref != "rx" || uses[0].Status != taskstate.ConstraintSelected {
		t.Fatalf("uses %+v", uses)
	}
}
