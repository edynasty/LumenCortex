package knowledge

import "testing"

// FFR-008. A validated record with a positive expected result cannot rest on an origin action that
// failed; the same trace still supports a warning (expected result "failed").
func TestExperiencePositivePromotionRequiresNoFailedOriginAction(t *testing.T) {
	step := func(outcome string) ExperienceStep {
		return ExperienceStep{ExecutionID: "ex-1", RequestID: "rq-1", Tool: "bash", ArgsDigest: Digest("args"), Outcome: outcome, Coverage: "complete", LineageComplete: true}
	}
	x := experienceTestExperience()
	x.Origin.Trace.Steps = []ExperienceStep{step("completed")}
	if err := x.Validate(); err != nil {
		t.Fatalf("a completed procedure was refused: %v", err)
	}
	if x.Origin.Trace.Failed() {
		t.Fatal("a completed procedure is reported as failed")
	}

	x.Origin.Trace.Steps = []ExperienceStep{step("completed"), step("failed")}
	x.Origin.Trace.Steps[1].ExecutionID = "ex-2"
	if !x.Origin.Trace.Failed() {
		t.Fatal("a failed action is not reported")
	}
	if err := x.Validate(); err == nil {
		t.Fatal("a positive promotion rests on a failed origin action")
	}

	// The same trace as the origin of a warning: the expected result is the failing check.
	x.Definition.ExpectedResult = "failed"
	source := x.Origin.Source
	x.Validation.Outcome = experienceTestObservationPtr(x.Definition.ExpectedOutcome, source, "origin-session", "origin-run", "failed")
	if err := x.Validate(); err != nil {
		t.Fatalf("a warning with a failed origin action was refused: %v", err)
	}

	// A record that is not promoted may keep the failed trace as history.
	x = experienceTestExperience()
	x.Origin.Trace.Steps = []ExperienceStep{step("failed")}
	x.State, x.Validation = ExperienceRejected, ExperienceValidation{Method: ExperienceMethod, Reason: "failed_origin_action_is_not_success"}
	x.Confidence = x.ConditionalConfidence()
	if err := x.Validate(); err != nil {
		t.Fatalf("a rejected record with a failed trace was refused: %v", err)
	}
}
