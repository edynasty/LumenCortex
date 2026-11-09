package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/edynasty/LumenCortex/evalruntime"
	"github.com/edynasty/LumenCortex/internal/debugger"
)

type DebugAdapter = debugger.Adapter
type DebugSnapshot = debugger.Snapshot
type DebugStartOptions = debugger.StartOptions
type DebugControl = debugger.Control
type DebugBreakpoint = debugger.Breakpoint

type DebugOptions struct {
	Adapters        []DebugAdapter             `json:"adapters"`
	Sandbox         evalruntime.SandboxOptions `json:"sandbox"`
	MaxSessions     int                        `json:"maxSessions,omitempty"`
	LifetimeSeconds int                        `json:"lifetimeSeconds,omitempty"`
}

// DebugGrant is host authority for one Session. It is never accepted from tool
// input or persisted in the workspace; revocation closes every owned adapter.
type DebugGrant struct {
	Enabled       bool `json:"enabled"`
	AllowAttach   bool `json:"allowAttach"`
	AllowEvaluate bool `json:"allowEvaluate"`
}

var ErrDebugDisabled = errors.New("program debugging is disabled or not authorized")

func (e *Engine) initDebug(opts *DebugOptions) error {
	if opts == nil {
		return nil
	}
	m, err := debugger.New(debugger.Options{Adapters: opts.Adapters, Sandbox: opts.Sandbox, MaxSessions: opts.MaxSessions, Lifetime: time.Duration(opts.LifetimeSeconds) * time.Second, MCP: e.mcpRegistry, OnEvent: func(id, event string, data map[string]any) { e.events.publish(event, id, data) }})
	if err != nil {
		return err
	}
	e.dapManager = m
	e.dapGrants = make(map[string]DebugGrant)
	e.dapContext, e.dapCancel = context.WithCancel(context.Background())
	return nil
}
func (e *Engine) closeDebug() error {
	if e.dapCancel != nil {
		e.dapCancel()
	}
	if e.dapManager != nil {
		return e.dapManager.Close()
	}
	return nil
}
func (e *Engine) DebugAdapters() []DebugAdapter {
	if e.dapManager == nil {
		return nil
	}
	return e.dapManager.Adapters()
}
func (e *Engine) SetDebugGrant(ctx context.Context, id string, grant DebugGrant) error {
	if e.dapManager == nil {
		return ErrDebugDisabled
	}
	if err := e.runtimeEventAccess(ctx, id); err != nil {
		return err
	}
	e.dapMu.Lock()
	old := e.dapGrants[id]
	e.dapGrants[id] = grant
	e.dapMu.Unlock()
	if !grant.Enabled || (old.AllowAttach && !grant.AllowAttach) || (old.AllowEvaluate && !grant.AllowEvaluate) {
		return e.dapManager.CloseOwner(id)
	}
	return nil
}
func (e *Engine) debugAccess(ctx context.Context, id string) (DebugGrant, error) {
	if err := ctx.Err(); err != nil {
		return DebugGrant{}, err
	}
	if e.dapManager == nil {
		return DebugGrant{}, ErrDebugDisabled
	}
	e.runsMu.Lock()
	closing := e.closing
	e.runsMu.Unlock()
	if closing {
		return DebugGrant{}, ErrDebugDisabled
	}
	if err := e.runtimeEventAccess(ctx, id); err != nil {
		return DebugGrant{}, err
	}
	e.dapMu.Lock()
	grant := e.dapGrants[id]
	e.dapMu.Unlock()
	if !grant.Enabled {
		return grant, ErrDebugDisabled
	}
	return grant, nil
}
func (e *Engine) DebugSessions(ctx context.Context, id string) ([]DebugSnapshot, error) {
	if _, err := e.debugAccess(ctx, id); err != nil {
		return nil, err
	}
	return e.dapManager.List(id), nil
}
func (e *Engine) DebugStart(ctx context.Context, opts DebugStartOptions) (DebugSnapshot, error) {
	return e.startProgramDebug(ctx, e.dapContext, opts, "human")
}
func (e *Engine) startProgramDebug(ctx, owner context.Context, opts DebugStartOptions, actor string) (DebugSnapshot, error) {
	_, err := e.debugAccess(ctx, opts.SessionID)
	if err != nil {
		return DebugSnapshot{}, err
	}
	pre, err := e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecycleBeforeDebugStart, SessionID: opts.SessionID, RunID: opts.RunID})
	if err != nil {
		return DebugSnapshot{}, err
	}
	if pre.Denied() {
		return DebugSnapshot{}, errors.New("debug start denied: " + pre.FeedbackText())
	}
	identity, err := e.SessionRuntime(ctx, opts.SessionID)
	if err != nil {
		return DebugSnapshot{}, err
	}
	opts.Workspace = identity.Path
	opts.Actor = actor
	// Serialize grant revocation with admission. Revocation cannot miss a start
	// between its grant update and owned-process cleanup.
	e.dapMu.Lock()
	defer e.dapMu.Unlock()
	grant := e.dapGrants[opts.SessionID]
	if !grant.Enabled || (opts.PrepareForAttach && !grant.AllowAttach) {
		return DebugSnapshot{}, ErrDebugDisabled
	}
	opts.AllowEvaluate = grant.AllowEvaluate
	opts.AllowAttach = grant.AllowAttach
	if err = e.runtimeEventAccess(ctx, opts.SessionID); err != nil {
		return DebugSnapshot{}, err
	}
	life, cancel := context.WithCancel(owner)
	stop := context.AfterFunc(ctx, cancel)
	result, err := e.dapManager.Start(life, opts)
	stopped := stop()
	if err != nil || !stopped || ctx.Err() != nil {
		cancel()
		if result.ID != "" {
			_ = e.dapManager.CloseOwner(opts.SessionID)
		}
		return DebugSnapshot{}, errors.Join(err, ctx.Err())
	}
	// Manager lifetime bounds execution; cancelling the call after Start returns
	// does not silently kill a host-launched debugging session.
	done, err := e.dapManager.Done(opts.SessionID, result.ID)
	if err != nil {
		cancel()
		return DebugSnapshot{}, err
	}
	go func() {
		select {
		case <-done:
		case <-owner.Done():
		}
		cancel()
		<-done // AfterDebugStop observes completed cleanup, never just cancellation.
		hookCtx, doneHook := context.WithTimeout(context.Background(), 5*time.Second)
		defer doneHook()
		_, _ = e.DispatchExecutionHook(hookCtx, LifecycleRequest{Event: LifecycleAfterDebugStop, SessionID: opts.SessionID, RunID: opts.RunID, Detail: map[string]any{"debugSessionId": result.ID}})
	}()
	return result, nil
}

func (e *Engine) DebugAttach(ctx context.Context, c DebugControl) (DebugSnapshot, error) {
	c.Actor = "human"
	return e.programAttach(ctx, c)
}
func (e *Engine) programAttach(ctx context.Context, c DebugControl) (DebugSnapshot, error) {
	grant, err := e.debugAccess(ctx, c.SessionID)
	if err != nil {
		return DebugSnapshot{}, err
	}
	if !grant.AllowAttach {
		return DebugSnapshot{}, ErrDebugDisabled
	}
	return e.dapManager.Attach(ctx, c)
}
func (e *Engine) DebugCommand(ctx context.Context, c DebugControl) (DebugSnapshot, error) {
	c.Actor = "human"
	return e.programControl(ctx, c)
}
func (e *Engine) programControl(ctx context.Context, c DebugControl) (DebugSnapshot, error) {
	if _, err := e.debugAccess(ctx, c.SessionID); err != nil {
		return DebugSnapshot{}, err
	}
	return e.dapManager.Control(ctx, c)
}
func (e *Engine) DebugTakeover(ctx context.Context, c DebugControl, owner string) (DebugSnapshot, error) {
	if _, err := e.debugAccess(ctx, c.SessionID); err != nil {
		return DebugSnapshot{}, err
	}
	c.Actor = "human"
	return e.dapManager.Transfer(ctx, c, owner)
}
func (e *Engine) DebugSetBreakpoints(ctx context.Context, c DebugControl, path string, points []DebugBreakpoint) (json.RawMessage, error) {
	c.Actor = "human"
	return e.programBreakpoints(ctx, c, path, points)
}
func (e *Engine) programBreakpoints(ctx context.Context, c DebugControl, path string, points []DebugBreakpoint) (json.RawMessage, error) {
	if _, err := e.debugAccess(ctx, c.SessionID); err != nil {
		return nil, err
	}
	return e.dapManager.Breakpoints(ctx, c, path, points)
}
func (e *Engine) DebugInspect(ctx context.Context, c DebugControl, kind string, reference, start, count int) (json.RawMessage, error) {
	c.Actor = "human"
	return e.programInspect(ctx, c, kind, reference, start, count)
}
func (e *Engine) programInspect(ctx context.Context, c DebugControl, kind string, reference, start, count int) (json.RawMessage, error) {
	if _, err := e.debugAccess(ctx, c.SessionID); err != nil {
		return nil, err
	}
	return e.dapManager.Inspect(ctx, c, kind, reference, start, count)
}
func (e *Engine) DebugEvaluate(ctx context.Context, c DebugControl, frame int, expression string, sideEffects bool) (json.RawMessage, error) {
	c.Actor = "human"
	return e.programEvaluate(ctx, c, frame, expression, sideEffects)
}
func (e *Engine) programEvaluate(ctx context.Context, c DebugControl, frame int, expression string, sideEffects bool) (json.RawMessage, error) {
	grant, err := e.debugAccess(ctx, c.SessionID)
	if err != nil {
		return nil, err
	}
	if sideEffects && !grant.AllowEvaluate {
		return nil, ErrDebugDisabled
	}
	return e.dapManager.Evaluate(ctx, c, frame, expression, sideEffects)
}
