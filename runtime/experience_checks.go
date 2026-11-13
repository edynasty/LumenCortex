package runtime

import (
	"context"
	"slices"

	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// All conditions and the validation outcome are observed in one closed episode. If a later check
// changes the workspace, none of the earlier conditions can qualify a mixed-state promotion.
func (e *Engine) observeExperienceAssertions(ctx context.Context, id string, assertions []CognitiveAssertion, scope CognitiveScope, source knowledge.SourceRef) ([]ExperienceObservation, error) {
	if len(assertions) == 0 || len(assertions) > knowledge.MaxExperienceConditions+1 {
		return nil, session.ErrNotQualified
	}
	specs, _, _, err := e.approvedChecks(ctx)
	if err != nil {
		return nil, err
	}
	bindings, err := e.store.ActiveConstraintBindings(ctx, e.projectID())
	if err != nil {
		return nil, err
	}
	selected := make([]session.ApprovalRecord, len(assertions))
	selectedSpecs := make([]evaluator.Spec, len(assertions))
	ids := make([]string, 0, len(assertions))
	for i, a := range assertions {
		binding, spec, _ := matchBinding(bindings, specs, taskstate.ConstraintUse{Ref: a.Ref(), Digest: a.ValueDigest(), Scope: session.SourceScopeProject})
		if binding == nil {
			return nil, session.ErrNotQualified
		}
		selected[i], selectedSpecs[i] = *binding, spec
		if !slices.Contains(ids, binding.CheckID) {
			ids = append(ids, binding.CheckID)
		}
	}
	verdicts, err := e.decideForCognition(ctx, id, specs, ids, maintenanceVerifyBudget)
	if err != nil {
		return nil, err
	}
	out := make([]ExperienceObservation, 0, len(assertions))
	for i, a := range assertions {
		index := slices.IndexFunc(verdicts, func(v evaluator.Verdict) bool { return v.Result.Check == selected[i].CheckID })
		if index < 0 || verdicts[index].Result.Seq == 0 {
			return nil, session.ErrNotQualified
		}
		ev, err := e.cognitiveCheckEvidence(ctx, id, verdicts[index].Result.Seq, a, scope, source, selected[i], selectedSpecs[i])
		if err != nil {
			return nil, err
		}
		out = append(out, ExperienceObservation{Assertion: a, Evidence: ev})
	}
	return out, nil
}
