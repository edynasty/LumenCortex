package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// twoReadsProvider answers steps-1 steps with two read_file calls each, then "done". It records the
// transcript part of every request (after System and the Task State block).
type twoReadsProvider struct {
	mu    sync.Mutex
	steps int
	seen  [][]protocol.Message
}

func (p *twoReadsProvider) Model() string { return "two-reads" }

func (p *twoReadsProvider) Complete(_ context.Context, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, append([]protocol.Message(nil), req.Messages[2:]...))
	n := len(p.seen)
	if n >= p.steps {
		return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", Content: "done"}, FinishReason: "stop"}, nil
	}
	args, _ := json.Marshal(map[string]any{"path": "README.md"})
	return protocol.ProviderResponse{Message: protocol.Message{Role: "assistant", ToolCalls: []protocol.ToolCall{
		{ID: fmt.Sprintf("a%d", n), Name: "read_file", Arguments: args},
		{ID: fmt.Sprintf("b%d", n), Name: "read_file", Arguments: args},
	}}, FinishReason: "tool_calls"}, nil
}

// A run whose transcript grows past any fixed row count keeps sending whole, current windows through
// the SQLite store: with two tool results per step the transcript passes 50 rows (the store's read
// limit) mid-run, and every request still holds the whole transcript in seq order, ending with the
// newest row. Before the fix, request 17 failed with an unpaired tool result.
func TestLongRunWindowReadsWholeTranscript(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine, err := Open(Options{Workspace: root})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	handle, err := engine.NewSession(context.Background(), SessionOptions{Goal: "read README many times"})
	if err != nil {
		t.Fatal(err)
	}
	provider := &twoReadsProvider{steps: 30}
	result, err := engine.runAgentWith(context.Background(), handle.ID, provider, AgentOptions{Policy: "read-only", MaxSteps: 40})
	if err != nil || result.Status != "completed" {
		t.Fatalf("result %+v err %v", result, err)
	}
	if len(provider.seen) != 30 {
		t.Fatalf("requests=%d", len(provider.seen))
	}
	for i, transcript := range provider.seen {
		// the goal row, then three rows per earlier step: assistant and two tool results
		if want := 1 + 3*i; len(transcript) != want {
			t.Fatalf("request %d holds %d transcript rows, want %d", i, len(transcript), want)
		}
		if i > 0 {
			if last := transcript[len(transcript)-1]; last.Role != "tool" || last.ToolCallID != fmt.Sprintf("b%d", i) {
				t.Fatalf("request %d ends with %+v, want the tool result b%d", i, last, i)
			}
		}
	}
}
