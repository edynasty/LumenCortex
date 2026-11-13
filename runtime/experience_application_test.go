package runtime

import (
	"context"
	"testing"
)

func expApply(t *testing.T, f *cognitionFixture, consumer string, n CognitiveNode, adoption string) {
	t.Helper()
	if err := f.engine.EnableCognition(context.Background()); err != nil {
		t.Fatal(err)
	}
	actions := []experienceAction{{Tool: "write_file", Arguments: map[string]any{"path": "build/reused", "content": "ready"}}}
	expMustRunSession(t, f, consumer, completionApplyProvider(t, n, adoption, actions))
}

// Counterexamples execute the very same conditional command in both runs. The
// consumer's environment changes an uncited precondition the proposal omitted;
// the command really runs and its independently checked result contradicts it.
func expConditionalValidated(t *testing.T, f *cognitionFixture, pattern string) (CognitiveNode, string) {
	t.Helper()
	expInstallChecks(t, f)
	writeFile(t, f.root+"/build/fixture", "conditional procedure workspace")
	origin := expNewSession(t, f, pattern)
	result, err := f.engine.runAgentWith(context.Background(), origin, expCommandThenFinal("if test -f build/block; then rm -f build/reused; else printf ready > build/reused; fi", "conditional origin"), AgentOptions{MaxSteps: 6, Policy: "full"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("conditional origin %+v %v", result, err)
	}
	return expValidate(t, f, origin, expExtract(t, f, origin, expProposal(pattern))), origin
}
func expApplyCounterexample(t *testing.T, f *cognitionFixture, consumer string, n CognitiveNode, adoption string) (AgentResult, error) {
	t.Helper()
	if err := f.engine.EnableCognition(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.root+"/build/block", "blocked")
	actions := []experienceAction{{Tool: "bash", Arguments: map[string]any{"command": "if test -f build/block; then rm -f build/reused; else printf ready > build/reused; fi"}}}
	return f.engine.runAgentWith(context.Background(), consumer, completionApplyProvider(t, n, adoption, actions), AgentOptions{MaxSteps: 6, Policy: "full"})
}
