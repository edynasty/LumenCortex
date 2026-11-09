package runtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

type heldReadCallback struct {
	entered, finished chan struct{}
	once              sync.Once
}

func (*heldReadCallback) ID() string                       { return "hold-detached-read" }
func (*heldReadCallback) Version() int                     { return 1 }
func (*heldReadCallback) Events() []LifecycleEvent         { return []LifecycleEvent{LifecycleBeforeToolCall} }
func (*heldReadCallback) Priority() int                    { return 0 }
func (*heldReadCallback) Timeout() time.Duration           { return time.Second * 5 }
func (*heldReadCallback) FailurePolicy() HookFailurePolicy { return HookFailClosed }
func (*heldReadCallback) MutableArgs() bool                { return false }
func (*heldReadCallback) Safety() bool                     { return true }
func (h *heldReadCallback) Handle(ctx context.Context, r LifecycleRequest) (LifecycleResult, error) {
	if r.ToolName != "read_file" {
		return LifecycleResult{Decision: HookContinue}, nil
	}
	h.once.Do(func() { close(h.entered) })
	<-ctx.Done()
	close(h.finished)
	return LifecycleResult{Decision: HookDeny, Reason: "cancelled"}, ctx.Err()
}

func TestAgentJavaScriptDetachedCallbackEndsBeforeNextStep(t *testing.T) {
	engine, owner := executionTestEngine(t)
	hook := &heldReadCallback{entered: make(chan struct{}), finished: make(chan struct{})}
	if err := engine.RegisterLifecycleHook(hook); err != nil {
		t.Fatal(err)
	}
	script := &executionScript{runtimeID: executionRuntimeID(t, engine, "javascript"),
		setupCode: `var values = [3,7,11]; var detached = lc_tool('read_file', {path:'missing.txt'}).catch(()=>null); await new Promise(resolve=>setTimeout(resolve,200)); console.log('cell complete');`,
		reuseCode: `console.log((values.reduce((a,b)=>a+b,0)/values.length).toFixed(1));`}
	provider := &exitProvider{fn: func(ctx context.Context, _ int, req protocol.ProviderRequest) (protocol.ProviderResponse, error) {
		if script.step == 2 {
			select {
			case <-hook.entered:
			default:
				t.Error("test did not enter actual JS callback")
			}
			select {
			case <-hook.finished:
			default:
				t.Error("next step reached before detached callback finished")
			}
		}
		return script.Complete(ctx, req)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*15)
	defer cancel()
	result, err := engine.runAgentWith(ctx, owner, provider, AgentOptions{Policy: "full", EvalEnabled: true, MaxSteps: 6})
	if err != nil || result.Status != "completed" || !script.observed {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
