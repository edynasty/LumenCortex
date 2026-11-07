package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

// executionScript uses only visible tool results to decide its next request; it
// exercises the actual loop/tool/worker route, without a network model provider.
type executionScript struct {
	step                 int
	kernel               string
	runtimeID            string
	observed             bool
	setupCode, reuseCode string
}

func (p *executionScript) Model() string { return "local-execution-fixture" }
func (p *executionScript) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.step++
	tool := func(id, name string, args any) (protocol.ProviderResponse, error) {
		raw, err := json.Marshal(args)
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: raw}}}, FinishReason: "tool_calls", Usage: protocol.Usage{Requests: 1}}, err
	}
	switch p.step {
	case 1:
		found := false
		for _, spec := range req.Tools {
			if spec.Name == "eval_create" {
				found = true
			}
		}
		if !found {
			return protocol.ProviderResponse{}, fmt.Errorf("eval not offered")
		}
		return tool("create", "eval_create", map[string]any{"runtime_id": p.runtimeID})
	case 2:
		for _, msg := range req.Messages {
			if msg.Role == "tool" && msg.ToolCallID == "create" {
				var s evalapi.Session
				if err := json.Unmarshal([]byte(msg.Content), &s); err != nil {
					return protocol.ProviderResponse{}, err
				}
				p.kernel = s.ID
			}
		}
		if p.kernel == "" {
			return protocol.ProviderResponse{}, fmt.Errorf("no kernel identity returned")
		}
		code := p.setupCode
		if code == "" {
			code = "values = [3, 7, 11]\nprint(sum(values))"
		}
		return tool("calculate", "eval", map[string]any{"session_id": p.kernel, "cell_id": "calculate", "code": code})
	case 3:
		code := p.reuseCode
		if code == "" {
			code = "print(sum(values) / len(values))"
		}
		return tool("reuse", "eval", map[string]any{"session_id": p.kernel, "cell_id": "reuse", "code": code})
	default:
		for _, msg := range req.Messages {
			if msg.Role == "tool" && msg.ToolCallID == "reuse" {
				var result evalapi.Result
				if err := json.Unmarshal([]byte(msg.Content), &result); err != nil {
					return protocol.ProviderResponse{}, err
				}
				p.observed = result.Succeeded() && strings.TrimSpace(result.Stdout) == "7.0"
			}
		}
		if !p.observed {
			return protocol.ProviderResponse{}, fmt.Errorf("persistent computation was not returned to model")
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "Mean 7.0 observed from three original values; no independent semantic claim."}, FinishReason: "stop", Usage: protocol.Usage{Requests: 1}}, nil
	}
}

func TestAgentUsesPersistentRuntimeAndReceivesActualComputation(t *testing.T) {
	engine, owner := executionTestEngine(t)
	provider := &executionScript{runtimeID: executionRuntimeID(t, engine, "python")}
	result, err := engine.runAgentWith(context.Background(), owner, provider, AgentOptions{Policy: "workspace", EvalEnabled: true, MaxSteps: 6})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || !provider.observed {
		t.Fatalf("workflow did not complete from visible result: %#v", result)
	}
	if result.Usage.Requests != 4 {
		t.Fatalf("fake-provider accounting missing: %#v", result.Usage)
	}
}

func TestAgentEvalCallbacksKeepWorkspaceAuthorityAndToolPairing(t *testing.T) {
	engine, owner := executionTestEngine(t)
	provider := &executionScript{runtimeID: executionRuntimeID(t, engine, "python"), setupCode: `import json
lc_tool('write_file', path='callback-data.json', content='[3,7,11]')
values = json.loads(json.loads(lc_tool('read_file', path='callback-data.json'))['content'])
print(sum(values))`, reuseCode: `names = [tool['name'] for tool in lc_tools()]
assert 'bash' not in names and 'eval' not in names and 'eval_create' not in names
print(sum(values) / len(values))`}
	result, err := engine.runAgentWith(context.Background(), owner, provider, AgentOptions{Policy: "full", EvalEnabled: true, MaxSteps: 6})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || !provider.observed {
		t.Fatalf("callback workflow failed: %#v", result)
	}
	data, err := os.ReadFile(filepath.Join(engine.workspace, "callback-data.json"))
	if err != nil || string(data) != "[3,7,11]" {
		t.Fatalf("authorized callback did not write actual workspace data: %s %v", data, err)
	}
	messages, err := engine.store.RecentMessages(context.Background(), owner, 100)
	if err != nil {
		t.Fatal(err)
	}
	issued := map[string]bool{}
	for _, stored := range messages {
		var message protocol.Message
		if err := json.Unmarshal(stored.JSON, &message); err != nil {
			t.Fatal(err)
		}
		for _, call := range message.ToolCalls {
			issued[call.ID] = true
		}
		if message.Role == "tool" && !issued[message.ToolCallID] {
			t.Fatalf("unmatched nested tool result persisted: %s", message.ToolCallID)
		}
	}
}
