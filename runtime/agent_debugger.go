package runtime

import (
	"context"
	"errors"

	"github.com/edynasty/LumenCortex/internal/agent"
)

type AgentDebugOptions = agent.DebugOptions
type AgentDebugBreakpoint = agent.DebugBreakpoint
type AgentDebugSnapshot = agent.DebugSnapshot

// AgentDebugCommand is host-only; it is not registered as an Agent tool.
// RunID + Revision prevent a delayed UI command from controlling a new run or pause.
type AgentDebugCommand struct {
	SessionID   string                 `json:"sessionId"`
	RunID       string                 `json:"runId"`
	Revision    uint64                 `json:"revision"`
	Action      string                 `json:"action"`
	Breakpoints []AgentDebugBreakpoint `json:"breakpoints,omitempty"`
}

func (e *Engine) AgentDebugSnapshot(ctx context.Context, sessionID string) (AgentDebugSnapshot, error) {
	if err := e.runtimeEventAccess(ctx, sessionID); err != nil {
		return AgentDebugSnapshot{}, err
	}
	e.runsMu.Lock()
	run := e.runs[sessionID]
	var control *agent.DebugControl
	if run != nil {
		control = run.debug
	}
	e.runsMu.Unlock()
	if control == nil {
		return AgentDebugSnapshot{State: "inactive"}, nil
	}
	return control.Snapshot(), nil
}
func (e *Engine) ControlAgentDebug(ctx context.Context, command AgentDebugCommand) (AgentDebugSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return AgentDebugSnapshot{}, err
	}
	if err := e.runtimeEventAccess(ctx, command.SessionID); err != nil {
		return AgentDebugSnapshot{}, err
	}
	e.runsMu.Lock()
	run := e.runs[command.SessionID]
	var control *agent.DebugControl
	if run != nil {
		control = run.debug
	}
	e.runsMu.Unlock()
	if control == nil {
		return AgentDebugSnapshot{}, errors.New("agent has no active debug controller")
	}
	snapshot := control.Snapshot()
	if snapshot.RunID != command.RunID || snapshot.Revision != command.Revision {
		return AgentDebugSnapshot{}, agent.ErrDebugStale
	}
	if command.Action == "cancel" || command.Action == "close" {
		run.cancel()
		return control.Snapshot(), nil
	}
	return control.Command(command.Revision, command.Action, command.Breakpoints)
}
