package protocol

import (
	"context"
	"time"
)

type toolCallKey struct{}
type toolDeadlineKey struct{}

// WithToolCallID returns ctx carrying the id of the tool call being executed, so a tool that records
// what it started (a background job) can name the call that started it.
func WithToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallKey{}, id)
}

// ToolCallID is the id WithToolCallID stored, "" when ctx carries none.
func ToolCallID(ctx context.Context) string {
	id, _ := ctx.Value(toolCallKey{}).(string)
	return id
}

// WithToolDeadline returns ctx carrying the time by which a tool call must have returned: the run's
// deadline less the margin it keeps for the reply after the last tool result (ADR-0019 §2). It is not a
// context deadline: a tool that reaches it stops its own work and reports a timeout as its result, while
// cancelling ctx ends the run. A zero t stores nothing.
func WithToolDeadline(ctx context.Context, t time.Time) context.Context {
	if t.IsZero() {
		return ctx
	}
	return context.WithValue(ctx, toolDeadlineKey{}, t)
}

// ToolDeadline is the time WithToolDeadline stored; ok is false when ctx carries none.
func ToolDeadline(ctx context.Context) (t time.Time, ok bool) {
	t, ok = ctx.Value(toolDeadlineKey{}).(time.Time)
	return t, ok
}
