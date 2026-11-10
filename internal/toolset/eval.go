package toolset

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/protocol"
)

// EvalController is bound to one LC run; model input never selects its authority.
type EvalController interface {
	Runtimes(context.Context) ([]evalapi.Manifest, error)
	Sessions(context.Context) ([]evalapi.Session, error)
	Create(context.Context, string) (evalapi.Session, error)
	Execute(context.Context, evalapi.Request) (evalapi.Result, error)
	// Control resets or closes a session.
	Control(context.Context, string, string) error
	// Interrupt stops exactly the execution the target names.
	Interrupt(context.Context, string, evalapi.InterruptTarget) (evalapi.InterruptOutcome, error)
	Transfer(context.Context, string, string, string, string, string) (evalapi.Artifact, error)
	// Inspect lists the kernel's live names and types without running a cell.
	Inspect(context.Context, string, evalapi.InspectOptions) (evalapi.Inspection, error)
}

// BindEval registers the language-neutral surface only for an explicitly enabled host.
// It is called while constructing the set, before any execution or concurrent catalog reads.
func (s *Set) BindEval(controller EvalController) error {
	if controller == nil {
		return errors.New("eval controller is required")
	}
	for _, name := range []string{"eval_runtimes", "eval_sessions", "eval_create", "eval", "eval_control", "eval_artifact", "eval_inspect"} {
		if _, exists := s.tools[name]; exists {
			return fmt.Errorf("duplicate tool: %s", name)
		}
	}
	s.add(protocol.ToolSpec{Name: "eval_runtimes", Description: "Discover available execution runtimes and their actual capabilities. Registration does not imply installed dependencies or supported isolation.", Permission: "read", Parameters: obj(map[string]any{})},
		func(ctx context.Context, _ map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			value, err := controller.Runtimes(ctx)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			return jsonResult(value)
		})
	s.add(protocol.ToolSpec{Name: "eval_sessions", Description: "List your authorized execution sessions, generation and live worker state. Different runtimes never share an object heap.", Permission: "read", Parameters: obj(map[string]any{})},
		func(ctx context.Context, _ map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			value, err := controller.Sessions(ctx)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			return jsonResult(value)
		})
	s.add(protocol.ToolSpec{Name: "eval_create", Description: "Create an isolated persistent execution session in a discovered runtime. No packages are installed; no namespace is shared implicitly.", Permission: "exec", Parameters: obj(map[string]any{"runtime_id": str()}, "runtime_id")},
		func(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			id := strings.TrimSpace(stringArg(args, "runtime_id"))
			if id == "" {
				return protocol.ToolResult{}, errors.New("runtime_id is required")
			}
			value, err := controller.Create(ctx, id)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			return jsonResult(value)
		})
	s.add(protocol.ToolSpec{Name: "eval", Description: "Execute one cell in an authorized persistent runtime. Variables survive successful cells; exceptions do not roll back side effects. Cancel/timeout destroys worker state. A crashed cell is never automatically replayed. Inspect stateStatus and generation before reusing variables. Persist final code and verify in a clean process; a successful cell is not semantic acceptance.", Permission: "exec", MutatesWorkspace: true, Parameters: obj(map[string]any{"session_id": str(), "cell_id": str(), "code": str(), "timeout_sec": map[string]any{"type": "integer", "minimum": 1, "maximum": 1800}}, "session_id", "cell_id", "code")},
		func(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			id, cell, code := stringArg(args, "session_id"), stringArg(args, "cell_id"), stringArg(args, "code")
			if id == "" || cell == "" || strings.TrimSpace(code) == "" {
				return protocol.ToolResult{}, errors.New("session_id, cell_id and code are required")
			}
			seconds := 120
			if value, ok := args["timeout_sec"]; ok {
				switch n := value.(type) {
				case float64:
					if n != float64(int(n)) {
						return protocol.ToolResult{}, errors.New("timeout_sec must be an integer")
					}
					seconds = int(n)
				case int:
					seconds = n
				default:
					return protocol.ToolResult{}, errors.New("timeout_sec must be an integer")
				}
				if seconds < 1 || seconds > 1800 {
					return protocol.ToolResult{}, errors.New("timeout_sec outside 1..1800")
				}
			}
			deadline := time.Now().Add(time.Duration(seconds) * time.Second)
			if d, ok := protocol.ToolDeadline(ctx); ok && d.Before(deadline) {
				deadline = d
			}
			if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
				deadline = d
			}
			value, err := controller.Execute(ctx, evalapi.Request{ID: protocol.ToolCallID(ctx), SessionID: id, CellID: cell, Code: code, Deadline: deadline})
			if err != nil {
				return protocol.ToolResult{}, err
			}
			result, err := jsonResult(value)
			result.OK = value.Succeeded()
			result.MutatesWorkspace = true
			result.Interrupted = value.Interrupted()
			return result, err
		})
	s.add(protocol.ToolSpec{Name: "eval_control", Description: "Interrupt, reset, restart or close your execution session. restart replaces the kernel process: live variables, functions and imported modules are discarded, files it wrote are kept. reset additionally clears the kernel workspace. These operations stop its owned process tree; reset discards variables. An interrupt names the execution it stops: pass request_id (eval_sessions shows the running one as activeRequestId); an interrupt whose execution already finished stops nothing. Executor/observer grants cannot reset or close the owner's kernel.", Permission: "exec", Parameters: obj(map[string]any{"session_id": str(), "action": map[string]any{"type": "string", "enum": []string{"interrupt", "reset", "restart", "close"}}, "request_id": str()}, "session_id", "action")},
		func(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			id, action := stringArg(args, "session_id"), stringArg(args, "action")
			if action == "interrupt" {
				requestID := stringArg(args, "request_id")
				if strings.TrimSpace(requestID) == "" {
					return protocol.ToolResult{}, errors.New("interrupt needs request_id: the execution to stop (eval_sessions lists the running one as activeRequestId)")
				}
				outcome, err := controller.Interrupt(ctx, id, evalapi.InterruptTarget{RequestID: requestID})
				if err != nil {
					return protocol.ToolResult{}, err
				}
				return jsonResult(outcome)
			}
			if err := controller.Control(ctx, id, action); err != nil {
				return protocol.ToolResult{}, err
			}
			return jsonResult(map[string]string{"status": "completed"})
		})
	s.add(protocol.ToolSpec{Name: "eval_inspect", Description: "List names and types held by your kernel without running code. Values are never rendered, so inspection cannot execute a __repr__ or block the kernel. Use it after a cell to confirm which names exist before a later cell assumes them; it is not evidence that the values are correct.", Permission: "read", Parameters: obj(map[string]any{"session_id": str(), "names": map[string]any{"type": "array", "maxItems": 100, "items": str()}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 1000}}, "session_id")},
		func(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			id := strings.TrimSpace(stringArg(args, "session_id"))
			if id == "" {
				return protocol.ToolResult{}, errors.New("session_id is required")
			}
			opts := evalapi.InspectOptions{Limit: intArg(args, "limit", 0)}
			if raw, ok := args["names"].([]any); ok {
				for _, item := range raw {
					if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
						opts.Names = append(opts.Names, strings.TrimSpace(name))
					}
				}
			}
			value, err := controller.Inspect(ctx, id, opts)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			return jsonResult(value)
		})
	s.add(protocol.ToolSpec{Name: "eval_artifact", Description: "Explicitly transfer a bounded artifact: import from your authorized workspace into a kernel; export a kernel artifact to your workspace; copy a granted artifact to another authorized runtime. Data provenance and hashes are retained. Kernel files are private until explicitly exported.", Permission: "exec", MutatesWorkspace: true, Parameters: obj(map[string]any{"action": map[string]any{"type": "string", "enum": []string{"import", "export", "copy"}}, "session_id": str(), "artifact_id": str(), "path": str(), "target_session_id": str()}, "action", "session_id")},
		func(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			artifact, err := controller.Transfer(ctx, stringArg(args, "action"), stringArg(args, "session_id"), stringArg(args, "artifact_id"), stringArg(args, "path"), stringArg(args, "target_session_id"))
			if err != nil {
				return protocol.ToolResult{}, err
			}
			result, err := jsonResult(artifact)
			result.MutatesWorkspace = stringArg(args, "action") == "export"
			return result, err
		})
	return nil
}
