package protocol

import "context"

// ToolCaller is the host-authorized tool surface for exactly one execution (the eval bridge). It is
// not a second tool set: the loop binds it to the context of one admitted tool call, and every
// nested call is checked again against that call's effective tool set, policy, budget and lifecycle
// hooks.
//
// Tools returns the specs a nested call may use. Call runs one nested call and returns the same
// result shape a top-level call has. A caller that is not bound (a host call outside the loop) is
// absent from the context; ToolCallbackFromContext reports that, and an execution runtime that
// receives no caller refuses callbacks rather than inventing authority.
type ToolCaller interface {
	Tools(ctx context.Context) ([]ToolSpec, error)
	Call(ctx context.Context, name string, arguments map[string]any) (ToolResult, error)
}

// toolCallbackSlot is the context value. It is a struct, not the interface, so a nil caller can be
// stored explicitly: binding nil clears a caller inherited from an outer context instead of leaving
// it reachable.
type toolCallbackSlot struct {
	caller ToolCaller
}

type toolCallbackKey struct{}

// WithToolCallback returns ctx carrying caller. A nil caller stores an empty slot, which clears any
// caller inherited from an outer context: a nested execution can never keep the previous caller.
func WithToolCallback(ctx context.Context, caller ToolCaller) context.Context {
	return context.WithValue(ctx, toolCallbackKey{}, toolCallbackSlot{caller: caller})
}

// ToolCallbackFromContext returns the caller bound to ctx, false when ctx carries none or an empty
// slot.
func ToolCallbackFromContext(ctx context.Context) (ToolCaller, bool) {
	slot, ok := ctx.Value(toolCallbackKey{}).(toolCallbackSlot)
	return slot.caller, ok && slot.caller != nil
}
