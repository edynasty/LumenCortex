package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

// FFR-008. The origin's only action failed and the expected artifact was already there. The outcome
// check passes without the procedure, so nothing was learned: validated, the record let a consumer
// repeat the failing command and be credited with a successful use.
func TestExperienceFailedOriginActionIsNotAValidatedProcedure(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	expInstallChecks(t, f)
	writeFile(t, filepath.Join(f.root, "build/reused"), "already here")
	origin := expNewSession(t, f, "procedure whose action failed")
	result, err := f.engine.runAgentWith(ctx, origin, expCommandThenFinal("false", "I wrote the marker"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("origin %+v %v", result, err)
	}
	n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("procedure whose action failed")))
	x := n.Experience
	if len(x.Origin.Trace.Steps) != 1 || x.Origin.Trace.Steps[0].Outcome != "failed" || x.Origin.Trace.Incomplete {
		t.Fatalf("fixture: the origin action did not settle as failed: %+v", x.Origin.Trace)
	}
	if x.State != knowledge.ExperienceRejected || x.Validation.Reason != "failed_origin_action_is_not_success" || x.Validation.Outcome != nil {
		t.Fatalf("a failed action was validated as a successful procedure: state=%s validation=%+v", x.State, x.Validation)
	}
	consumer := expNewSession(t, f, "consumer of a failed procedure")
	if _, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version); !errors.Is(err, ErrExperienceNotApplicable) {
		t.Fatalf("a rejected procedure was adopted: %v", err)
	}
	if matches, err := f.engine.RecallExperiences(ctx, consumer, "procedure whose action failed", 8); err != nil || len(matches) != 1 || matches[0].State != knowledge.ExperienceRejected {
		t.Fatalf("recall = %+v, %v", matches, err)
	}
}

// A failed action still supports the lesson that the procedure fails: the negative Experience of the
// same origin validates against its failing outcome check.
func TestExperienceFailedOriginActionStillSupportsAWarning(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	expInstallChecks(t, f)
	origin := expNewSession(t, f, "procedure that is known to fail")
	result, err := f.engine.runAgentWith(ctx, origin, expCommandThenFinal("false", "it failed"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("origin %+v %v", result, err)
	}
	warning := expProposal("procedure that is known to fail")
	warning.ExpectedResult = "failed"
	n := expValidate(t, f, origin, expExtract(t, f, origin, warning))
	if n.Experience.State != knowledge.ExperienceValidated || n.Experience.Validation.Outcome == nil || n.Experience.Validation.Outcome.Evidence.Verification != "failed" {
		t.Fatalf("the warning did not validate: state=%s validation=%+v", n.Experience.State, n.Experience.Validation)
	}
}
