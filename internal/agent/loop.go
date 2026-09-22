package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

const defaultSystemPrompt = `You are LumenCortex Agent, an autonomous coding agent.
Inspect before editing. Use tools until the requested outcome is implemented and verified. Keep changes scoped. Do not claim completion without evidence. When a Workflow Contract is active, obey its current action, tool boundary, outcomes, routes, and gates.`

type Loop struct {
	Provider protocol.Provider
	Store    Store
	Tools    Tools
	Emit     func(Event)
}

func (l *Loop) Run(ctx context.Context, sessionID string, opts Options) (Result, error) {
	if l.Provider == nil || l.Store == nil || l.Tools == nil {
		return Result{}, errors.New("provider, store, and tools are required")
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 24
	}
	if opts.RecentMessages <= 0 {
		opts.RecentMessages = 8
	}
	if opts.MaxToolCallsPerStep <= 0 {
		opts.MaxToolCallsPerStep = 8
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = defaultSystemPrompt
	}

	state, err := l.Store.Get(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	wasInterrupted := state.Status == "interrupted"
	wf, err := restoreWorkflow(state.Metadata, opts.WorkflowJSON)
	if err != nil {
		return Result{}, err
	}
	if err := l.ensureInitialMessage(ctx, state); err != nil {
		return Result{}, err
	}
	usage := state.Usage
	running := "running"
	state.Status = running
	if err := l.Store.Update(ctx, sessionID, SessionPatch{Status: &running, ClearFinal: true, ClearError: true}); err != nil {
		return Result{}, err
	}
	startStep, err := l.Store.NextStep(ctx, sessionID)
	if err != nil {
		return Result{}, err
	}
	if wasInterrupted {
		l.checkpoint(sessionID, "agent.resume", map[string]any{
			"nextStep": startStep,
		})
	}

	for turn := 0; turn < opts.MaxSteps; turn++ {
		if err := ctx.Err(); err != nil {
			return l.interrupt(ctx, state, usage, err)
		}
		step := startStep + int64(turn)
		if wf != nil && len(wf.WaitingHumanGates()) > 0 {
			metadata := withWorkflow(state.Metadata, wf)
			status := "waiting_gate"
			_ = l.Store.Update(context.Background(), sessionID, SessionPatch{Status: &status, Metadata: metadata, Usage: &usage})
			l.checkpoint(sessionID, "agent.safe", map[string]any{
				"phase": "waiting_gate",
				"step": step,
				"gates": wf.WaitingHumanGates(),
			})
			l.emit("workflow.gate_waiting", sessionID, map[string]any{"step": step, "gates": wf.WaitingHumanGates()})
			return Result{SessionID: sessionID, Status: status, Usage: usage, WaitingGate: true, Workflow: wf.Summary()}, nil
		}

		allowlist := opts.ToolAllowlist
		if wf != nil {
			allowlist = wf.EffectiveAllowlist(allowlist)
		}
		specs := filterDeniedTools(l.Tools.Specs(allowlist), opts.ToolDenylist)
		messages, err := l.buildMessages(ctx, sessionID, opts, wf)
		if err != nil {
			return Result{}, err
		}
		usage.Requests++
		l.emit("llm.request", sessionID, map[string]any{"step": step, "model": l.Provider.Model(), "messages": len(messages), "tools": toolNames(specs)})
		response, err := l.Provider.Complete(ctx, protocol.ProviderRequest{
			Messages: messages, Tools: specs, ToolChoice: "auto", Temperature: opts.Temperature, MaxTokens: opts.MaxTokens,
		})
		if err != nil {
			return l.interrupt(ctx, state, usage, err)
		}
		addUsage(&usage, response.Usage)
		assistant := response.Message
		assistant.Role = "assistant"
		calls := assistant.ToolCalls
		if len(calls) > opts.MaxToolCallsPerStep {
			calls = calls[:opts.MaxToolCallsPerStep]
			assistant.ToolCalls = calls
			l.emit("tools.deferred", sessionID, map[string]any{"step": step, "requested": len(response.Message.ToolCalls), "executing": len(calls)})
		}
		if _, err := l.Store.AppendMessage(ctx, sessionID, assistant); err != nil {
			return Result{}, err
		}
		record := stepRecord{Step: step, FinishReason: response.FinishReason, Content: assistant.Content}

		if len(calls) == 0 {
			if strings.TrimSpace(assistant.Content) == "" {
				return l.interrupt(ctx, state, usage, errors.New("model returned neither tool calls nor final content"))
			}
			if wf != nil && !wf.CanFinish() {
				reason := wf.BlockReason()
				record.Workflow = wf.Summary()
				if err := l.Store.AppendStep(ctx, sessionID, step, record); err != nil {
					return Result{}, err
				}
				correction := protocol.Message{Role: "user", Content: "Workflow contract rejected completion: " + reason + ". Continue the current action and obtain the required evidence or gate approval."}
				if _, err := l.Store.AppendMessage(ctx, sessionID, correction); err != nil {
					return Result{}, err
				}
				state.Metadata = withWorkflow(state.Metadata, wf)
				_ = l.Store.Update(ctx, sessionID, SessionPatch{Metadata: state.Metadata, Usage: &usage})
				l.emit("workflow.blocked_final", sessionID, map[string]any{"step": step, "reason": reason})
				continue
			}
			status := "completed"
			final := assistant.Content
			if wf != nil {
				record.Workflow = wf.Summary()
				state.Metadata = withWorkflow(state.Metadata, wf)
			}
			if err := l.Store.AppendStep(ctx, sessionID, step, record); err != nil {
				return Result{}, err
			}
			if err := l.Store.Update(ctx, sessionID, SessionPatch{Status: &status, Metadata: state.Metadata, Final: &final, Usage: &usage}); err != nil {
				return Result{}, err
			}
			l.emit("session.complete", sessionID, map[string]any{"step": step})
			return Result{SessionID: sessionID, Status: status, Final: final, Usage: usage, Workflow: workflowSummary(wf)}, nil
		}

		for _, call := range calls {
			args := map[string]any{}
			if len(call.Arguments) > 0 {
				if err := json.Unmarshal(call.Arguments, &args); err != nil {
					result := protocol.ToolResult{OK: false, Content: fmt.Sprintf(`{"error":%q}`, "invalid tool arguments: "+err.Error())}
					if _, err := l.persistToolResult(ctx, sessionID, call, args, result); err != nil {
						return Result{}, err
					}
					continue
				}
			}
			denied := !containsTool(specs, call.Name) || (wf != nil && !wf.IsToolAllowed(call.Name))
			var toolResult protocol.ToolResult
			if denied {
				toolResult = protocol.ToolResult{OK: false, Denied: true, Permission: "workflow", Content: "tool is not allowed in the current working set"}
			} else {
				l.emit("tool.start", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "args": args})
				toolResult, err = l.Tools.Execute(ctx, call.Name, args, func(output protocol.ToolOutput) {
					l.emit("tool.output", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "stream": output.Stream, "chunk": output.Chunk})
				})
				if err != nil {
					if ctx.Err() != nil {
						return l.interrupt(ctx, state, usage, ctx.Err())
					}
					toolResult = protocol.ToolResult{OK: false, Content: fmt.Sprintf(`{"error":%q}`, err.Error())}
				}
			}
			l.emit("tool.end", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "ok": toolResult.OK, "denied": toolResult.Denied})
			if wf != nil {
				update, wfErr := wf.ObserveTool(workflow.ToolObservation{Tool: call.Name, Args: args, Result: map[string]any{"ok": toolResult.OK, "denied": toolResult.Denied, "permission": toolResult.Permission, "content": toolResult.Content}, Step: step})
				if wfErr != nil {
					return Result{}, wfErr
				}
				if len(update.ChangedFacts) > 0 {
					l.emit("workflow.facts", sessionID, map[string]any{"step": step, "facts": update.ChangedFacts})
				}
				if update.Transition != nil {
					l.emit("workflow.transition", sessionID, map[string]any{"step": step, "from": update.Transition.From, "to": update.Transition.To, "reason": update.Transition.Reason})
				}
				state.Metadata = withWorkflow(state.Metadata, wf)
			}
			messageSeq, err := l.persistToolResult(ctx, sessionID, call, args, toolResult)
			if err != nil {
				return Result{}, err
			}
			l.checkpoint(sessionID, "agent.safe", map[string]any{
				"phase": "tool_result",
				"step": step,
				"toolCallId": call.ID,
				"tool": call.Name,
				"messageSeq": messageSeq,
				"ok": toolResult.OK,
				"denied": toolResult.Denied,
			})
			record.ToolCalls = append(record.ToolCalls, toolCallRecord{ID: call.ID, Name: call.Name, Args: args, OK: toolResult.OK, Denied: toolResult.Denied})
		}
		if wf != nil {
			record.Workflow = wf.Summary()
		}
		if err := l.Store.AppendStep(ctx, sessionID, step, record); err != nil {
			return Result{}, err
		}
		if err := l.Store.Update(ctx, sessionID, SessionPatch{Metadata: state.Metadata, Usage: &usage}); err != nil {
			return Result{}, err
		}
		l.checkpoint(sessionID, "agent.safe", map[string]any{
			"phase": "step_complete",
			"step": step,
			"nextStep": step + 1,
		})
	}
	return l.interrupt(ctx, state, usage, fmt.Errorf("agent reached max steps: %d", opts.MaxSteps))
}

func (l *Loop) ensureInitialMessage(ctx context.Context, state SessionState) error {
	messages, err := l.Store.RecentMessages(ctx, state.ID, 1)
	if err != nil {
		return err
	}
	if len(messages) == 0 && strings.TrimSpace(state.Goal) != "" {
		_, err = l.Store.AppendMessage(ctx, state.ID, protocol.Message{Role: "user", Content: state.Goal})
	}
	return err
}

func (l *Loop) buildMessages(ctx context.Context, sessionID string, opts Options, wf *workflow.Runtime) ([]protocol.Message, error) {
	recent, err := l.Store.RecentMessages(ctx, sessionID, opts.RecentMessages)
	if err != nil {
		return nil, err
	}
	messages := []protocol.Message{{Role: "system", Content: opts.SystemPrompt}}
	if strings.TrimSpace(opts.AdditionalSystemPrompt) != "" {
		messages = append(messages, protocol.Message{Role: "system", Content: opts.AdditionalSystemPrompt})
	}
	if wf != nil {
		messages = append(messages, protocol.Message{Role: "system", Content: wf.Prompt()})
	}
	messages = append(messages, recent...)
	return messages, nil
}

func (l *Loop) persistToolResult(ctx context.Context, sessionID string, call protocol.ToolCall, args map[string]any, result protocol.ToolResult) (int64, error) {
	return l.Store.AppendMessage(ctx, sessionID, protocol.Message{Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: result.Content})
}

func (l *Loop) checkpoint(sessionID, reason string, payload any) {
	store, ok := l.Store.(CheckpointStore)
	if !ok {
		return
	}
	if err := store.AppendCheckpoint(context.Background(), sessionID, reason, payload); err != nil {
		l.emit("checkpoint.error", sessionID, map[string]any{"reason": reason, "error": err.Error()})
		return
	}
	l.emit("checkpoint.saved", sessionID, map[string]any{"reason": reason})
}

func (l *Loop) interrupt(ctx context.Context, state SessionState, usage protocol.Usage, cause error) (Result, error) {
	status := "interrupted"
	errorData := map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "message": cause.Error()}
	_ = l.Store.Update(context.Background(), state.ID, SessionPatch{Status: &status, Metadata: state.Metadata, Usage: &usage, Error: errorData})
	l.emit("session.interrupted", state.ID, map[string]any{"error": cause.Error()})
	return Result{SessionID: state.ID, Status: status, Usage: usage}, cause
}

func (l *Loop) emit(kind, sessionID string, data map[string]any) {
	if l.Emit != nil {
		l.Emit(Event{Type: kind, SessionID: sessionID, Data: data})
	}
}

func restoreWorkflow(metadata map[string]any, supplied []byte) (*workflow.Runtime, error) {
	var persisted *workflow.Snapshot
	if metadata != nil {
		if rawValue, ok := metadata["workflow"]; ok {
			raw, err := json.Marshal(rawValue)
			if err != nil {
				return nil, err
			}
			var snap workflow.Snapshot
			if err := json.Unmarshal(raw, &snap); err != nil {
				return nil, err
			}
			persisted = &snap
		}
	}
	var suppliedDef *workflow.Definition
	var err error
	if len(supplied) > 0 {
		suppliedDef, err = workflow.Parse(supplied)
		if err != nil {
			return nil, err
		}
	}
	if persisted == nil && suppliedDef == nil {
		return nil, nil
	}
	if persisted != nil {
		if persisted.Definition == nil {
			return nil, errors.New("persisted workflow is missing its definition")
		}
		if suppliedDef != nil {
			left, _ := json.Marshal(persisted.Definition)
			right, _ := json.Marshal(suppliedDef)
			if !bytes.Equal(left, right) {
				return nil, errors.New("workflow definition differs from the contract persisted in this session")
			}
		}
		return workflow.New(persisted.Definition, &workflow.State{CurrentAction: persisted.CurrentAction, Facts: persisted.Facts, FactSources: persisted.FactSources, History: persisted.History, Status: persisted.Status})
	}
	return workflow.New(suppliedDef, nil)
}

func withWorkflow(metadata map[string]any, wf *workflow.Runtime) map[string]any {
	out := map[string]any{}
	for key, value := range metadata {
		out[key] = value
	}
	if wf != nil {
		out["workflow"] = wf.Snapshot()
	}
	return out
}

func workflowSummary(wf *workflow.Runtime) any {
	if wf == nil {
		return nil
	}
	return wf.Summary()
}

func toolNames(specs []protocol.ToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Name)
	}
	return out
}

func containsTool(specs []protocol.ToolSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func addUsage(total *protocol.Usage, value protocol.Usage) {
	total.PromptTokens += value.PromptTokens
	total.CompletionTokens += value.CompletionTokens
	if value.TotalTokens > 0 {
		total.TotalTokens += value.TotalTokens
	} else {
		total.TotalTokens += value.PromptTokens + value.CompletionTokens
	}
}


func filterDeniedTools(specs []protocol.ToolSpec, deny []string) []protocol.ToolSpec {
	if len(deny) == 0 {
		return specs
	}
	blocked := make(map[string]struct{}, len(deny))
	for _, name := range deny {
		name = strings.TrimSpace(name)
		if name != "" {
			blocked[name] = struct{}{}
		}
	}
	if len(blocked) == 0 {
		return specs
	}
	out := make([]protocol.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if _, denied := blocked[spec.Name]; denied {
			continue
		}
		out = append(out, spec)
	}
	return out
}
