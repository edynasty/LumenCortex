package runtime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

type scriptedProvider struct {
	responses []protocol.ProviderResponse
}

func (p *scriptedProvider) Model() string { return "scripted" }

func (p *scriptedProvider) Complete(context.Context, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

func TestRunAgentUsesEmbeddedGoHarness(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "inspect the workspace"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []protocol.ProviderResponse{
		{Message: protocol.Message{ToolCalls: []protocol.ToolCall{{ID: "1", Name: "list_dir", Arguments: json.RawMessage(`{"path":"."}`)}}},
		{Message: protocol.Message{Content: "inspection complete"}},
	}}
	result, err := engine.RunAgent(ctx, handle.ID, provider, AgentOptions{Policy: "read-only", MaxSteps: 4})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "completed" || result.Final != "inspection complete" {
		t.Fatalf("result=%#v", result)
	}
	messages, err := handle.RecentMessages(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) < 4 {
		t.Fatalf("expected persisted user/assistant/tool/final messages, got %d", len(messages))
	}
}
