package knowledge

import (
	"testing"
	"time"
)

// FFR-002. An adoption made by a tool is bound to the run that made it (RunID; the runtime records
// the same run as PreviousRunID). Only that run can have applied it. A later run of the consumer may
// close an adoption its run left open, and only as never applied: it cannot credit or blame the
// strategy with an outcome observed in another run.
func TestExperienceRunBoundAdoptionIsClosedOnlyUnappliedByALaterRun(t *testing.T) {
	consumer := "consumer-session"
	bound := func() (Experience, *ExperienceUse) {
		x := experienceTestExperience()
		u := experienceTestUse(x.Definition, x)
		u.RunID, u.PreviousRunID = "consumer-adopting-run", "consumer-adopting-run"
		x.Uses = []ExperienceUse{u}
		return x, &x.Uses[0]
	}
	sameRun := experienceTestSource(consumer)
	sameRun.RunID = "consumer-adopting-run"
	laterRun := experienceTestSource(consumer)
	if laterRun.RunID == sameRun.RunID {
		t.Fatal("fixture: the later source must name another run")
	}

	x, u := bound()
	u.Feedback.Source = sameRun
	u.Feedback.Observation = experienceTestObservationPtr(u.ExpectedOutcome, sameRun, consumer, "", "passed")
	if err := x.Validate(); err != nil {
		t.Fatalf("the adopting run's own checked success was refused: %v", err)
	}

	x, u = bound()
	if u.Feedback.Source != laterRun || !u.Feedback.Used {
		t.Fatal("fixture: the default feedback is an executed use from the later run")
	}
	if err := x.Validate(); err == nil {
		t.Fatal("an executed outcome from another run was credited to a run-bound adoption")
	}

	x, u = bound()
	u.Feedback = &ExperienceFeedback{Source: laterRun, Outcome: "unknown", Reason: "adoption_run_left_unsettled", At: experienceTestNow.Add(time.Hour)}
	if err := x.Validate(); err != nil {
		t.Fatalf("a later run could not close the dead adoption as never applied: %v", err)
	}
	if stats := x.Statistics(); stats.Unknown != 1 || stats.Unconfirmed != 0 || stats.Successes != 0 || stats.Failures != 0 {
		t.Fatalf("statistics = %+v", stats)
	}

	x, u = bound()
	u.Feedback = &ExperienceFeedback{Source: laterRun, Outcome: "unknown", Reason: "adoption_run_left_unsettled", At: experienceTestNow.Add(time.Hour)}
	u.Feedback.Source.Digest = u.BaselineDigest
	if err := x.Validate(); err == nil {
		t.Fatal("a source at the adoption baseline closed the adoption")
	}
}
