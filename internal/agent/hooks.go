package agent

import (
	"context"
	"time"

	"github.com/edynasty/LumenCortex/internal/verify"
)

// Lifecycle hooks are host-supplied and versioned (spec §20). The loop runs them at the events it
// declares, in the registry's deterministic order, with the registry's timeout and fail policy, and
// records every decision as an event. A hook can allow, deny, continue or carry bounded feedback; it
// never changes the run's policy, and a safety hook is fail-closed.
//
// The loop's own names are stable event strings, so a host's hook sees the same events the runtime
// documents even when the internal event type changes.

type HookEvent = verify.Event

const (
	verifyEventPreToolUse   = verify.EventPreToolUse
	verifyEventPostToolUse  = verify.EventPostToolUse
	verifyEventPreEval      = verify.EventPreEval
	verifyEventPostEval     = verify.EventPostEval
	verifyEventPreSubagent  = verify.EventPreSubagent
	verifyEventPostSubagent = verify.EventPostSubagent
	verifyEventPreVerify    = verify.EventPreVerify
	verifyEventPostVerify   = verify.EventPostVerify
	verifyEventOnFailure    = verify.EventOnFailure
	verifyEventOnCancel     = verify.EventOnCancel
	verifyEventOnTimeout    = verify.EventOnTimeout
	verifyEventOnClose      = verify.EventOnSessionClose
	verifyEventStop         = verify.EventStop
	verifyEventCompletion   = verify.EventCompletion
)

// HookRequest is the observable input of one lifecycle event.
type HookRequest = verify.Request

// HookOutcome is a hook event's merged decision.
type HookOutcome = verify.Outcome

// Hook returns the run's hook registry, nil when the host configured none.
func (l *Loop) hookRegistry() *verify.Registry { return l.Hooks }

// runHook dispatches one lifecycle event and emits each hook's audit entry. A nil registry allows
// every event without work.
func (l *Loop) runHook(ctx context.Context, sessionID, runID string, event verify.Event, req HookRequest) verify.Outcome {
	registry := l.hookRegistry()
	if registry == nil {
		return verify.Outcome{Decision: verify.DecisionAllow}
	}
	req.Version = registry.Version
	req.Event = event
	req.SessionID = sessionID
	req.RunID = runID
	if req.Detail == nil {
		req.Detail = map[string]any{}
	}
	out := registry.Decide(ctx, req)
	for _, entry := range out.Audit {
		data := map[string]any{
			"hook": entry.Hook, "hookVersion": entry.Version, "event": string(entry.Event),
			"decision": string(entry.Decision), "elapsedMs": entry.ElapsedMS,
		}
		if entry.Reason != "" {
			data["reason"] = entry.Reason
		}
		if entry.Err != "" {
			data["error"] = entry.Err
		}
		if entry.FailPolicy != "" {
			data["failPolicy"] = string(entry.FailPolicy)
		}
		if entry.Overridden != "" {
			data["overridden"] = entry.Overridden
		}
		if entry.ArgsIgnored {
			data["argsIgnored"] = true
		}
		l.emit("hook."+string(entry.Event), sessionID, data)
	}
	return out
}

// hookExit is the exit a run takes when an exit-observing hook denies it. Exit hooks are advisory:
// they observe a failure, cancellation or timeout that already happened, so a denial is reported,
// not turned into a second failure.
func (l *Loop) observeExitHook(ctx context.Context, sessionID, runID string, event verify.Event, detail map[string]any) {
	if l.hookRegistry() == nil {
		return
	}
	l.runHook(ctx, sessionID, runID, event, HookRequest{Detail: detail})
}

// isSubagentTool reports the tools that start or observe another agent: their events are
// PreSubagent and PostSubagent in addition to the tool events.
func isSubagentTool(name string) bool {
	return name == toolStartTask || name == toolSpawnSubagent || name == "wait_subagent" || name == "list_subagents"
}

// hookFeedback is the bounded text a blocking hook adds to a completion_blocked row.
func hookFeedback(out verify.Outcome) string {
	text := out.FeedbackText()
	if text == "" {
		return "A lifecycle hook denied the action without feedback."
	}
	return text
}

// timeoutHookDetail is the bounded detail an exit hook sees.
func exitHookDetail(kind, message string, at time.Time) map[string]any {
	return map[string]any{"kind": kind, "message": message, "at": at.UTC().Format(time.RFC3339Nano)}
}
