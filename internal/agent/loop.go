package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

// The two run purposes (ADR-0006 D3). A task run works until its outcome is achieved and verified; a
// conversation turn answers, and its end is not the end of the task.
const (
	taskPrompt = `You are LumenCortex Agent, an autonomous coding agent.
Inspect before editing. Use tools until the requested outcome is achieved and verified. Keep changes scoped. Do not claim completion without evidence. When a Workflow Contract is active, obey its current action, tool boundary, outcomes, routes, and gates.`
	conversationPrompt = `You are LumenCortex Agent, a coding agent in a conversation with the user.
Answer questions and discuss plans directly; use tools when they make an answer accurate. Change the workspace only when the user asks for a change, inspect before editing, and keep changes scoped. Your reply ends this turn, not the task: open work units and required checks stay open until evidence or the user resolves them, so say what is left instead of claiming it is done. Do not claim completion without evidence. When a Workflow Contract is active, obey its current action, tool boundary, outcomes, routes, and gates; the turn ends only when the workflow can finish.`
)

// SystemPrompt is a run's default system prompt: its purpose, a conversation turn or a task, followed by
// authority, the host's statement of what the run may do ("" for none). A host prompt replaces it.
func SystemPrompt(interactive bool, authority string) string {
	prompt := taskPrompt
	if interactive {
		prompt = conversationPrompt
	}
	if authority = strings.TrimSpace(authority); authority != "" {
		prompt += "\n" + authority
	}
	return prompt
}

type Loop struct {
	// Models holds the catalog models the run may use, keyed by reference: the session model and
	// every failover reference that has tool calling, each with the client built for it.
	Models map[config.ModelRef]Model
	// Health is the Engine's registry; nil gets a private one.
	Health *Health
	Store  Store
	Tools  Tools
	Emit   func(Event)
	// Evidence returns what the evidence rule reads beyond the run's own state: whether the session
	// has work pending application (its owned worktree is dirty or holds commits not yet applied),
	// the required checks and whether each is satisfied at the current fingerprint, and the workspace
	// refs a resolution would rest on. The loop fills in the workflow's verdicts. nil means nothing
	// pending, no checks and no refs.
	Evidence func(context.Context) (taskstate.Evidence, error)
	// Checks decides configured checks: the completion gate's required checks and a completing unit's
	// checks (design §4.7). nil means no checks are configured.
	Checks Checks
}

// run is the Task State a run works on: the run's current state, the workflow and work units decoded
// from it, and the JSON of the version the store holds. ts.Version is always that stored version; a
// change not yet written is the difference between ts and saved.
//
// It also holds the window's source, see transcript.go.
type run struct {
	ts    taskstate.State
	wf    *workflow.Runtime
	units *taskstate.WorkUnitManager
	saved json.RawMessage
	// mutated: a tool call of this run changed the workspace successfully.
	mutated bool
	runTranscript
}

// change returns the write that stores the run's unsaved Task State, or nil when nothing changed.
func (r *run) change(reason string) (*TaskStateChange, error) {
	raw, err := r.ts.Encode()
	if err != nil || bytes.Equal(raw, r.saved) {
		return nil, err
	}
	return &TaskStateChange{Reason: reason, Next: r.ts}, nil
}

// stored records that change was written as version.
func (r *run) stored(change *TaskStateChange, version int64) {
	raw, _ := change.Next.Encode()
	r.saved = raw
	r.ts.Version = version
}

func (l *Loop) Run(ctx context.Context, sessionID string, opts Options) (result Result, err error) {
	if len(l.Models) == 0 || l.Store == nil || l.Tools == nil {
		return Result{}, errors.New("models, store, and tools are required")
	}
	if l.Health == nil {
		l.Health = NewHealth(nil)
	}
	if opts.MaxSteps <= 0 {
		opts.MaxSteps = 24
	}
	if opts.MaxToolCallsPerStep <= 0 {
		opts.MaxToolCallsPerStep = 8
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = SystemPrompt(opts.Interactive, "")
	}

	var (
		state = SessionState{ID: sessionID}
		usage protocol.Usage
	)
	// The host's Start transaction set the session to running before Run is called,
	// so from here on every return goes through finish: the run owns one ExitCause.
	defer func() {
		if recovered := recover(); recovered != nil {
			result, err = l.panicExit(ctx, true, state, usage, recovered)
		}
	}()

	state, err = l.Store.Get(ctx, sessionID)
	if err != nil {
		return l.exit(ctx, SessionState{ID: sessionID}, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
	}
	if state.Status != StatusRunning {
		// Nothing was started, so there is no exit to record and nothing to overwrite.
		return Result{}, fmt.Errorf("session %s is %q: Run needs a Start that has set it to running", sessionID, state.Status)
	}
	usage = state.Usage
	ts, err := l.Store.TaskState(ctx, sessionID)
	if err != nil {
		return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
	}
	wf, err := ts.RestoreWorkflow()
	if err != nil {
		return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindConfig, Err: err})
	}
	saved, err := ts.Encode()
	if err != nil {
		return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
	}
	r := &run{ts: ts, wf: wf, units: ts.Units(), saved: saved}
	if err := r.load(ctx, l.Store, sessionID); err != nil {
		return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
	}
	startStep, err := l.Store.NextStep(ctx, sessionID)
	if err != nil {
		return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
	}
	if opts.Resumed {
		l.checkpoint(sessionID, "agent.resume", map[string]any{
			"nextStep": startStep,
		})
	}

	for turn := 0; turn < opts.MaxSteps; turn++ {
		if err := ctx.Err(); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
		}
		step := startStep + int64(turn)
		// Host edits queued since the last step become visible before this step's request is built.
		if err := l.applyInbox(ctx, sessionID, r); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		if r.wf != nil && len(r.wf.WaitingHumanGates()) > 0 {
			gates := r.wf.WaitingHumanGates()
			cause := ExitCause{Status: StatusWaitingGate, Kind: KindWorkflowGate, Err: errors.New("workflow human gate is pending")}
			if err := l.finish(ctx, state, usage, cause, nil); err != nil {
				return finishFailure(state, usage, cause, err)
			}
			l.checkpoint(sessionID, "agent.safe", map[string]any{
				"phase": "waiting_gate",
				"step":  step,
				"gates": gates,
			})
			l.emit("workflow.gate_waiting", sessionID, map[string]any{"step": step, "gates": gates})
			return Result{SessionID: sessionID, Status: StatusWaitingGate, Usage: usage, WaitingGate: true, Workflow: r.wf.Summary()}, nil
		}

		allowlist := opts.ToolAllowlist
		if r.wf != nil {
			allowlist = r.wf.EffectiveAllowlist(allowlist)
		}
		specs := l.Tools.Specs(allowlist)
		specs = append(specs, filterTaskToolSpecs(allowlist)...)
		specs = filterDeniedTools(specs, opts.ToolDenylist)
		activeWorkUnit, _ := r.units.EnsureActive()
		r.ts.SetUnits(r.units)
		// The request window (design §3.3 steps 4-6, §5): System, the Task State block and the newest
		// whole groups of the transcript that fit the budget of the candidate being asked.
		groups, err := r.groups(l.maxContext())
		if err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindTranscriptUnpaired, Err: err})
		}
		in := window.Input{
			System:     systemMessages(opts),
			State:      protocol.Message{Role: "user", Content: taskstate.Render(state.Goal, r.ts, r.wf)},
			Transcript: r.transcript,
			ToolTokens: window.Estimate(specs),
		}
		fixed := window.Fixed(in)
		base := fixed // §3.3 step 4: the newest group counts without echo
		if len(groups) > 0 {
			for _, e := range groups[len(groups)-1] {
				base += window.Estimate(e.Msg)
			}
		}
		cands, candidatesDecision := l.candidates(opts, base)
		stepDecisions := append(append([]Decision(nil), opts.Decisions...), candidatesDecision)
		if len(cands) == 0 {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindWindowOverflow,
				Err: fmt.Errorf("no model can take this request: it needs about %d tokens before the reply, and no candidate of %s has the context, tool calling and health for it", base, refList(append([]config.ModelRef{opts.Session}, opts.Failover...)))})
		}
		stall := Stall{SameFailure: r.ts.Progress.SameFailureCount, NoProgressSteps: r.ts.Progress.NoProgressSteps}
		answered, cause := l.send(ctx, sessionID, step, opts, stall, r.ts.Version, cands, r, in, len(groups), specs, &usage, &stepDecisions)
		if cause != nil {
			return l.exit(ctx, state, usage, *cause)
		}
		response := answered.Response
		addUsage(&usage, response.Usage)
		assistant := response.Message
		assistant.Role = "assistant"
		calls := assistant.ToolCalls
		if len(calls) > opts.MaxToolCallsPerStep {
			calls = calls[:opts.MaxToolCallsPerStep]
			l.emit("tools.deferred", sessionID, map[string]any{"step": step, "requested": len(response.Message.ToolCalls), "executing": len(calls)})
		}
		calls = preserveToolArguments(calls)
		assistant.ToolCalls = calls
		seq, _, err := l.Store.AppendMessage(ctx, sessionID, assistant, nil)
		if err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		r.appendAssistant(seq, assistant, answered.Model.Ref, response.Echo)
		record := stepRecord{
			Step:         step,
			FinishReason: response.FinishReason,
			Content:      assistant.Content,
			Provider:     answered.Model.ProviderID,
			Model:        answered.Model.ModelID,
			ModelRef:     answered.Model.Ref,
			Decisions:    stepDecisions,
			Window:       &windowSummary{FirstSeq: answered.Window.FirstSeq, Dropped: answered.Window.Dropped + r.trimmed, Tokens: answered.Window.Tokens},
		}
		if activeWorkUnit != nil {
			record.WorkUnit = map[string]any{
				"id":     activeWorkUnit.ID,
				"goal":   activeWorkUnit.Goal,
				"status": activeWorkUnit.Status,
				"risk":   activeWorkUnit.Risk,
			}
		}

		if len(calls) == 0 {
			if strings.TrimSpace(assistant.Content) == "" {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindEmptyResponse, Err: errors.New("model returned neither tool calls nor final content")})
			}
			var correction string
			switch {
			case r.wf != nil && !r.wf.CanFinish():
				reason := r.wf.BlockReason()
				correction = "Workflow contract rejected completion: " + reason + ". Continue the current action and obtain the required evidence or gate approval."
				l.emit("workflow.blocked_final", sessionID, map[string]any{"step": step, "reason": reason})
			case opts.Interactive && r.wf == nil:
				// A conversation turn without a host workflow ends with its answer (ADR-0006 D3): the run
				// completed, the task did not. Open work units and unpassed required checks stay
				// obligations that keep the thread open, and no correction is appended. A turn that changed
				// the workspace still has its required checks decided, so the evidence rule below sees
				// results for the new state; a question-only turn runs none and latches only on results
				// already stored.
				if r.mutated {
					if _, cause := l.requiredChecks(ctx); cause != nil {
						return l.exit(ctx, state, usage, *cause)
					}
				}
			case len(r.units.Incomplete()) > 0:
				remaining := r.units.Incomplete()
				correction = workUnitCompletionCorrection(remaining)
				l.emit("work_unit.blocked_final", sessionID, map[string]any{"step": step, "remaining": remaining})
			default:
				// The completion gate's last condition: every required check decided passed (§4.7).
				failed, cause := l.requiredChecks(ctx)
				if cause != nil {
					return l.exit(ctx, state, usage, *cause)
				}
				if len(failed) > 0 {
					correction = checksCorrection(failed)
					l.emit("checks.blocked_final", sessionID, map[string]any{"step": step, "checks": verdictViews(failed)})
				}
			}
			if correction == "" {
				l.latchEvidence(ctx, sessionID, r)
			}
			if err := l.flush(ctx, sessionID, r, taskstate.ReasonStep); err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
			}
			record.Workflow = workflowSummary(r.wf)
			record.TaskState = r.ts.Version
			if err := l.recordStep(ctx, sessionID, step, record, &opts); err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
			}
			if correction != "" {
				blocked := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked, Content: correction}
				seq, _, err := l.Store.AppendMessage(ctx, sessionID, blocked, nil)
				if err != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
				}
				r.appendRow(seq, blocked)
				if err := l.Store.Update(ctx, sessionID, SessionPatch{Usage: &usage}); err != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
				}
				continue
			}
			final := assistant.Content
			cause := ExitCause{Status: StatusCompleted}
			if err := l.finish(ctx, state, usage, cause, &final); err != nil {
				return finishFailure(state, usage, cause, err)
			}
			l.emit("session.complete", sessionID, map[string]any{"step": step})
			return Result{SessionID: sessionID, Status: StatusCompleted, Final: final, Usage: usage, Workflow: workflowSummary(r.wf)}, nil
		}

		for _, call := range calls {
			args, argsErr := parseToolArguments(call.Arguments)
			if argsErr != nil {
				rejected := protocol.ToolResult{OK: false, Content: toolErrorContent("invalid tool arguments: " + argsErr.Error())}
				if _, err := l.persistToolResult(ctx, sessionID, call, rejected, nil, r); err != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
				}
				continue
			}
			denied := !containsTool(specs, call.Name) || (r.wf != nil && !r.wf.IsToolAllowed(call.Name))
			var toolResult protocol.ToolResult
			if denied {
				toolResult = protocol.ToolResult{OK: false, Denied: true, Permission: "workflow", Content: "tool is not allowed in the current working set"}
			} else {
				l.emit("tool.start", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "args": args})
				if isTaskTool(call.Name) {
					var cause *ExitCause
					toolResult, cause = executeTaskTool(ctx, l.Checks, r, call.Name, args, time.Now().UTC())
					if cause != nil {
						// Deciding a unit's checks was cancelled or failed: the call stays
						// unanswered and the run ends.
						return l.exit(ctx, state, usage, *cause)
					}
				} else {
					toolResult, err = l.Tools.Execute(ctx, call.Name, args, func(output protocol.ToolOutput) {
						l.emit("tool.output", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "stream": output.Stream, "chunk": output.Chunk})
					})
					if ctx.Err() != nil {
						// A tool stopped by cancellation often returns a result, not an error. Its
						// output is not saved: the run ends interrupted/cancelled and the call
						// stays unanswered.
						return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: ctx.Err()})
					}
					if err != nil {
						toolResult = protocol.ToolResult{OK: false, Content: fmt.Sprintf(`{"error":%q}`, err.Error())}
					}
					if toolResult.OK && toolResult.MutatesWorkspace {
						// A successful change to the workspace is a new obligation.
						r.ts.Reopen(time.Now().UTC())
						r.mutated = true
					}
				}
			}
			l.emit("tool.end", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "ok": toolResult.OK, "denied": toolResult.Denied})
			r.ts.Progress.ObserveTool(call.Name, toolResult.OK, toolResult.Content)
			if r.wf != nil {
				update, wfErr := r.wf.ObserveTool(workflow.ToolObservation{Tool: call.Name, Args: args, Result: map[string]any{"ok": toolResult.OK, "denied": toolResult.Denied, "permission": toolResult.Permission, "content": toolResult.Content}, Step: step})
				if wfErr != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindConfig, Err: wfErr})
				}
				if len(update.ChangedFacts) > 0 {
					l.emit("workflow.facts", sessionID, map[string]any{"step": step, "facts": update.ChangedFacts})
				}
				if update.Transition != nil {
					l.emit("workflow.transition", sessionID, map[string]any{"step": step, "from": update.Transition.From, "to": update.Transition.To, "reason": update.Transition.Reason})
				}
				r.ts.SetWorkflow(r.wf)
			}
			// The Task State change this result caused commits with its row (design §4.4).
			change, err := r.change(taskstate.ReasonToolPrefix + call.Name)
			if err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
			}
			messageSeq, err := l.persistToolResult(ctx, sessionID, call, toolResult, change, r)
			if err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
			}
			l.checkpoint(sessionID, "agent.safe", map[string]any{
				"phase":      "tool_result",
				"step":       step,
				"toolCallId": call.ID,
				"tool":       call.Name,
				"messageSeq": messageSeq,
				"ok":         toolResult.OK,
				"denied":     toolResult.Denied,
			})
			record.ToolCalls = append(record.ToolCalls, toolCallRecord{ID: call.ID, Name: call.Name, Args: args, OK: toolResult.OK, Denied: toolResult.Denied})
		}
		if err := l.flush(ctx, sessionID, r, taskstate.ReasonStep); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		record.Workflow = workflowSummary(r.wf)
		record.TaskState = r.ts.Version
		if err := l.recordStep(ctx, sessionID, step, record, &opts); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		if err := l.Store.Update(ctx, sessionID, SessionPatch{Usage: &usage}); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		l.checkpoint(sessionID, "agent.safe", map[string]any{
			"phase":    "step_complete",
			"step":     step,
			"nextStep": step + 1,
		})
	}
	return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindMaxSteps, Err: fmt.Errorf("agent reached max steps: %d", opts.MaxSteps)})
}

// recordStep appends the step record and, once the first one of the run is written, drops the
// Start's decisions from opts: only the first step record carries them.
func (l *Loop) recordStep(ctx context.Context, sessionID string, step int64, record stepRecord, opts *Options) error {
	if err := l.Store.AppendStep(ctx, sessionID, step, record); err != nil {
		return err
	}
	opts.Decisions = nil
	return nil
}

// applyInbox applies the host edits queued since the last read (ADR-0006 D1′): one Task State version
// with reason "user" that also consumes them. Edits never touch the workflow or work units.
func (l *Loop) applyInbox(ctx context.Context, sessionID string, r *run) error {
	edits, err := l.Store.InboxEdits(ctx, sessionID)
	if err != nil || len(edits) == 0 {
		return err
	}
	r.ts.Apply(edits)
	change := &TaskStateChange{Reason: taskstate.ReasonUser, Next: r.ts, InboxThrough: edits[len(edits)-1].Seq}
	if err := l.putTaskState(ctx, sessionID, r, change); err != nil {
		return err
	}
	l.emit("task_state.edits_applied", sessionID, map[string]any{"version": r.ts.Version, "edits": len(edits)})
	return nil
}

// latchEvidence applies the evidence rule to the run's state when a run completes (ADR-0006 D2): an
// open thread with no obligation left and positive acceptance evidence becomes done. Evidence that
// cannot be read latches nothing. The caller writes the change.
func (l *Loop) latchEvidence(ctx context.Context, sessionID string, r *run) {
	ev := taskstate.Evidence{}
	if l.Evidence != nil {
		var err error
		if ev, err = l.Evidence(ctx); err != nil {
			l.emit("task_state.evidence_unavailable", sessionID, map[string]any{"error": err.Error()})
			return
		}
	}
	if r.wf != nil {
		ev.Workflow, ev.WorkflowCanFinish, ev.GatePending = true, r.wf.CanFinish(), len(r.wf.PendingGates()) > 0
	}
	if r.ts.AutoDone(ev, time.Now().UTC()) {
		l.emit("task_state.resolved", sessionID, map[string]any{"state": r.ts.Resolution.State, "by": r.ts.Resolution.By})
	}
}

// requiredChecks decides the configured required checks for the completion gate and returns those
// that did not pass, or the exit when deciding was cancelled or failed.
func (l *Loop) requiredChecks(ctx context.Context) ([]evaluator.Verdict, *ExitCause) {
	if l.Checks == nil {
		return nil, nil
	}
	_, required := l.Checks.Configured()
	if len(required) == 0 {
		return nil, nil
	}
	verdicts, err := l.Checks.Decide(ctx, required, "")
	if err != nil {
		cause := checkFailureExit(ctx, err)
		return nil, &cause
	}
	return failing(verdicts), nil
}

// flush writes the run's unsaved Task State on its own, if any.
func (l *Loop) flush(ctx context.Context, sessionID string, r *run, reason string) error {
	change, err := r.change(reason)
	if err != nil || change == nil {
		return err
	}
	return l.putTaskState(ctx, sessionID, r, change)
}

// putTaskState writes change on its own; the run's state then carries the stored version.
func (l *Loop) putTaskState(ctx context.Context, sessionID string, r *run, change *TaskStateChange) error {
	version, err := l.Store.PutTaskState(ctx, sessionID, *change)
	if err != nil {
		return err
	}
	r.stored(change, version)
	l.emit("task_state.changed", sessionID, map[string]any{"version": version, "reason": change.Reason})
	return nil
}

// systemMessages is the request's fixed system prefix.
func systemMessages(opts Options) []protocol.Message {
	system := []protocol.Message{{Role: "system", Content: opts.SystemPrompt}}
	if strings.TrimSpace(opts.AdditionalSystemPrompt) != "" {
		system = append(system, protocol.Message{Role: "system", Content: opts.AdditionalSystemPrompt})
	}
	return system
}

// persistToolResult appends a tool row and, when change is non-nil, the Task State version it caused,
// in one transaction; the run's state then carries the stored version.
func (l *Loop) persistToolResult(ctx context.Context, sessionID string, call protocol.ToolCall, result protocol.ToolResult, change *TaskStateChange, r *run) (int64, error) {
	row := protocol.Message{Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: result.Content}
	seq, version, err := l.Store.AppendMessage(ctx, sessionID, row, change)
	if err != nil {
		return 0, err
	}
	r.appendRow(seq, row)
	if change != nil {
		r.stored(change, version)
		l.emit("task_state.changed", sessionID, map[string]any{"version": version, "reason": change.Reason})
	}
	return seq, nil
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

// finishTimeout bounds the terminal write, which runs without the run's cancellation.
const finishTimeout = 5 * time.Second

// finish persists the terminal status, ExitCause, final text and usage in one
// store update. It ignores the run's cancellation (a cancelled run must still be
// able to record that it was cancelled) and is bounded by its own timeout.
func (l *Loop) finish(ctx context.Context, state SessionState, usage protocol.Usage, cause ExitCause, final *string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	status := cause.Status
	patch := SessionPatch{Status: &status, Usage: &usage}
	if cause.Status == StatusCompleted {
		patch.Final = final
		patch.ClearError = true
	} else {
		patch.Error = cause.errorData(time.Now())
	}
	return l.Store.Update(ctx, state.ID, patch)
}

// finishFailure reports a run whose terminal write failed. The row still says
// "running"; the next Start overwrites it. The cause is kept in the returned error.
func finishFailure(state SessionState, usage protocol.Usage, cause ExitCause, err error) (Result, error) {
	failure := fmt.Errorf("persist %s exit: %w", cause.Status, err)
	return Result{SessionID: state.ID, Status: state.Status, Usage: usage}, errors.Join(cause.returnErr(), failure)
}

// exit ends the run with an interrupted or failed cause: finish, then the
// terminal event. A storage or provider error that arrives with the run context
// already cancelled is the cancellation, not a failure.
func (l *Loop) exit(ctx context.Context, state SessionState, usage protocol.Usage, cause ExitCause) (Result, error) {
	if ctx.Err() != nil && (cause.Kind == KindStorage || strings.HasPrefix(cause.Kind, "provider:")) {
		cause = ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: ctx.Err()}
	}
	if err := l.finish(ctx, state, usage, cause, nil); err != nil {
		return finishFailure(state, usage, cause, err)
	}
	event := "session.interrupted"
	if cause.Status == StatusFailed {
		event = "session.failed"
	}
	l.emit(event, state.ID, map[string]any{"kind": cause.Kind, "error": cause.Err.Error()})
	return Result{SessionID: state.ID, Status: cause.Status, Usage: usage}, cause.Err
}

// panicExit turns a panic in the run into failed/panic. A panic before the
// session row became "running" has nothing to finalize and only becomes an error.
// Like every exit, the terminal event follows Finish; it also carries the stack.
func (l *Loop) panicExit(ctx context.Context, started bool, state SessionState, usage protocol.Usage, recovered any) (Result, error) {
	err := fmt.Errorf("panic: %v", recovered)
	if !started {
		return Result{}, err
	}
	stack := string(debug.Stack())
	cause := ExitCause{Status: StatusFailed, Kind: KindPanic, Err: err}
	if err := l.finish(ctx, state, usage, cause, nil); err != nil {
		return finishFailure(state, usage, cause, err)
	}
	l.emit("session.failed", state.ID, map[string]any{"kind": cause.Kind, "error": err.Error(), "stack": stack})
	return Result{SessionID: state.ID, Status: cause.Status, Usage: usage}, err
}

// preserveToolArguments stores arguments that are not valid JSON, or are a JSON
// string, as a JSON string holding the raw text, so the assistant row can be stored
// and a stored JSON string always means "the model's raw text": adapters send that
// text back unchanged. Other JSON is kept as is. It copies the slice before changing it.
func preserveToolArguments(calls []protocol.ToolCall) []protocol.ToolCall {
	var out []protocol.ToolCall
	for i, call := range calls {
		trimmed := bytes.TrimLeft(call.Arguments, " \t\r\n")
		if len(call.Arguments) == 0 || (json.Valid(call.Arguments) && trimmed[0] != '"') {
			continue
		}
		quoted, err := json.Marshal(string(call.Arguments))
		if err != nil {
			continue
		}
		if out == nil {
			out = append([]protocol.ToolCall(nil), calls...)
		}
		out[i].Arguments = quoted
	}
	if out == nil {
		return calls
	}
	return out
}

// parseToolArguments decodes tool call arguments, which must be a JSON object.
// Every other top-level value (null, string, number, boolean, array) is rejected,
// including the JSON string preserveToolArguments stores for raw text.
func parseToolArguments(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var args map[string]any
	// A null decodes into a nil map without an error.
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return map[string]any{}, errors.New("the arguments were not a JSON object; send one valid JSON object")
	}
	return args, nil
}

func toolErrorContent(message string) string {
	raw, _ := json.Marshal(map[string]string{"error": message})
	return string(raw)
}

func (l *Loop) emit(kind, sessionID string, data map[string]any) {
	if l.Emit != nil {
		l.Emit(Event{Type: kind, SessionID: sessionID, Data: data})
	}
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
