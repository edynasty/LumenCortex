package agent

import (
	"context"
	"time"

	"github.com/edynasty/LumenCortex/internal/verify"
)

// lifecycleTestHook is a scripted verify.Hook for loop tests.
type lifecycleTestHook struct {
	id       string
	version  int
	events   []verify.Event
	priority int
	timeout  time.Duration
	policy   verify.FailPolicy
	mutable  bool
	safety   bool
	run      func(context.Context, verify.Request) (verify.Result, error)
}

func (h *lifecycleTestHook) ID() string   { return h.id }
func (h *lifecycleTestHook) Version() int { return h.version }
func (h *lifecycleTestHook) Events() []verify.Event {
	return h.events
}
func (h *lifecycleTestHook) Priority() int                 { return h.priority }
func (h *lifecycleTestHook) Timeout() time.Duration        { return h.timeout }
func (h *lifecycleTestHook) FailPolicy() verify.FailPolicy { return h.policy }
func (h *lifecycleTestHook) MutableArgs() bool             { return h.mutable }
func (h *lifecycleTestHook) Safety() bool                  { return h.safety }
func (h *lifecycleTestHook) Run(ctx context.Context, req verify.Request) (verify.Result, error) {
	return h.run(ctx, req)
}
