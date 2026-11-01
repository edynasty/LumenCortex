package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// TestContextResetDropsEarlierRowsFromTheNextRequest: after ResetContext the next request starts at the
// reset - no message written before it is sent again - while a run without a reset still carries the
// earlier turns.
func TestContextResetDropsEarlierRowsFromTheNextRequest(t *testing.T) {
	engine, sessionID := notebookEngine(t)
	ctx := context.Background()
	run := func(input string) *notebookProvider {
		t.Helper()
		provider := &notebookProvider{responses: []protocol.ProviderResponse{finalResponse("answered " + input)}}
		result, err := engine.runAgentWith(ctx, sessionID, provider, AgentOptions{Policy: "read-only", MaxSteps: 2, Input: input})
		if err != nil {
			t.Fatal(err)
		}
		if result.Status != "completed" {
			t.Fatalf("run %q = %#v", input, result)
		}
		return provider
	}
	carries := func(provider *notebookProvider, text string) bool {
		for _, message := range provider.messages() {
			if strings.Contains(message.Content, text) {
				return true
			}
		}
		return false
	}

	const first, second, third = "SENTINEL-FIRST-7c1d", "SENTINEL-SECOND-7c1d", "SENTINEL-THIRD-7c1d"
	run(first)
	if control := run(second); !carries(control, first) {
		t.Fatal("without a reset the next request should still carry the earlier turn")
	}
	if _, err := engine.ResetContext(ctx, sessionID); err != nil {
		t.Fatal(err)
	}
	after := run(third)
	if carries(after, first) || carries(after, second) {
		t.Fatalf("the request after a reset carries pre-reset rows: %#v", after.messages())
	}
	if !carries(after, third) {
		t.Fatalf("the request after a reset lost its own input: %#v", after.messages())
	}
}
