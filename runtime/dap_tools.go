package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/edynasty/LumenCortex/internal/debugger"
	"github.com/edynasty/LumenCortex/internal/toolset"
)

// StopInfo is the bounded stop report the debug tools return.
type StopInfo = debugger.StopInfo

type runDebugger struct {
	engine           *Engine
	life             context.Context
	sessionID, runID string
}

func (r runDebugger) control(c DebugControl) (DebugControl, error) {
	c.SessionID = r.sessionID
	c.Actor = "agent"
	snapshot, err := r.engine.dapManager.Snapshot(r.sessionID, c.DebugID)
	if err != nil {
		return c, err
	}
	if snapshot.RunID != r.runID {
		return c, errors.New("debug session belongs to another run")
	}
	return c, nil
}
func (r runDebugger) Start(ctx context.Context, adapter, program string, args []string, prepare bool) (DebugSnapshot, error) {
	return r.engine.startProgramDebug(ctx, r.life, DebugStartOptions{SessionID: r.sessionID, RunID: r.runID, AdapterID: adapter, Program: program, Args: args, PrepareForAttach: prepare}, "agent")
}
func (r runDebugger) Attach(ctx context.Context, c DebugControl) (DebugSnapshot, error) {
	c, err := r.control(c)
	if err != nil {
		return DebugSnapshot{}, err
	}
	return r.engine.programAttach(ctx, c)
}
func (r runDebugger) Control(ctx context.Context, c DebugControl) (DebugSnapshot, error) {
	c, err := r.control(c)
	if err != nil {
		return DebugSnapshot{}, err
	}
	return r.engine.programControl(ctx, c)
}

// ControlAndWait clamps the wait to the tool deadline as well as MaxWait.
func (r runDebugger) ControlAndWait(ctx context.Context, c DebugControl, w DebugWaitOptions) (DebugSnapshot, StopInfo, error) {
	c, err := r.control(c)
	if err != nil {
		return DebugSnapshot{}, StopInfo{}, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return DebugSnapshot{}, StopInfo{}, context.DeadlineExceeded
		}
		if w.Timeout <= 0 || remaining < w.Timeout {
			w.Timeout = remaining
		}
	}
	return r.engine.programControlAndWait(ctx, c, w)
}

func (r runDebugger) Disconnect(ctx context.Context, c DebugControl, terminateDebuggee bool) (DebugSnapshot, string, error) {
	c, err := r.control(c)
	if err != nil {
		return DebugSnapshot{}, "", err
	}
	return r.engine.programDisconnect(ctx, c, terminateDebuggee)
}

func (r runDebugger) SetExceptionBreakpoints(ctx context.Context, c DebugControl, filters []string) (json.RawMessage, error) {
	c, err := r.control(c)
	if err != nil {
		return nil, err
	}
	return r.engine.programSetExceptionBreakpoints(ctx, c, filters)
}
func (r runDebugger) Breakpoints(ctx context.Context, c DebugControl, path string, points []DebugBreakpoint) (json.RawMessage, error) {
	c, err := r.control(c)
	if err != nil {
		return nil, err
	}
	return r.engine.programBreakpoints(ctx, c, path, points)
}
func (r runDebugger) Inspect(ctx context.Context, c DebugControl, kind string, ref, start, count int) (json.RawMessage, error) {
	c, err := r.control(c)
	if err != nil {
		return nil, err
	}
	return r.engine.programInspect(ctx, c, kind, ref, start, count)
}
func (r runDebugger) Evaluate(ctx context.Context, c DebugControl, frame int, expression string, sideEffects bool) (json.RawMessage, error) {
	c, err := r.control(c)
	if err != nil {
		return nil, err
	}
	return r.engine.programEvaluate(ctx, c, frame, expression, sideEffects)
}
func (e *Engine) debugTools(ctx context.Context, sessionID, runID, policy string, trusted bool, set *toolset.Set) error {
	if e.dapManager == nil || policy != toolset.PolicyFull || !trusted {
		return nil
	}
	if _, err := e.debugAccess(ctx, sessionID); err != nil {
		if errors.Is(err, ErrDebugDisabled) {
			return nil
		}
		return err
	}
	return set.BindDebugger(runDebugger{engine: e, life: ctx, sessionID: sessionID, runID: runID})
}
