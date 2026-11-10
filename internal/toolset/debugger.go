package toolset

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/internal/debugger"
	"github.com/edynasty/LumenCortex/protocol"
)

// debugStopResult reports which cleanup ran after debug_stop.
type debugStopResult struct {
	debugger.Snapshot
	Cleanup string `json:"cleanup"`
}

// DebugController is already bound to a run's identity and host grant.
// Tool arguments cannot select a Session, Actor, Workspace or authority.
type DebugController interface {
	Start(context.Context, string, string, []string, bool) (debugger.Snapshot, error)
	Attach(context.Context, debugger.Control) (debugger.Snapshot, error)
	Control(context.Context, debugger.Control) (debugger.Snapshot, error)
	ControlAndWait(context.Context, debugger.Control, debugger.WaitOptions) (debugger.Snapshot, debugger.StopInfo, error)
	Disconnect(context.Context, debugger.Control, bool) (debugger.Snapshot, string, error)
	Breakpoints(context.Context, debugger.Control, string, []debugger.Breakpoint) (json.RawMessage, error)
	SetExceptionBreakpoints(context.Context, debugger.Control, []string) (json.RawMessage, error)
	Inspect(context.Context, debugger.Control, string, int, int, int) (json.RawMessage, error)
	Evaluate(context.Context, debugger.Control, int, string, bool) (json.RawMessage, error)
}
type debugToolArgs struct {
	Adapter           string                `json:"adapter_id"`
	Program           string                `json:"program"`
	Args              []string              `json:"args"`
	ID                string                `json:"debug_id"`
	Epoch             uint64                `json:"epoch"`
	Path              string                `json:"path"`
	Breakpoints       []debugger.Breakpoint `json:"breakpoints"`
	Action            string                `json:"action"`
	Thread            int                   `json:"thread_id"`
	Frame             int                   `json:"frame_id"`
	Reference         int                   `json:"variables_reference"`
	Start             int                   `json:"start"`
	Count             int                   `json:"count"`
	Kind              string                `json:"kind"`
	Expression        string                `json:"expression"`
	SideEffects       bool                  `json:"side_effects"`
	PrepareForAttach  bool                  `json:"prepare_for_attach"`
	Wait              *bool                 `json:"wait"`
	TimeoutSec        int                   `json:"timeout_sec"`
	ExceptionFilters  []string              `json:"exception_filters"`
	TerminateDebuggee *bool                 `json:"terminate_debuggee"`
}

func (s *Set) BindDebugger(controller DebugController) error {
	if controller == nil {
		return errors.New("debug controller required")
	}
	base := func() map[string]any {
		return map[string]any{"debug_id": str(), "epoch": map[string]any{"type": "integer", "minimum": 1}}
	}
	integer := func() map[string]any { return map[string]any{"type": "integer", "minimum": 0} }
	type spec struct {
		name, description string
		params            map[string]any
		required          []string
	}
	definitions := []spec{
		{"debug_start", "Launch a configured adapter and workspace program; prepare_for_attach starts a target without attaching and requires a separate host grant.", map[string]any{"adapter_id": str(), "program": str(), "args": map[string]any{"type": "array", "maxItems": 128, "items": str()}, "prepare_for_attach": map[string]any{"type": "boolean"}}, []string{"adapter_id", "program"}},
		{"debug_attach", "Attach to this run's prepared same-sandbox target. No caller-supplied PID or network endpoint.", map[string]any{}, nil},
		{"debug_set_breakpoint", "Replace breakpoints for one validated workspace source. exception_filters selects the adapter's exception breakpoints (see exceptionFilters in the result). Conditions require separate host evaluation authority.", map[string]any{"path": str(), "breakpoints": map[string]any{"type": "array", "maxItems": 128, "items": obj(map[string]any{"line": map[string]any{"type": "integer", "minimum": 1}, "condition": str(), "hitCondition": str()}, "line")}, "exception_filters": map[string]any{"type": "array", "maxItems": 16, "items": str()}}, []string{"path", "breakpoints"}},
		{"debug_continue", "Finish breakpoint configuration or resume a stopped target and (wait, the default) report reason, stopEpoch, the top frames and any exception in one call. wait=false returns immediately with reason=pending; timeout_sec (max 30) bounds the wait. Human takeover makes your old epoch invalid.", map[string]any{"action": map[string]any{"type": "string", "enum": []string{"configure", "continue"}}, "thread_id": integer(), "wait": map[string]any{"type": "boolean"}, "timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 30}}, []string{"action"}},
		{"debug_step", "Step one source line, into a call, or out of a call in the owned debuggee, then (wait, the default) report the new stop and frames. Old frame and variables references become stale; re-read the stack after a step.", map[string]any{"action": map[string]any{"type": "string", "enum": []string{"step_over", "step_into", "step_out"}}, "thread_id": integer(), "wait": map[string]any{"type": "boolean"}, "timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 30}}, []string{"action", "thread_id"}},
		{"debug_pause", "Request a DAP target pause, not an Agent loop pause, and (wait, the default) report the stop.", map[string]any{"thread_id": integer(), "wait": map[string]any{"type": "boolean"}, "timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 30}}, []string{"thread_id"}},
		{"debug_stack", "Inspect bounded threads, stack frames or scopes of the owned target.", map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"threads", "stackTrace", "scopes"}}, "thread_id": integer(), "frame_id": integer(), "start": integer(), "count": integer()}, []string{"kind"}},
		{"debug_variables", "Inspect bounded children of a variablesReference returned by this debugger.", map[string]any{"variables_reference": integer(), "start": integer(), "count": integer()}, []string{"variables_reference"}},
		{"debug_evaluate", "Read one visible variable by name. Arbitrary expressions and calls require side_effects plus a separate host evaluation grant.", map[string]any{"frame_id": integer(), "expression": str(), "side_effects": map[string]any{"type": "boolean"}}, []string{"frame_id", "expression"}},
		{"debug_stop", "Disconnect the adapter and terminate the debuggee (terminate_debuggee, default true); falls back to killing it after 2 s. The result names the cleanup used. Does not stop any other worker.", map[string]any{"terminate_debuggee": map[string]any{"type": "boolean"}}, nil},
	}
	for _, def := range definitions {
		if _, ok := s.tools[def.name]; ok {
			return fmt.Errorf("duplicate tool %s", def.name)
		}
	}
	for _, def := range definitions {
		params := def.params
		required := def.required
		if def.name != "debug_start" {
			for k, v := range base() {
				params[k] = v
			}
			required = append([]string{"debug_id", "epoch"}, required...)
		}
		s.add(protocol.ToolSpec{Name: def.name, Description: def.description, Permission: "exec", MutatesWorkspace: true, Parameters: obj(params, required...)}, func(ctx context.Context, values map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			for k := range values {
				if _, ok := params[k]; !ok {
					return protocol.ToolResult{}, fmt.Errorf("unknown debugger parameter %s", k)
				}
			}
			for _, key := range required {
				if _, ok := values[key]; !ok {
					return protocol.ToolResult{}, fmt.Errorf("missing debugger parameter %s", key)
				}
			}
			raw, err := json.Marshal(values)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			if len(raw) > 64<<10 {
				return protocol.ToolResult{}, errors.New("debugger arguments too large")
			}
			var args debugToolArgs
			decoder := json.NewDecoder(bytes.NewReader(raw))
			decoder.DisallowUnknownFields()
			if err = decoder.Decode(&args); err != nil {
				return protocol.ToolResult{}, err
			}
			if def.name != "debug_start" && (args.ID == "" || args.Epoch == 0) {
				return protocol.ToolResult{}, errors.New("debug_id and epoch required")
			}
			c := debugger.Control{DebugID: args.ID, Epoch: args.Epoch, ThreadID: args.Thread}
			var value any
			switch def.name {
			case "debug_start":
				value, err = controller.Start(ctx, args.Adapter, args.Program, args.Args, args.PrepareForAttach)
			case "debug_attach":
				value, err = controller.Attach(ctx, c)
			case "debug_set_breakpoint":
				if args.ExceptionFilters != nil {
					var applied json.RawMessage
					if applied, err = controller.SetExceptionBreakpoints(ctx, c, args.ExceptionFilters); err != nil {
						return protocol.ToolResult{}, err
					}
					_ = applied
				}
				value, err = controller.Breakpoints(ctx, c, args.Path, args.Breakpoints)
			case "debug_stack":
				ref := args.Thread
				if args.Kind == "scopes" {
					ref = args.Frame
				}
				value, err = controller.Inspect(ctx, c, args.Kind, ref, args.Start, args.Count)
			case "debug_variables":
				value, err = controller.Inspect(ctx, c, "variables", args.Reference, args.Start, args.Count)
			case "debug_evaluate":
				value, err = controller.Evaluate(ctx, c, args.Frame, args.Expression, args.SideEffects)
			case "debug_stop":
				terminate := args.TerminateDebuggee == nil || *args.TerminateDebuggee
				var snapshot debugger.Snapshot
				var cleanup string
				snapshot, cleanup, err = controller.Disconnect(ctx, c, terminate)
				value = debugStopResult{Snapshot: snapshot, Cleanup: cleanup}
			default:
				c.Action = args.Action
				switch def.name {
				case "debug_pause":
					c.Action = "pause"
				case "debug_continue":
					if c.Action != "configure" && c.Action != "continue" {
						return protocol.ToolResult{}, errors.New("invalid continue action")
					}
				case "debug_step":
					if c.Action != "step_over" && c.Action != "step_into" && c.Action != "step_out" {
						return protocol.ToolResult{}, errors.New("invalid step action")
					}
				}
				if err == nil {
					wait := args.Wait == nil || *args.Wait
					var stop debugger.StopInfo
					_, stop, err = controller.ControlAndWait(ctx, c, debugger.WaitOptions{Timeout: time.Duration(args.TimeoutSec) * time.Second, Async: !wait})
					value = stop
				}
			}
			if err != nil {
				return protocol.ToolResult{}, err
			}
			return jsonResult(value)
		})
	}
	return nil
}
