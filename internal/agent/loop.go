package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

const defaultSystemPrompt = `You are LumenCortex Agent, an autonomous coding agent.
Inspect before editing. Use tools until the requested outcome is implemented and verified. Keep changes scoped. Do not claim completion without evidence. When a Workflow Contract is active, obey its current action, tool boundary, outcomes, routes, and gates.`

type Loop struct {
	Provider       protocol.Provider
	ProviderName   string
	ProviderChains map[string][]ProviderBinding
	ProviderHealth *cognition.HealthRegistry
	DecisionLayer  *cognition.DecisionLayer
	Store          Store
	Tools          Tools
	Emit           func(Event)
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
	workUnits, err := restoreWorkUnits(state.Metadata, opts.WorkUnitsJSON)
	if err != nil {
		return Result{}, err
	}
	if workUnits == nil && opts.CognitionEnabled {
		workUnits = cognition.NewWorkUnitManager(nil)
	}
	if workUnits != nil {
		state.Metadata = withWorkUnits(state.Metadata, workUnits)
	}
	if err := l.ensureInitialMessage(ctx, state); err != nil {
		return Result{}, err
	}
	usage := state.Usage
	cognitiveProgress := restoreCognitiveProgress(state.Metadata)
	router := cognition.Router{}
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
		specs := l.Tools.Specs(allowlist)
		if workUnits != nil {
			specs = append(specs, filterWorkUnitSpecs(allowlist)...)
		}
		specs = filterDeniedTools(specs, opts.ToolDenylist)
		activeWorkUnit, _ := currentWorkUnit(workUnits)
		var cognitivePlan *cognition.Plan
		if opts.CognitionEnabled {
			input := cognition.Input{
				Goal: state.Goal,
				Progress: cognitiveProgress,
			}
			if activeWorkUnit != nil {
				input.Focus = activeWorkUnit.Goal
			}
			if l.DecisionLayer != nil {
				decision, decisionErr := l.DecisionLayer.Decide(ctx, cognition.DecisionRequest{
					State: input,
					Questions: cognition.DefaultDecisionQuestions(),
				})
				if decisionErr != nil {
					return l.interrupt(ctx, state, usage, decisionErr)
				}
				input.Signals = decision.Signals
				state.Metadata = withDecisionSummary(state.Metadata, decision, step)
				l.emit("decision.complete", sessionID, map[string]any{
					"step": step,
					"signals": decision.Signals,
					"results": len(decision.Results),
					"errors": decision.Errors,
				})
			}
			plan := router.Route(input)
			cognitivePlan = &plan
			state.Metadata = withCognition(state.Metadata, cognitiveProgress, plan, step)
			l.emit("cognition.route", sessionID, map[string]any{
				"step": step,
				"category": plan.Category,
				"think": plan.Think,
				"effort": plan.Effort,
				"thinkScore": plan.ThinkScore,
				"retrieval": plan.Retrieval,
				"reasons": plan.Reasons,
			})
		}
		messages, err := l.buildMessages(ctx, sessionID, opts, wf, cognitivePlan, workUnits)
		if err != nil {
			return Result{}, err
		}
		reasoningEffort := ""
		if cognitivePlan != nil {
			reasoningEffort = string(cognition.EffortNone)
			if cognitivePlan.Think {
				reasoningEffort = string(cognitivePlan.Effort)
			}
		}
		category := "general"
		if cognitivePlan != nil && strings.TrimSpace(cognitivePlan.Category) != "" {
			category = cognitivePlan.Category
		}
		requestMaxTokens := opts.MaxTokens
		if cognitivePlan != nil && cognitivePlan.Think {
			requestMaxTokens = adjustedMaxTokens(opts.MaxTokens, cognitivePlan.Effort)
		}
		usage.Requests++
		attempt, err := l.completeWithProviderChain(
			ctx,
			sessionID,
			step,
			category,
			protocol.ProviderRequest{
				Messages: messages, Tools: specs, ToolChoice: "auto", Temperature: opts.Temperature,
				MaxTokens: requestMaxTokens, ReasoningEffort: reasoningEffort,
			},
			len(messages),
			specs,
		)
		if err != nil {
			return l.interrupt(ctx, state, usage, err)
		}
		response := attempt.Response
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
		record := stepRecord{
			Step: step,
			FinishReason: response.FinishReason,
			Content: assistant.Content,
			Provider: attempt.Binding.Name,
			Model: attempt.Binding.Provider.Model(),
		}
		if activeWorkUnit != nil {
			record.WorkUnit = map[string]any{
				"id": activeWorkUnit.ID,
				"goal": activeWorkUnit.Goal,
				"status": activeWorkUnit.Status,
				"risk": activeWorkUnit.Risk,
			}
		}
		if cognitivePlan != nil {
			record.Cognition = *cognitivePlan
		}

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
				if workUnits != nil {
					state.Metadata = withWorkUnits(state.Metadata, workUnits)
				}
				_ = l.Store.Update(ctx, sessionID, SessionPatch{Metadata: state.Metadata, Usage: &usage})
				l.emit("workflow.blocked_final", sessionID, map[string]any{"step": step, "reason": reason})
				continue
			}
			if workUnits != nil && len(workUnits.Incomplete()) > 0 {
				remaining := workUnits.Incomplete()
				if err := l.Store.AppendStep(ctx, sessionID, step, record); err != nil {
					return Result{}, err
				}
				correction := protocol.Message{Role: "user", Content: workUnitCompletionCorrection(remaining)}
				if _, err := l.Store.AppendMessage(ctx, sessionID, correction); err != nil {
					return Result{}, err
				}
				state.Metadata = withWorkUnits(state.Metadata, workUnits)
				_ = l.Store.Update(ctx, sessionID, SessionPatch{Metadata: state.Metadata, Usage: &usage})
				l.emit("work_unit.blocked_final", sessionID, map[string]any{"step": step, "remaining": remaining})
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
				if workUnits != nil && isWorkUnitTool(call.Name) {
					toolResult = executeWorkUnitTool(workUnits, call.Name, args)
					state.Metadata = withWorkUnits(state.Metadata, workUnits)
				} else {
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
			}
			l.emit("tool.end", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "ok": toolResult.OK, "denied": toolResult.Denied})
			if opts.CognitionEnabled {
				cognitiveProgress.ObserveTool(call.Name, toolResult.OK, toolResult.Content)
				state.Metadata = withCognitionProgress(state.Metadata, cognitiveProgress)
			}
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
		if workUnits != nil {
			state.Metadata = withWorkUnits(state.Metadata, workUnits)
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

func (l *Loop) buildMessages(ctx context.Context, sessionID string, opts Options, wf *workflow.Runtime, cognitivePlan *cognition.Plan, workUnits *cognition.WorkUnitManager) ([]protocol.Message, error) {
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
	if cognitivePlan != nil {
		messages = append(messages, protocol.Message{Role: "system", Content: cognitivePrompt(*cognitivePlan)})
	}
	if workUnits != nil {
		messages = append(messages, protocol.Message{Role: "system", Content: workUnitPrompt(workUnits)})
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


func restoreCognitiveProgress(metadata map[string]any) cognition.Progress {
	if metadata == nil {
		return cognition.Progress{}
	}
	rawValue, ok := metadata["cognition"]
	if !ok {
		return cognition.Progress{}
	}
	raw, err := json.Marshal(rawValue)
	if err != nil {
		return cognition.Progress{}
	}
	var envelope struct {
		Progress cognition.Progress `json:"progress"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return cognition.Progress{}
	}
	return envelope.Progress
}

func withCognition(metadata map[string]any, progress cognition.Progress, plan cognition.Plan, step int64) map[string]any {
	out := cloneMetadata(metadata)
	envelope := map[string]any{}
	if current, ok := out["cognition"].(map[string]any); ok {
		for key, value := range current {
			envelope[key] = value
		}
	}
	history, _ := envelope["history"].([]any)
	history = append(history, map[string]any{
		"step": step,
		"category": plan.Category,
		"think": plan.Think,
		"effort": plan.Effort,
		"thinkScore": plan.ThinkScore,
		"retrieval": plan.Retrieval,
		"reasons": plan.Reasons,
	})
	if len(history) > 64 {
		history = history[len(history)-64:]
	}
	envelope["history"] = history
	envelope["progress"] = progress
	out["cognition"] = envelope
	return out
}

func withCognitionProgress(metadata map[string]any, progress cognition.Progress) map[string]any {
	out := cloneMetadata(metadata)
	envelope := map[string]any{}
	if current, ok := out["cognition"].(map[string]any); ok {
		for key, value := range current {
			envelope[key] = value
		}
	}
	envelope["progress"] = progress
	out["cognition"] = envelope
	return out
}

func cloneMetadata(metadata map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range metadata {
		out[key] = value
	}
	return out
}

func cognitivePrompt(plan cognition.Plan) string {
	if !plan.Think {
		return fmt.Sprintf(
			"Cognitive policy: category=%s; deliberate Think is not required. Stay focused and verify the next concrete action.",
			plan.Category,
		)
	}
	return fmt.Sprintf(
		"Cognitive policy: category=%s; Think is active; reasoning effort=%s. Deliberate before acting, identify missing evidence, and verify the chosen path.",
		plan.Category, plan.Effort,
	)
}


func restoreWorkUnits(metadata map[string]any, supplied []byte) (*cognition.WorkUnitManager, error) {
	if metadata != nil {
		if rawValue, ok := metadata["workUnits"]; ok {
			raw, err := json.Marshal(rawValue)
			if err != nil {
				return nil, err
			}
			var state cognition.WorkUnitState
			if err := json.Unmarshal(raw, &state); err != nil {
				return nil, err
			}
			return cognition.NewWorkUnitManager(&state), nil
		}
	}
	if len(supplied) == 0 {
		return nil, nil
	}
	units, err := cognition.ParseWorkUnits(supplied)
	if err != nil {
		return nil, err
	}
	manager := cognition.NewWorkUnitManager(nil)
	if err := manager.Seed(units); err != nil {
		return nil, err
	}
	return manager, nil
}

func withWorkUnits(metadata map[string]any, manager *cognition.WorkUnitManager) map[string]any {
	out := cloneMetadata(metadata)
	if manager != nil {
		out["workUnits"] = manager.Snapshot()
	}
	return out
}

func currentWorkUnit(manager *cognition.WorkUnitManager) (*cognition.WorkUnit, error) {
	if manager == nil {
		return nil, nil
	}
	return manager.EnsureActive()
}

func workUnitToolSpecs() []protocol.ToolSpec {
	return []protocol.ToolSpec{
		{
			Name: "work_unit_list",
			Description: "Inspect persistent Work Units for this Agent session.",
			Permission: "read",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false},
		},
		{
			Name: "work_unit_create",
			Description: "Create a persistent Work Unit. Work Units cannot choose providers, models, Categories or reasoning effort.",
			Permission: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
					"goal": map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"risk": map[string]any{"type": "string", "enum": []string{"low", "medium", "high", "critical"}},
					"required_evidence": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"verification": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"depends_on": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
				"required": []string{"goal"},
				"additionalProperties": false,
			},
		},
		{
			Name: "work_unit_update",
			Description: "Update Work Unit status, evidence and verification. Completion is gated by required evidence and passed checks.",
			Permission: "read",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "string"},
					"status": map[string]any{"type": "string", "enum": []string{"pending", "active", "blocked", "verifying", "completed", "failed", "cancelled"}},
					"summary": map[string]any{"type": "string"},
					"evidence": map[string]any{"type": "array"},
					"verification_results": map[string]any{"type": "array"},
				},
				"required": []string{"id"},
				"additionalProperties": false,
			},
		},
	}
}

func filterWorkUnitSpecs(allow []string) []protocol.ToolSpec {
	specs := workUnitToolSpecs()
	if allow == nil {
		return specs
	}
	allowed := map[string]bool{}
	for _, name := range allow {
		allowed[name] = true
	}
	out := make([]protocol.ToolSpec, 0, len(specs))
	for _, spec := range specs {
		if allowed[spec.Name] {
			out = append(out, spec)
		}
	}
	return out
}

func isWorkUnitTool(name string) bool {
	return name == "work_unit_list" || name == "work_unit_create" || name == "work_unit_update"
}

func executeWorkUnitTool(manager *cognition.WorkUnitManager, name string, args map[string]any) protocol.ToolResult {
	var payload any
	var err error

	switch name {
	case "work_unit_list":
		active, activeErr := manager.EnsureActive()
		if activeErr != nil {
			err = activeErr
			break
		}
		payload = map[string]any{"active": active, "units": manager.List()}
	case "work_unit_create":
		raw, marshalErr := json.Marshal([]any{args})
		if marshalErr != nil {
			err = marshalErr
			break
		}
		units, parseErr := cognition.ParseWorkUnits(raw)
		if parseErr != nil || len(units) != 1 {
			err = parseErr
			if err == nil {
				err = errors.New("invalid work unit")
			}
			break
		}
		var unit cognition.WorkUnit
		unit, err = manager.Add(units[0])
		if err == nil {
			payload = map[string]any{"unit": unit, "active": manager.Current()}
		}
	case "work_unit_update":
		id := strings.TrimSpace(fmt.Sprint(args["id"]))
		if id == "" {
			err = errors.New("work_unit_update requires id")
			break
		}
		patch := cognition.WorkUnitPatch{}
		if value, ok := args["status"]; ok {
			status := cognition.WorkUnitStatus(strings.TrimSpace(fmt.Sprint(value)))
			patch.Status = &status
		}
		if value, ok := args["summary"]; ok {
			summary := fmt.Sprint(value)
			patch.Summary = &summary
		}
		if value, ok := args["evidence"]; ok {
			raw, _ := json.Marshal(value)
			var items []cognition.EvidenceRef
			if decodeErr := json.Unmarshal(raw, &items); decodeErr != nil {
				err = decodeErr
				break
			}
			patch.Evidence = &items
		}
		if value, ok := args["verification_results"]; ok {
			raw, _ := json.Marshal(value)
			var items []cognition.VerificationResult
			if decodeErr := json.Unmarshal(raw, &items); decodeErr != nil {
				err = decodeErr
				break
			}
			patch.VerificationResults = &items
		}
		var unit cognition.WorkUnit
		unit, err = manager.Update(id, patch)
		if err == nil {
			active, _ := manager.EnsureActive()
			payload = map[string]any{"unit": unit, "active": active, "remaining": manager.Incomplete()}
		}
	default:
		err = fmt.Errorf("unknown Work Unit tool: %s", name)
	}

	if err != nil {
		raw, _ := json.Marshal(map[string]any{"error": err.Error()})
		return protocol.ToolResult{OK: false, Content: string(raw)}
	}
	raw, _ := json.Marshal(payload)
	return protocol.ToolResult{OK: true, Content: string(raw)}
}

func workUnitPrompt(manager *cognition.WorkUnitManager) string {
	active, _ := manager.EnsureActive()
	var b strings.Builder
	b.WriteString("Persistent Work Units are active. They constrain goals/evidence/verification and never choose providers/models/Categories.\n")
	if active != nil {
		fmt.Fprintf(&b, "Current Work Unit: %s — %s\n", active.ID, active.Goal)
	}
	for _, unit := range manager.List() {
		fmt.Fprintf(&b, "- %s [%s/%s] %s\n", unit.ID, unit.Status, unit.Risk, unit.Goal)
	}
	b.WriteString("Use work_unit_update to record evidence/verification and complete the active unit before final completion.")
	return b.String()
}

func workUnitCompletionCorrection(units []cognition.WorkUnit) string {
	var parts []string
	for _, unit := range units {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", unit.ID, unit.Status, unit.Goal))
	}
	return "Work Unit completion gate rejected final completion. Remaining units: " + strings.Join(parts, "; ") + ". Use work_unit_update to satisfy evidence/verification and complete them before finishing."
}


func withDecisionSummary(metadata map[string]any, summary cognition.DecisionSummary, step int64) map[string]any {
	out := cloneMetadata(metadata)
	envelope := map[string]any{}
	if current, ok := out["cognition"].(map[string]any); ok {
		for key, value := range current {
			envelope[key] = value
		}
	}
	providers := make([]map[string]any, 0, len(summary.Results))
	for _, result := range summary.Results {
		providers = append(providers, map[string]any{
			"source": result.Source,
			"model": result.Model,
		})
	}
	envelope["lastDecision"] = map[string]any{
		"step": step,
		"signals": summary.Signals,
		"providers": providers,
		"errors": summary.Errors,
	}
	out["cognition"] = envelope
	return out
}


func adjustedMaxTokens(base int, effort cognition.Effort) int {
	if base <= 0 {
		return base
	}
	multiplier := 1.0
	switch effort {
	case cognition.EffortMedium:
		multiplier = 1.15
	case cognition.EffortHigh:
		multiplier = 1.5
	case cognition.EffortMax:
		multiplier = 2
	}
	return int(math.Ceil(float64(base) * multiplier))
}
