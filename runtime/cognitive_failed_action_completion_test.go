package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/internal/knowledge"
)

func TestCompletionExecutedToolFailureBecomesCounterexample(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	expInstallChecks(t, f)
	command := "if test -f build/block; then rm -f build/reused; exit 1; else printf ready > build/reused; fi"
	origin := expNewSession(t, f, "conditional procedure with a real error")
	writeFile(t, filepath.Join(f.root, "build/fixture"), "procedure workspace")
	result, err := f.engine.runAgentWith(ctx, origin, expCommandThenFinal(command, "observed"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("origin %+v %v", result, err)
	}
	n := expValidate(t, f, origin, expExtract(t, f, origin, expProposal("conditional tool outcome")))
	consumer := expNewSession(t, f, "reuse under changed uncited condition")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(f.root, "build/block"), "blocked")
	result, err = f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": command}}}), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	receipt, err := f.engine.store.ExperienceApplication(ctx, consumer, adoption.ID)
	if err != nil || receipt.State != "failed" || receipt.Actions[0].ExecutionID == "" {
		t.Fatalf("failed execution not settled: %+v %v", receipt, err)
	}
	got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	fb := got.Experience.Uses[0].Feedback
	if fb == nil || !fb.Used || fb.Outcome != "failure" || !fb.RevisionNeeded || got.Experience.State != knowledge.ExperienceNeedsRevision {
		t.Fatalf("executed tool failure disappeared from learning: %+v", fb)
	}
	if _, err := os.Stat(filepath.Join(f.root, "build/reused")); !os.IsNotExist(err) {
		t.Fatalf("failed action did not run: %v", err)
	}
}

func TestCompletionObservedFailedActionSupportsNegativeExperience(t *testing.T) {
	f := newCognitionFixture(t, true)
	ctx := context.Background()
	expInstallChecks(t, f)
	command := "mkdir -p build; rm -f build/reused; exit 1"
	origin := expNewSession(t, f, "negative procedure observation")
	result, err := f.engine.runAgentWith(ctx, origin, expCommandThenFinal(command, "failure observed"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("origin %+v %v", result, err)
	}
	proposal := expProposal("avoid this failing procedure")
	proposal.ExpectedResult = "failed"
	n := expValidate(t, f, origin, expExtract(t, f, origin, proposal))
	if n.Experience.Origin.Trace.Steps[0].Outcome != "failed" {
		t.Fatalf("origin action did not fail: %+v", n.Experience.Origin.Trace)
	}
	consumer := expNewSession(t, f, "confirm failure warning independently")
	adoption, err := f.engine.AdoptExperience(ctx, consumer, n.ID, n.Version)
	if err != nil {
		t.Fatal(err)
	}
	result, err = f.engine.runAgentWith(ctx, consumer, completionApplyProvider(t, n, adoption.ID, []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": command}}}), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("consumer %+v %v", result, err)
	}
	got, err := f.engine.ReadExperience(ctx, consumer, n.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	fb := got.Experience.Uses[0].Feedback
	if fb == nil || !fb.Used || fb.Outcome != "success" || fb.Observation == nil || fb.Observation.Evidence.Verification != "failed" {
		t.Fatalf("confirmed failure warning was not learned: %+v", fb)
	}
}
