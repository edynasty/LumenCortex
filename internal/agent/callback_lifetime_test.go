package agent

import (
	"context"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// detachedCallbackTools models a JS cell returning with an unawaited callback.
// The callback stays inside the tool until cancellation; the next provider turn
// must not run while it can still mutate the loop's spend or journal.
type detachedCallbackTools struct {
	entered chan struct{}
	settled chan struct{}
	caller  protocol.ToolCaller
	revoked bool
}

func (f *detachedCallbackTools) Specs([]string) []protocol.ToolSpec {
	return []protocol.ToolSpec{{Name: "eval", Permission: "exec"}, {Name: "read_file", Permission: "read"}}
}
func (f *detachedCallbackTools) Execute(ctx context.Context, name string, _ map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	if name == "read_file" {
		close(f.entered)
		<-ctx.Done()
		close(f.settled)
		return protocol.ToolResult{}, ctx.Err()
	}
	f.caller, _ = protocol.ToolCallbackFromContext(ctx)
	callbackCtx, cancel := context.WithCancel(ctx)
	go func() { f.caller.Call(callbackCtx, "read_file", nil); cancel() }()
	<-f.entered
	if f.revoked {
		cancel()
	}
	return protocol.ToolResult{OK: true, Content: `{"status":"completed"}`}, nil
}
func TestLoopDrainsDetachedCallbackBeforeNextTurn(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unawaited", true: "revoked"}[revoked], func(t *testing.T) {
			tools := &detachedCallbackTools{entered: make(chan struct{}), settled: make(chan struct{}), revoked: revoked}
			store := newRunningStore("callback-lifetime")
			provider := &scriptedProvider{responses: []protocol.ProviderResponse{
				{Message: protocol.Message{ToolCalls: []protocol.ToolCall{call("eval-1", "eval", `{}`)}}},
				{Message: protocol.Message{Content: "done"}},
			}}
			loop := Loop{Models: testModels(provider), Store: store, Tools: tools}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second*5)
			defer cancel()
			result, err := loop.Run(ctx, "callback-lifetime", Options{Session: testRef, MaxSteps: 3})
			if err != nil || result.Status != StatusCompleted {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			select {
			case <-tools.settled:
			default:
				t.Fatal("eval completed while callback still active")
			}
			before := result.Spend.ToolCalls
			late, err := tools.caller.Call(context.Background(), "read_file", nil)
			if err != nil || !late.Denied {
				t.Fatalf("late callback=%+v error=%v", late, err)
			}
			d := tools.caller.(*callbackDispatcher)
			if d.spend.ToolCalls != before {
				t.Fatal("late callback changed settled spend")
			}
			if _, err := tools.caller.Tools(context.Background()); err == nil {
				t.Fatal("closed capability still advertises tools")
			}
		})
	}
}
