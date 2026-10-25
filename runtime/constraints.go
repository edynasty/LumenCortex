package runtime

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// constraintRevalidation bounds what a Start runs to decide the thread's constraints (DATA §3, plan
// T4-S3): at most max checks, within budget in all. A constraint past either is unknown and the run
// starts anyway; nothing scans the project's other checks.
var constraintRevalidation = struct {
	max    int
	budget time.Duration
}{max: 2, budget: 15 * time.Second}

// decideConstraints decides each constraint selected for the thread, as Start runID, in the session's
// current workspace (fresh-at-use, ADR-0017). A constraint is satisfied only through an active binding
// the host approved for its ref, digest and scope, to a check whose definition is still the bound one
// and is approved as it is now, passing in a closed episode run here. A stored pass from an earlier run
// or a person's earlier acceptance decides nothing; a user-accepted deviation is left as it is. A check
// bound to several constraints runs once. It fails only when ctx is cancelled: anything else that
// keeps a constraint from being decided leaves it unknown.
func (e *Engine) decideConstraints(ctx context.Context, runID string, state *taskstate.State, checks runChecks) error {
	if len(state.Constraints) == 0 {
		return nil
	}
	now := time.Now()
	undecided := func(reason string) {
		for i := range state.Constraints {
			state.DecideConstraint(i, runID, taskstate.ConstraintDecision{Status: taskstate.ConstraintUnknown, Reason: reason}, now)
		}
	}
	specs, _, _, err := e.approvedChecks(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		undecided("the check definitions could not be read: " + err.Error())
		return nil
	}
	bindings, err := e.store.ActiveConstraintBindings(ctx, e.projectID())
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		undecided("the constraint bindings could not be read: " + err.Error())
		return nil
	}
	budget, cancel := context.WithTimeout(ctx, constraintRevalidation.budget)
	defer cancel()
	type outcome struct {
		verdict evaluator.Verdict
		err     error
	}
	decided := map[string]outcome{}
	for i, u := range state.Constraints {
		if u.Status == taskstate.ConstraintDeviation {
			continue
		}
		binding, spec, stale := matchBinding(bindings, specs, u)
		if binding == nil {
			reason := "no approved binding of this constraint for this scope"
			if stale {
				reason = "the bound check's definition changed since the binding was approved"
			}
			state.DecideConstraint(i, runID, taskstate.ConstraintDecision{Status: taskstate.ConstraintUnbound, Reason: reason}, now)
			continue
		}
		d := taskstate.ConstraintDecision{BindingDigest: binding.BindingDigest, Check: spec.ID, SpecDigest: binding.SpecDigest, ApprovalEpoch: spec.ApprovalEpoch}
		o, ok := decided[spec.ID]
		switch {
		case ok:
		case len(decided) >= constraintRevalidation.max:
			d.Status, d.Reason = taskstate.ConstraintUnknown, fmt.Sprintf("not run: a Start runs at most %d checks for its constraints", constraintRevalidation.max)
			state.DecideConstraint(i, runID, d, now)
			continue
		case budget.Err() != nil:
			d.Status, d.Reason = taskstate.ConstraintUnknown, fmt.Sprintf("not run: the %s a Start spends on its constraints ran out", constraintRevalidation.budget)
			state.DecideConstraint(i, runID, d, now)
			continue
		default:
			verdicts, err := checks.Decide(budget, []string{spec.ID}, "")
			if err == nil && len(verdicts) != 1 {
				err = fmt.Errorf("deciding %s returned %d verdicts", spec.ID, len(verdicts))
			}
			if err == nil {
				o.verdict = verdicts[0]
			}
			o.err = err
			decided[spec.ID] = o
		}
		if o.err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			d.Status, d.Reason = taskstate.ConstraintUnknown, constraintErrorReason(o.err, budget)
			state.DecideConstraint(i, runID, d, now)
			continue
		}
		state.DecideConstraint(i, runID, constraintVerdict(d, o.verdict), now)
	}
	return nil
}

// matchBinding finds the active binding of u: same constraint ref and digest, approved for u's scope,
// to a configured check whose definition is the bound one. stale reports a binding of u whose check was
// redefined or removed since.
func matchBinding(bindings []session.ApprovalRecord, specs []evaluator.Spec, u taskstate.ConstraintUse) (*session.ApprovalRecord, evaluator.Spec, bool) {
	stale := false
	for i, b := range bindings {
		if b.ConstraintRef != u.Ref || b.ConstraintDigest != u.Digest ||
			b.BindingDigest != bindingDigest(u.Ref, u.Digest, b.CheckID, b.SpecDigest, u.Scope) {
			continue
		}
		spec, ok := findSpec(specs, b.CheckID)
		if !ok || evaluator.SpecDigest(spec) != b.SpecDigest {
			stale = true
			continue
		}
		return &bindings[i], spec, false
	}
	return nil, evaluator.Spec{}, stale
}

// constraintVerdict is the decision a check verdict makes: satisfied only by a pass the check ran for in
// this closed episode, at a fingerprint the episode held. A reason is fixed text and a result seq, never
// the check's output: the log stays in the eval result it names.
func constraintVerdict(d taskstate.ConstraintDecision, v evaluator.Verdict) taskstate.ConstraintDecision {
	r := v.Result
	episode := func() {
		d.EpisodeID, d.ResultSeq, d.PreFingerprint, d.PostFingerprint = r.EpisodeID, r.Seq, r.PreFingerprint, r.PostFingerprint
	}
	switch {
	case r.Status == evaluator.StatusSpecUnapproved:
		d.Status, d.Reason = taskstate.ConstraintSpecUnapproved, ErrSpecUnapproved.Error()
	case v.Reused:
		d.Status, d.Reason = taskstate.ConstraintUnknown, "an earlier acceptance decided the check without running it here"
	case v.Episode != evaluator.EpisodeClosed:
		d.Status, d.Reason = taskstate.ConstraintUnknown, "the check episode ended "+cmp.Or(v.Episode, "before the check ran")
		episode()
	case r.PreFingerprint == "" || r.PreFingerprint != r.PostFingerprint:
		d.Status, d.Reason = taskstate.ConstraintUnknown, "the workspace has no fingerprint the result can be tied to"
		episode()
	case v.Passed:
		d.Status = taskstate.ConstraintSatisfied
		episode()
	case r.Status == evaluator.StatusFailed:
		d.Status, d.Reason = taskstate.ConstraintFailed, fmt.Sprintf("the check did not pass (eval result %d)", r.Seq)
		episode()
	default:
		d.Status, d.Reason = taskstate.ConstraintUnknown, fmt.Sprintf("the check ended %s (eval result %d)", r.Status, r.Seq)
		episode()
	}
	return d
}

// constraintErrorReason explains a check that could not be decided.
func constraintErrorReason(err error, budget context.Context) string {
	switch {
	case budget.Err() != nil:
		return fmt.Sprintf("the %s a Start spends on its constraints ran out", constraintRevalidation.budget)
	case errors.Is(err, session.ErrFenced):
		return "the session is fenced: no check runs"
	}
	return "the check could not be decided: " + err.Error()
}
