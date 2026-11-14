package runtime

import (
	"context"
	"errors"
	"slices"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

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
	if x.State != knowledge.ExperienceValidated || consumerID == x.Origin.Source.SessionID || len(x.Uses) >= knowledge.MaxExperienceUses {
		return ExperienceAdoption{}, ErrExperienceNotApplicable
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

func (e *Engine) recordExperienceFeedback(ctx context.Context, consumerID, id, adoptionID string) (CognitiveNode, error) {
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
		return CognitiveNode{}, errors.New("feedback requires a later independent consumer run")
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
		ev, err := e.verifyAssertion(ctx, consumerID, u.ExpectedOutcome, n.Scope, origin.Source)
		if err != nil && !errors.Is(err, session.ErrNotQualified) {
			return CognitiveNode{}, err
		}
		if err == nil {
			feedback.Observation = &knowledge.ExperienceObservation{Assertion: u.ExpectedOutcome, Evidence: ev}
			switch {
			case ev.Verification != u.ExpectedResult:
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
	return e.commitExperienceRevision(ctx, consumerID, n, n.Version)
}
