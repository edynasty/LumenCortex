package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/protocol"
)

// repairScript writes a wrong answer, waits for the runtime's own correction (which must name the
// failed host check), then writes the right one. It never advances on a step counter alone.
type repairScript struct {
	wroteWrong bool
	sawFailure bool
	wroteRight bool
}

func (p *repairScript) Model() string { return "repair-fixture" }

func (p *repairScript) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	call := func(id, name string, args any) (protocol.ProviderResponse, error) {
		raw, err := json.Marshal(args)
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant",
			ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: raw}}}, FinishReason: "tool_calls",
			Usage: protocol.Usage{Requests: 1}}, err
	}
	final := func(text string) (protocol.ProviderResponse, error) {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: text}, FinishReason: "stop"}, nil
	}
	last := req.Messages[len(req.Messages)-1]
	switch {
	case last.Origin == protocol.OriginCompletionBlocked:
		// The correction comes from the runtime's verification gate, not from the script: it must
		// name the host check that failed before the provider repairs anything.
		if !strings.Contains(last.Content, "answer") {
			return protocol.ProviderResponse{}, fmt.Errorf("correction did not name the failed host check: %q", last.Content)
		}
		p.sawFailure = true
		p.wroteRight = true
		return call("write-right", "write_file", map[string]any{"path": "answer.txt", "content": "42"})
	case last.Role == "tool":
		if p.wroteRight {
			return final("done, corrected")
		}
		if !p.wroteWrong {
			return protocol.ProviderResponse{}, fmt.Errorf("tool result before any write")
		}
		return final("done")
	default:
		if p.wroteWrong || p.sawFailure {
			return protocol.ProviderResponse{}, fmt.Errorf("unexpected request before the host failure")
		}
		p.wroteWrong = true
		return call("write-wrong", "write_file", map[string]any{"path": "answer.txt", "content": "41"})
	}
}

// Scenario D: the workspace really holds the wrong content, the semantic verifier really fails, the run
// gets bounded feedback naming that check, repairs the file, and the same check passes. The failed and
// passed attempts are durable in Task State.
func TestOwner44ScenarioDWrongFileRepairsToPass(t *testing.T) {
	engine, owner := executionTestEngine(t)
	initGitRepo(t, engine.workspace)
	checks, _ := json.Marshal(map[string]any{"answer": map[string]any{
		"kind": "semantic", "command": `test "$(cat answer.txt)" = 42`, "required": false,
	}})
	useChecks(t, string(checks))
	approveChecks(t, engine)
	provider := &repairScript{}
	result, err := engine.runAgentWith(context.Background(), owner, provider, AgentOptions{
		Policy: "full", MaxSteps: 8,
		Retry:        &config.Retry{Disabled: true},
		Verification: &VerificationOptions{RequireSemantic: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" {
		t.Fatalf("repair did not complete: %#v", result)
	}
	if !provider.sawFailure {
		t.Fatal("the provider never saw the runtime's host-failure correction")
	}
	records, err := engine.CheckResults(context.Background(), owner, -1, 0)
	if err != nil {
		t.Fatal(err)
	}
	var failed, passed bool
	for _, record := range records {
		if record.Check == "answer" && record.Status == evaluator.StatusFailed {
			failed = true
		}
		if record.Check == "answer" && record.Status == evaluator.StatusPassed {
			passed = true
		}
	}
	if !failed || !passed {
		t.Fatalf("expected a real semantic failure then a real pass: %#v", records)
	}
	state, err := engine.loadTaskState(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if state.Verification == nil || state.Verification.Status != "passed" {
		t.Fatalf("verification record=%+v", state.Verification)
	}
	if state.Verification.Levels["semantic"] != "passed" || state.Verification.Levels["final"] != "passed" {
		t.Fatalf("verification levels=%+v", state.Verification.Levels)
	}
	attemptFailed := false
	for _, attempt := range state.Verification.Attempts {
		if attempt.Status == "failed" {
			attemptFailed = true
		}
	}
	if !attemptFailed {
		t.Fatalf("the failed semantic attempt was not preserved: %+v", state.Verification.Attempts)
	}
	rows, err := engine.store.RecentMessages(context.Background(), owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	blocked := false
	for _, row := range rows {
		var message protocol.Message
		if json.Unmarshal(row.JSON, &message) == nil && message.Origin == protocol.OriginCompletionBlocked &&
			strings.Contains(message.Content, "answer") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("the run never received the host check's bounded feedback")
	}
	data, err := os.ReadFile(filepath.Join(engine.workspace, "answer.txt"))
	if err != nil || string(data) != "42" {
		t.Fatalf("the repaired file is %q %v", data, err)
	}
}
