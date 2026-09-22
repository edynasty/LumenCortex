package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

type checkpointProvider struct {
	mu            sync.Mutex
	calls         int
	secondStarted chan struct{}
}

func (p *checkpointProvider) Model() string { return "checkpoint-model" }

func (p *checkpointProvider) Complete(ctx context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()

	if call == 1 {
		args, _ := json.Marshal(map[string]any{"path": "README.md"})
		return protocol.ProviderResponse{
			Message: protocol.Message{
				Role: "assistant",
				ToolCalls: []protocol.ToolCall{{
					ID:        "read-1",
					Name:      "read_file",
					Arguments: args,
				}},
			},
			FinishReason: "tool_calls",
		}, nil
	}
	if call == 2 {
		close(p.secondStarted)
		<-ctx.Done()
		return protocol.ProviderResponse{}, ctx.Err()
	}
	return protocol.ProviderResponse{
		Message: protocol.Message{Role: "assistant", Content: "unexpected"},
		FinishReason: "stop",
	}, nil
}

type resumeProvider struct{}

func (p *resumeProvider) Model() string { return "resume-model" }

func (p *resumeProvider) Complete(_ context.Context, _ protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	return protocol.ProviderResponse{
		Message: protocol.Message{Role: "assistant", Content: "resumed safely"},
		FinishReason: "stop",
	}, nil
}

func TestInterruptedRunResumesFromPersistedSafeBoundaryWithoutToolReplay(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()

	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "inspect README"})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	provider := &checkpointProvider{secondStarted: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, runErr := engine.RunAgent(ctx, handle.ID, provider, AgentOptions{
			Policy:         "read-only",
			MaxSteps:       4,
			RecentMessages: 16,
		})
		done <- runErr
	}()

	select {
	case <-provider.secondStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("second provider request did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("first run err=%v", err)
	}

	checkpoints, err := engine.SessionCheckpoints(context.Background(), handle.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	seenToolBoundary := false
	seenStepBoundary := false
	for _, cp := range checkpoints {
		if cp.Reason != "agent.safe" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(cp.JSON, &payload) != nil {
			continue
		}
		switch payload["phase"] {
		case "tool_result":
			seenToolBoundary = true
		case "step_complete":
			seenStepBoundary = true
		}
	}
	if !seenToolBoundary || !seenStepBoundary {
		t.Fatalf("safe checkpoints=%#v", checkpoints)
	}

	if _, err := engine.RunAgent(context.Background(), handle.ID, &resumeProvider{}, AgentOptions{
		Policy:         "read-only",
		MaxSteps:       2,
		RecentMessages: 16,
	}); err != nil {
		t.Fatal(err)
	}

	messages, err := engine.RecentMessages(context.Background(), handle.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	toolResults := 0
	for _, message := range messages {
		if message.Role == "tool" {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Fatalf("tool results=%d want=1", toolResults)
	}

	checkpoints, err = engine.SessionCheckpoints(context.Background(), handle.ID, 50)
	if err != nil {
		t.Fatal(err)
	}
	seenResume := false
	for _, cp := range checkpoints {
		if cp.Reason == "agent.resume" {
			seenResume = true
			break
		}
	}
	if !seenResume {
		t.Fatalf("resume checkpoint missing: %#v", checkpoints)
	}
}
