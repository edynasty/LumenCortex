package evalruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/streambuf"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/santhosh-tekuri/jsonschema/v5"
)

// maxToolContent bounds one tool result handed back to a worker.
const maxToolContent = 64 << 10

// activeCall is the callback surface of exactly one execution: the host-authorized
// caller, the tool names this request may use, its budget, and its audit state.
//
// It is bound to an immutable execution identity (the executor agent, the request,
// the kernel and worker generation). Every callback re-validates that identity
// against the session before it reaches the host caller, so a capability that was
// revoked, expired or superseded by a new kernel generation stops working even if
// the worker keeps the connection open.
type activeCall struct {
	s      *session
	ctx    context.Context
	caller evalapi.ToolCaller

	specs    map[string]protocol.ToolSpec
	schemas  map[string]*jsonschema.Schema
	schemaMu sync.Mutex

	allowedAll bool
	allowed    map[string]bool
	budget     evalapi.Budget
	deadline   time.Time
	depth      int

	// executor is the agent that owns this cell. It is not the session's
	// creator: a shared kernel executes cells for several agents, and every
	// audit record names the agent that actually ran.
	executor string
	// generation is the kernel generation this capability belongs to; worker is
	// the process allowed to use it (nil in direct unit tests).
	generation uint64
	worker     *worker

	calls atomic.Int32

	out *streambuf.HeadTail
	err *streambuf.HeadTail

	requestID string
	cellID    string
	runID     string
	trialID   string
}

func newActiveCall(ctx context.Context, s *session, caller evalapi.ToolCaller, req evalapi.Request, budget evalapi.Budget, deadline time.Time) *activeCall {
	call := &activeCall{
		s:          s,
		ctx:        ctx,
		caller:     caller,
		specs:      make(map[string]protocol.ToolSpec),
		schemas:    make(map[string]*jsonschema.Schema),
		allowed:    make(map[string]bool),
		allowedAll: req.AllowsAllCapabilities(),
		budget:     budget,
		deadline:   deadline,
		executor:   s.agentID,
		generation: s.generationValue(),
		out:        boundedText(budget.OutputBytes),
		err:        boundedText(budget.OutputBytes),
		requestID:  req.ID,
		cellID:     req.CellID,
		runID:      req.RunID,
		trialID:    req.TrialID,
	}
	for _, name := range req.AllowedCapabilities {
		if name != "*" {
			call.allowed[name] = true
		}
	}
	return call
}

// bindExecution pins this capability to the worker and agent that will use it.
func (a *activeCall) bindExecution(agentID string, w *worker) {
	if agentID != "" {
		a.executor = agentID
	}
	if w != nil {
		a.worker = w
		a.generation = w.generation
	}
}

// authorize re-validates the capability against the live session. It is the
// authority check that makes a callback belong to one execution: the executor
// must still hold execute rights (a revoked or expired grant fails here), the
// kernel generation must be the one this cell was admitted in, the worker must
// still be the session's current worker, and the execution must still be running.
func (a *activeCall) authorize() (string, bool) {
	if a.ctx != nil && a.ctx.Err() != nil {
		return "the execution that owns this callback is no longer running", false
	}
	s := a.s
	s.mu.Lock()
	closed := s.closed
	current := s.worker
	generation := s.generation
	s.mu.Unlock()
	if closed {
		return "the kernel is closed", false
	}
	if a.generation != generation {
		return fmt.Sprintf("kernel generation %d is no longer current (now %d)", a.generation, generation), false
	}
	if a.worker != nil && current != a.worker {
		return "the worker that issued this callback is no longer the kernel's worker", false
	}
	if !s.canExecute(a.executor) {
		return fmt.Sprintf("agent %q no longer has execute access to this kernel", a.executor), false
	}
	return "", true
}

// loadTools asks the host caller for the tool surface it is willing to expose and
// keeps only the tools this request named.
func (a *activeCall) loadTools(ctx context.Context) ([]protocol.ToolSpec, error) {
	if a.caller == nil {
		return nil, nil
	}
	specs, err := a.caller.Tools(ctx)
	if err != nil {
		return nil, err
	}
	exposed := make([]protocol.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if spec.Name == "" {
			continue
		}
		if !a.allowedAll && !a.allowed[spec.Name] {
			continue
		}
		a.specs[spec.Name] = spec
		exposed = append(exposed, spec)
	}
	return exposed, nil
}

func (a *activeCall) invoke(req wireToolCall) (any, error) {
	a.s.m.emit(evalapi.Event{
		Type: evalapi.EventToolCall, SessionID: a.s.id, OwnerID: a.s.ownerID, AgentID: a.executor,
		RequestID: a.requestID, CellID: a.cellID, RunID: a.runID, TrialID: a.trialID,
		Generation: a.generation,
		Data:       map[string]any{"tool": req.Name, "callId": req.CallID},
	})
	deny := func(reason string) (any, error) {
		a.s.m.emit(evalapi.Event{
			Type: evalapi.EventToolDenied, SessionID: a.s.id, OwnerID: a.s.ownerID, AgentID: a.executor,
			RequestID: a.requestID, CellID: a.cellID, RunID: a.runID, TrialID: a.trialID,
			Generation: a.generation,
			Data:       map[string]any{"tool": req.Name, "callId": req.CallID, "reason": reason},
		})
		return map[string]any{"ok": false, "denied": true, "content": reason}, nil
	}
	// A callback belongs to one execution. The identity is re-checked before
	// anything else, so a revoked grant, an expired grant, a superseded kernel
	// generation or a stale worker cannot use a capability that outlived it.
	if reason, ok := a.authorize(); !ok {
		a.s.m.emit(evalapi.Event{
			Type: evalapi.EventCallbackUnauthorized, SessionID: a.s.id, OwnerID: a.s.ownerID, AgentID: a.executor,
			RequestID: a.requestID, CellID: a.cellID, RunID: a.runID, TrialID: a.trialID,
			Generation: a.generation,
			Data:       map[string]any{"tool": req.Name, "callId": req.CallID, "reason": reason},
		})
		return deny(reason)
	}
	if a.caller == nil {
		return deny("this execution has no authorized tool caller")
	}
	if req.CallID == "" {
		return deny("tool callback requires a unique call id")
	}
	// Callback ids are unique inside one kernel generation. A worker numbers its
	// callbacks from its own counter, which restarts with its process, so the
	// guard is per generation rather than per session lifetime.
	if err := a.s.trackCallbackID(a.generation, req.CallID); err != nil {
		return deny(err.Error())
	}
	if _, known := a.specs[req.Name]; !known {
		return deny(fmt.Sprintf("tool %q is not offered to this execution", req.Name))
	}
	if !a.allowedAll && !a.allowed[req.Name] {
		return deny(fmt.Sprintf("tool %q is not in the request's allowed capabilities", req.Name))
	}
	depth := evalapi.CallDepth(a.ctx) + 1
	if a.budget.MaxDepth > 0 && depth > a.budget.MaxDepth {
		return deny(fmt.Sprintf("callback recursion depth %d exceeds the limit of %d", depth, a.budget.MaxDepth))
	}
	if err := a.validate(req); err != nil {
		return deny("invalid arguments: " + err.Error())
	}
	// Only a validated, authorized call consumes the callback budget. The
	// reservation is a compare-and-swap: concurrent callbacks from the worker's
	// own goroutines can never overshoot MaxCallbacks.
	if !a.reserveCallback() {
		return deny(fmt.Sprintf("callback budget of %d exhausted", a.budget.MaxCallbacks))
	}

	ctx := evalapi.WithCallDepth(a.ctx, depth)
	timeout := 60 * time.Second
	if !a.deadline.IsZero() {
		remaining := time.Until(a.deadline)
		if remaining <= 0 {
			return map[string]any{"ok": false, "interrupted": true, "content": "execution deadline passed before the tool call"}, nil
		}
		if remaining < timeout {
			timeout = remaining
		}
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := a.caller.Call(callCtx, req.Name, req.Arguments)
	if err != nil {
		if callCtx.Err() != nil {
			return map[string]any{"ok": false, "interrupted": true, "content": "tool call cancelled: " + callCtx.Err().Error()}, nil
		}
		return map[string]any{"ok": false, "content": err.Error()}, nil
	}
	content := boundText(result.Content, maxToolContent)
	return map[string]any{
		"ok":               result.OK,
		"denied":           result.Denied,
		"permission":       result.Permission,
		"interrupted":      result.Interrupted,
		"mutatesWorkspace": result.MutatesWorkspace,
		"content":          content,
	}, nil
}

// reserveCallback atomically takes one slot of the callback budget. A zero or
// negative MaxCallbacks means the budget is not bounded by count.
func (a *activeCall) reserveCallback() bool {
	limit := a.budget.MaxCallbacks
	if limit <= 0 {
		a.calls.Add(1)
		return true
	}
	for {
		current := a.calls.Load()
		if int(current) >= limit {
			return false
		}
		if a.calls.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

func (a *activeCall) validate(req wireToolCall) error {
	spec := a.specs[req.Name]
	if len(spec.Parameters) == 0 {
		return nil
	}
	schema, err := a.schemaFor(req.Name, spec)
	if err != nil {
		return fmt.Errorf("tool schema is not usable: %w", err)
	}
	if schema == nil {
		return nil
	}
	if req.Arguments == nil {
		req.Arguments = map[string]any{}
	}
	return schema.Validate(req.Arguments)
}

func (a *activeCall) schemaFor(name string, spec protocol.ToolSpec) (*jsonschema.Schema, error) {
	a.schemaMu.Lock()
	defer a.schemaMu.Unlock()
	if schema, ok := a.schemas[name]; ok {
		return schema, nil
	}
	data, err := json.Marshal(spec.Parameters)
	if err != nil {
		return nil, err
	}
	schema, err := jsonschema.CompileString(name+".json", string(data))
	if err != nil {
		return nil, err
	}
	a.schemas[name] = schema
	return schema, nil
}

// boundText keeps a bounded head and tail of a string.
func boundText(text string, limit int64) string {
	if limit <= 0 || int64(len(text)) <= limit {
		return text
	}
	head := int(limit / 2)
	tail := int(limit) - head
	if head < 1 {
		head = 1
	}
	return text[:head] + fmt.Sprintf("\n... [truncated %d bytes] ...\n", int64(len(text))-limit) + text[len(text)-tail:]
}
