//go:build cgo

package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/protocol"
)

// Write transactions of a whole real-Engine run (T1-S7 item 7, T1-S2 report §2.1). Runs on fresh
// sessions of one Engine differ only in their last tool call; each run's committed write transactions
// on the state database are counted by commitcount_test.go. A step whose call is a side-effect tool
// (write_file, shell) costs exactly one write transaction more than a step whose call is read_file:
// the execution admission. Read-only calls take none, and the side-effect call's result, Task State
// and post record commit in the one transaction a read_file result takes too.
func TestRealEngineSideEffectStepTakesOneMoreWriteTransaction(t *testing.T) {
	installCommitCounter(t)
	workspace := t.TempDir()
	gitInit(t, workspace)
	if err := os.WriteFile(filepath.Join(workspace, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	engine := openEngineAt(t, workspace)
	ctx := context.Background()

	read := protocol.ToolCall{Name: "read_file", Arguments: json.RawMessage(`{"path":"a.txt"}`)}
	run := func(last protocol.ToolCall) int64 {
		t.Helper()
		calls := []protocol.ToolCall{read, read, last}
		handle, err := engine.NewSession(ctx, SessionOptions{Goal: "count " + last.Name})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		p := &exitProvider{fn: func(context.Context, int, protocol.ProviderRequest) (protocol.ProviderResponse, error) {
			n++
			if n > len(calls) {
				return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}, nil
			}
			c := calls[n-1]
			c.ID = fmt.Sprintf("c%d", n)
			return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{c}}, FinishReason: "tool_calls"}, nil
		}}
		before := commitsOn(engine.store.Path())
		result, err := engine.runAgentWith(ctx, handle.ID, p, AgentOptions{Policy: "full", MaxSteps: 6})
		if err != nil || result.Status != "completed" {
			t.Fatalf("%s run = %+v err=%v", last.Name, result, err)
		}
		return commitsOn(engine.store.Path()) - before
	}

	run(read) // warm-up: first statements and first git run outside the counted runs
	readOnly := run(read)
	if again := run(read); again != readOnly {
		t.Fatalf("the same read-only run took %d then %d write transactions", readOnly, again)
	}
	for _, last := range []protocol.ToolCall{
		{Name: "write_file", Arguments: json.RawMessage(`{"path":"b.txt","content":"beta\n"}`)},
		// No output, so no tool.output event adds a journal row.
		{Name: "shell", Arguments: json.RawMessage(`{"command":"true"}`)},
	} {
		if got := run(last); got != readOnly+1 {
			t.Errorf("a run ending in %s took %d write transactions, the same run ending in read_file %d: want exactly one more (the admission)",
				last.Name, got, readOnly)
		}
	}
}
