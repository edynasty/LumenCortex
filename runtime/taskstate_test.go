package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
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

// TestContextResetAfterACancelledToolResumes: a run cancelled during a real bash call leaves the call
// without a result. ResetContext repairs that before it moves the window, so the repair row stays
// before the window start with its call, and the next run starts on a paired transcript.
func TestContextResetAfterACancelledToolResumes(t *testing.T) {
	engine := openTestEngine(t)
	handle := newTestSession(t, engine, "reset a cancelled run")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	events, unsubscribe := engine.Events(512)
	defer unsubscribe()
	provider := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "reset-sleep", Name: "bash", Arguments: json.RawMessage(`{"command":"echo reset-ready && exec sleep 30"}`)},
		}}, FinishReason: "tool_calls"}, nil
	}}
	if _, err := supervisor.startWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "full", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitForEventWith(t, events, "tool.output", "reset-ready")
	if !supervisor.Cancel(handle.ID) {
		t.Fatal("run was not active")
	}
	waitUntilIdle(t, supervisor, handle.ID, 15*time.Second)

	info, err := engine.ResetContext(context.Background(), handle.ID)
	if err != nil {
		t.Fatal(err)
	}
	var results []int64
	if err := engine.store.EachMessageFrom(context.Background(), handle.ID, 0, func(row session.Message) error {
		var msg protocol.Message
		if err := json.Unmarshal(row.JSON, &msg); err != nil {
			return err
		}
		if row.Role == "tool" && msg.ToolCallID == "reset-sleep" {
			results = append(results, row.Seq)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0] >= info.State.WindowStartSeq {
		t.Fatalf("result rows of the cancelled call at %v, window start %d: want one, before the window",
			results, info.State.WindowStartSeq)
	}

	result, err := engine.runAgentWith(context.Background(), handle.ID, &finalProvider{},
		AgentOptions{Policy: "read-only", MaxSteps: 2, Input: "continue after reset"})
	if err != nil || result.Status != "completed" {
		t.Fatalf("reset then resume status=%s error=%v; want completed", result.Status, err)
	}
}
