package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// errExperienceUsesSpent refuses the adoption of a record that can take no further use
// (knowledge.Node.AdmitsExperienceUse): it already holds MaxExperienceUses uses, or its bytes cannot
// hold another use together with the feedback of every use that is still open. Both bounds are on
// the record, not a condition a consumer can change; recall reports such a record as
// use_limit_reached.
var errExperienceUsesSpent = fmt.Errorf("%w: the record can hold no further use (at most %d uses with their feedback in %d bytes)", ErrExperienceNotApplicable, knowledge.MaxExperienceUses, knowledge.MaxGraphRecordBytes)

// AdoptExperience checks the consumer's conditions before returning strategy text. It records a
// one-use identity and provenance first; it never executes commands, modifies Workflow or bypasses
// RunAgent's ordinary approvals. Repeat adoption at the same consumer version is refused, not replayed.
func (e *Engine) AdoptExperience(ctx context.Context, consumerID, id string, expected int64) (ExperienceAdoption, error) {
	lock, err := e.acquireRun(ctx, consumerID)
	if err != nil {
		return ExperienceAdoption{}, err
	}
	defer lock.Release()
	return e.adoptExperience(ctx, consumerID, id, expected, "")
}

func (e *Engine) adoptExperience(ctx context.Context, consumerID, id string, expected int64, runID string) (ExperienceAdoption, error) {
	n, err := e.readExperience(ctx, consumerID, id, 0)
	if err != nil {
		return ExperienceAdoption{}, err
	}
	if n.Version != expected {
		return ExperienceAdoption{}, session.ErrCognitiveRevision
	}
	x := n.Experience
	if x.State != knowledge.ExperienceValidated || consumerID == x.Origin.Source.SessionID {
		return ExperienceAdoption{}, ErrExperienceNotApplicable
	}
	if !n.AdmitsExperienceUse() {
		return ExperienceAdoption{}, errExperienceUsesSpent
	}
	if err := e.experienceCurrent(ctx, n); err != nil {
		return ExperienceAdoption{}, err
	}
	source, err := e.store.ExperienceConsumerForRun(ctx, consumerID, runID)
	if err != nil {
		return ExperienceAdoption{}, err
	}
	for _, u := range x.Uses {
		if u.ConsumerID == consumerID && u.BaselineDigest == source.Digest {
			return ExperienceAdoption{}, errors.New("experience already adopted for this consumer version")
		}
	}
	conditions, err := e.observeExperienceConditions(ctx, consumerID, x.Definition.Preconditions, n.Scope, source)
	if err != nil {
		return ExperienceAdoption{}, err
	}
	useID, err := newCandidateID()
	if err != nil {
		return ExperienceAdoption{}, err
	}
	u := knowledge.ExperienceUse{ID: useID, ConsumerID: consumerID, RunID: runID, BaselineDigest: source.Digest, PreviousRunID: source.RunID,
		DefinitionVersion: x.DefinitionVersion, DefinitionDigest: x.Definition.Digest(), ExpectedOutcome: x.Definition.ExpectedOutcome, ExpectedResult: x.Definition.ExpectedResult, Conditions: conditions, AdoptedAt: e.now()}
	x.Uses = append(x.Uses, u)
	if err := e.experienceCurrent(ctx, n); err != nil {
		return ExperienceAdoption{}, err
	}
	n, err = e.commitExperienceRevision(ctx, consumerID, n, expected)
	if err != nil {
		return ExperienceAdoption{}, err
	}
	return ExperienceAdoption{ID: useID, ExperienceID: n.ID, Version: n.Version, DefinitionVersion: x.DefinitionVersion,
		Strategy: slices.Clone(x.Definition.Strategy), Conditions: conditions, ExpectedOutcome: x.Definition.ExpectedOutcome, ExpectedResult: x.Definition.ExpectedResult, Uncertainty: x.Uncertainty}, nil
}

// errExperienceFeedbackNotDue reports an open use whose consumer has not ended a run since the
// adoption: nothing can have been applied or observed yet. A host that asks for feedback gets this
// error; the memory settlement of a run or of an Engine open leaves the use open without one.
var errExperienceFeedbackNotDue = errors.New("feedback requires a later independent consumer run")

// RecordExperienceFeedback requires a distinct terminal consumer run and an
// explicit application with the complete sequence of settled execution IDs.
// A tool failure can support a checked counterexample; admission, denial and
// partial/interrupted execution stay unknown. A negative example is a warning.
func (e *Engine) RecordExperienceFeedback(ctx context.Context, consumerID, id, adoptionID string) (CognitiveNode, error) {
	lock, err := e.acquireRun(ctx, consumerID)
	if err != nil {
		return CognitiveNode{}, err
	}
	defer lock.Release()
	return e.recordExperienceFeedback(ctx, consumerID, id, adoptionID)
}

// experienceSettlementAttempts bounds how often one settlement is committed again after another
// writer revised the record. Each conflict is one commit of another writer, and a record holds at
// most MaxExperienceUses uses: every other use can conflict once as an adoption and once as a
// settlement. The remainder is room for host lifecycle changes.
const experienceSettlementAttempts = 2*knowledge.MaxExperienceUses + 4

// recordExperienceFeedback settles one open use. One record holds every consumer's use of an
// Experience, so another consumer's adoption or settlement, or a host lifecycle change, can commit
// between this read and this commit. That conflict is benign: the settlement is recomputed against
// the newer revision. Left open, the use could only be closed as unknown by a later run of the
// consumer, and the checked result, a counterexample included, would be lost.
//
// No check runs here. The outcome was observed when the application ended (cognitiveAccess.apply)
// and is read from its receipt, however often and however late the settlement is made.
func (e *Engine) recordExperienceFeedback(ctx context.Context, consumerID, id, adoptionID string) (CognitiveNode, error) {
	for attempt := 1; ; attempt++ {
		n, err := e.settleExperienceUse(ctx, consumerID, id, adoptionID)
		if attempt == experienceSettlementAttempts || !errors.Is(err, session.ErrCognitiveRevision) {
			return n, err
		}
	}
}

// testHookBeforeExperienceSettlement runs between a settlement's read of the record and its commit
// (nil outside tests): the point where another writer's revision makes the commit conflict.
var testHookBeforeExperienceSettlement func()

func (e *Engine) settleExperienceUse(ctx context.Context, consumerID, id, adoptionID string) (CognitiveNode, error) {
	n, err := e.readExperience(ctx, consumerID, id, 0)
	if err != nil {
		return CognitiveNode{}, err
	}
	x := n.Experience
	index := slices.IndexFunc(x.Uses, func(u knowledge.ExperienceUse) bool { return u.ID == adoptionID && u.ConsumerID == consumerID })
	if index < 0 {
		return CognitiveNode{}, experienceMissing(adoptionID)
	}
	u := &x.Uses[index]
	if u.Feedback != nil {
		return experienceForReader(n, consumerID), nil
	}
	origin, err := e.store.ExperienceOrigin(ctx, consumerID)
	if err != nil {
		return CognitiveNode{}, err
	}
	// A host adoption is settled by a run after its baseline. A tool adoption is settled by the run
	// that made it; when that run ended without a terminal status (interrupted, waiting at a gate) or
	// its settlement failed, the adoption can never be applied again and the consumer's next terminal
	// run closes it as unknown instead of leaving it unconfirmed forever.
	left := u.RunID != "" && origin.Source.RunID != u.RunID
	if origin.Source.Digest == u.BaselineDigest || (u.RunID == "" && origin.Source.RunID == u.PreviousRunID) || origin.CompletedAt.Before(u.AdoptedAt) {
		return CognitiveNode{}, errExperienceFeedbackNotDue
	}
	application, err := e.store.ExperienceApplication(ctx, consumerID, adoptionID)
	if err != nil && !errors.Is(err, session.ErrNotFound) {
		return CognitiveNode{}, err
	}
	executed := err == nil && !left && application.Executed() && application.RunID == origin.Source.RunID && application.DefinitionVersion == u.DefinitionVersion
	feedback := &knowledge.ExperienceFeedback{Source: origin.Source, Used: executed, ConditionsHeld: executed, Outcome: "unknown", Reason: "experience_application_unconfirmed", At: e.now()}
	if left {
		feedback.Reason = "adoption_run_left_unsettled"
	}
	if executed {
		feedback.ApplicationID = application.ID
		feedback.Reason = "outcome_not_qualified"
		// The observation is evidence only while its approval stands. One that lapsed since the
		// application (a revoked binding, a privacy epoch) is not replaced by a check made now.
		qualified := false
		if o := application.Outcome; o != nil && o.Assertion == u.ExpectedOutcome {
			if qualified, err = e.store.CognitiveEvidenceQualified(ctx, e.projectID(), o.Assertion, o.Evidence); err != nil {
				return CognitiveNode{}, err
			}
		}
		if qualified {
			feedback.Observation = application.Outcome
			switch {
			case application.Outcome.Evidence.Verification != u.ExpectedResult:
				feedback.Outcome, feedback.Reason, feedback.RevisionNeeded = "failure", "outcome_counterexample", true
			case !application.Reproduced(x.Origin.Trace.Steps):
				// The expected outcome holds, but an action did not settle as it did in the origin: the
				// outcome was not produced by the observed procedure (a pre-existing artifact, another
				// actor). It is neither a success of the strategy nor a counterexample.
				feedback.Reason = "application_diverged_from_origin"
			case origin.Outcome == "completed":
				feedback.Outcome, feedback.Reason = "success", "expected_outcome_verified"
			default:
				feedback.Reason = "consumer_run_failed_with_matching_check"
			}
		}
	}
	u.Feedback = feedback
	if feedback.Outcome == "failure" && u.DefinitionVersion == x.DefinitionVersion {
		x.State = knowledge.ExperienceNeedsRevision
	}
	if testHookBeforeExperienceSettlement != nil {
		testHookBeforeExperienceSettlement()
	}
	return e.commitExperienceRevision(ctx, consumerID, n, n.Version)
}
