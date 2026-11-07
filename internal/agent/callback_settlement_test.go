package agent

import (
	"context"
	"testing"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

type cancellingWriteTools struct {
	callbackFakeTools
	cancel context.CancelFunc
}

func (w *cancellingWriteTools) Execute(context.Context, string, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	w.cancel()
	return protocol.ToolResult{OK: true, MutatesWorkspace: true, Content: `{"changed":true}`}, nil
}

type contextCheckingSettler struct {
	*memoryStore
	settled bool
}

func (s *contextCheckingSettler) SettleExecution(ctx context.Context, _ string, _ session.ExecutionResult, _ ...session.DerivedEdge) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.settled = true
	return nil
}
func TestCallbackCompletedWriteSettlesAfterEvalCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &contextCheckingSettler{memoryStore: newRunningStore("settle-cancelled")}
	tools := &cancellingWriteTools{cancel: cancel}
	d := &callbackDispatcher{loop: &Loop{Store: store, Tools: tools}, sessionID: "settle-cancelled", specs: callbackSpecs(), spend: &Spend{}, journal: &callbackJournal{}}
	result, err := d.Call(ctx, "write_file", map[string]any{"path": "answer.txt"})
	if err != nil || !result.OK || !store.settled {
		t.Fatalf("completed effect not settled: %+v %v settled=%v", result, err, store.settled)
	}
}
