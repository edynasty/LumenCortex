package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

// sameKernelScript drives the real agent loop (scripted provider, no model
// request) through eval_create and two eval cells of one kernel, then checks that
// the two results name the same kernel and generation and that the second cell
// saw the first cell's variables.
type sameKernelScript struct {
	runtimeID   string
	setupCode   string
	computeCode string
	wantOutput  string
	kernel      string
	step        int
	first       evalapi.Result
	second      evalapi.Result
	observed    bool
	toolsSeen   map[string]bool
}

func (p *sameKernelScript) Model() string { return "local-eval-persistence-fixture" }

func (p *sameKernelScript) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.step++
	tool := func(id, name string, args any) (protocol.ProviderResponse, error) {
		raw, err := json.Marshal(args)
		if err != nil {
			return protocol.ProviderResponse{}, err
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant",
			ToolCalls: []protocol.ToolCall{{ID: id, Name: name, Arguments: raw}}}, FinishReason: "tool_calls",
			Usage: protocol.Usage{Requests: 1}}, nil
	}
	switch p.step {
	case 1:
		p.toolsSeen = map[string]bool{}
		for _, spec := range req.Tools {
			p.toolsSeen[spec.Name] = true
		}
		for _, name := range []string{"eval_create", "eval", "eval_sessions", "eval_inspect"} {
			if !p.toolsSeen[name] {
				return protocol.ProviderResponse{}, fmt.Errorf("eval tool %q is not offered to the agent", name)
			}
		}
		return tool("create", "eval_create", map[string]any{"runtime_id": p.runtimeID})
	case 2:
		for _, msg := range req.Messages {
			if msg.Role == "tool" && msg.ToolCallID == "create" {
				var session evalapi.Session
				if err := json.Unmarshal([]byte(msg.Content), &session); err != nil {
					return protocol.ProviderResponse{}, err
				}
				p.kernel = session.ID
			}
		}
		if p.kernel == "" {
			return protocol.ProviderResponse{}, fmt.Errorf("eval_create returned no kernel")
		}
		return tool("setup", "eval", map[string]any{"session_id": p.kernel, "cell_id": "setup", "code": p.setupCode})
	case 3:
		p.first = toolResult(req, "setup")
		return tool("compute", "eval", map[string]any{"session_id": p.kernel, "cell_id": "compute", "code": p.computeCode})
	default:
		p.second = toolResult(req, "compute")
		p.observed = true
		// The same kernel answered both cells, in one generation, and the second
		// cell computed from the first cell's variables.
		if !p.first.Succeeded() || !p.second.Succeeded() {
			return protocol.ProviderResponse{}, fmt.Errorf("a cell failed: first=%+v second=%+v", p.first, p.second)
		}
		if p.first.SessionID != p.kernel || p.second.SessionID != p.kernel {
			return protocol.ProviderResponse{}, fmt.Errorf("kernel identity changed: %q -> %q (created %q)", p.first.SessionID, p.second.SessionID, p.kernel)
		}
		if p.first.Generation != p.second.Generation {
			return protocol.ProviderResponse{}, fmt.Errorf("generation changed between cells: %d -> %d", p.first.Generation, p.second.Generation)
		}
		if strings.TrimSpace(p.second.Stdout) != p.wantOutput {
			return protocol.ProviderResponse{}, fmt.Errorf("stdout = %q, want %q", p.second.Stdout, p.wantOutput)
		}
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant",
			Content: "300 observed from the same kernel and generation."}, FinishReason: "stop", Usage: protocol.Usage{Requests: 1}}, nil
	}
}

// toolResult decodes the evalapi.Result the loop handed back for one tool call.
func toolResult(req protocol.ProviderRequest, callID string) evalapi.Result {
	for _, msg := range req.Messages {
		if msg.Role == "tool" && msg.ToolCallID == callID {
			var result evalapi.Result
			if json.Unmarshal([]byte(msg.Content), &result) == nil {
				return result
			}
		}
	}
	return evalapi.Result{}
}

func runSameKernelScenario(t *testing.T, runtimeID, setup, compute, want string) (*sameKernelScript, *Engine, string) {
	t.Helper()
	engine, owner := executionTestEngine(t)
	provider := &sameKernelScript{runtimeID: runtimeID, setupCode: setup, computeCode: compute, wantOutput: want}
	result, err := engine.runAgentWith(context.Background(), owner, provider,
		AgentOptions{Policy: "workspace", EvalEnabled: true, MaxSteps: 8})
	if err != nil {
		t.Fatalf("agent loop: %v", err)
	}
	if result.Status != "completed" || !provider.observed {
		t.Fatalf("workflow did not complete: %#v", result)
	}
	// The scripted provider made one request per turn; no model was contacted.
	if result.Usage.Requests != 4 {
		t.Fatalf("provider accounting = %#v", result.Usage)
	}
	return provider, engine, owner
}

// TestAgentKeepsPythonKernelAcrossCells proves cross-call state for the Python
// kernel through the real Agent loop: x=100;y=200 in one cell, z=x+y in the next,
// the same kernel id and generation, print(z) == 300.
func TestAgentKeepsPythonKernelAcrossCells(t *testing.T) {
	engine, _ := executionTestEngine(t)
	runtimeID := executionRuntimeID(t, engine, "python")
	provider, engine, owner := runSameKernelScenario(t, runtimeID,
		"x = 100\ny = 200",
		"z = x + y\nprint(z)",
		"300")
	// The kernel the agent used is the one the host lists, with the same
	// generation, so the identity the agent saw is not a per-call artifact.
	sessions, err := engine.EvalSessions(context.Background(), owner)
	if err != nil {
		t.Fatalf("EvalSessions: %v", err)
	}
	if len(sessions) != 1 || sessions[0].ID != provider.kernel || sessions[0].Generation != provider.second.Generation {
		t.Fatalf("kernel listing = %+v", sessions)
	}
}

// TestAgentKeepsJavaScriptKernelAcrossCells is the same proof for the JS kernel.
func TestAgentKeepsJavaScriptKernelAcrossCells(t *testing.T) {
	engine, _ := executionTestEngine(t)
	runtimeID := executionRuntimeID(t, engine, "javascript")
	_, _, _ = runSameKernelScenario(t, runtimeID,
		"const x = 100\nconst y = 200",
		"const z = x + y\nconsole.log(z)",
		"300")
}
