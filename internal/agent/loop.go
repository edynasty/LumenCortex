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
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/verify"
	"github.com/edynasty/LumenCortex/internal/window"
	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

// The two run purposes (ADR-0006 D3). A task run works until its outcome is achieved and verified; a
// conversation turn answers, and its end is not the end of the task.
const (
	taskPrompt = `You are LumenCortex Agent, an autonomous coding agent.
Inspect before editing. Use tools until the requested outcome is achieved and verified. Keep changes scoped. Do not claim completion without evidence. Record what you verified that later steps or later work will need, such as a root cause, a decision or an approach that failed, with note_write. When a Workflow Contract is active, obey its current action, tool boundary, outcomes, routes, and gates.
Working strategy:
- Name the deliverable the task asks for (files, behaviour, output) and produce a working version of it early, then improve it. Investigation that does not move the deliverable forward is cost.
- For a task of several steps, keep the plan, findings and a handoff in a notebook with notebook_update and keep it current. When a run continues earlier work, read the notebook's Handoff and the runtime rows before acting.
- Keep experiments cheap and bounded: a small probe before a long run, each with a reason.
- When the run has a deadline, spend the time left on a verified result and deliver it before the reserve rather than a better one after it.
- Run a command that may take long (a build, a test suite, a download, a server) with bash mode "background" where it is offered, keep working, and collect it with job_wait or job_status.
- Before the final answer, check the actual result: the files hold the intended content, the commands or tests that prove it pass, and no background job is still running. If part of the task is not done, say which part and why.`
	conversationPrompt = `You are LumenCortex Agent, a coding agent in a conversation with the user.
Answer questions and discuss plans directly; use tools when they make an answer accurate. Change the workspace only when the user asks for a change, inspect before editing, and keep changes scoped. Your reply ends this turn, not the task: open work units and required checks stay open until evidence or the user resolves them, so say what is left instead of claiming it is done. Do not claim completion without evidence. Record what you verified that later turns or threads will need, such as a root cause, a decision or an approach that failed, with note_write. When a Workflow Contract is active, obey its current action, tool boundary, outcomes, routes, and gates; the turn ends only when the workflow can finish.`
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
	// Tasks starts execution tasks for Options.Delegation isolated; nil disables that grant.
	Tasks TaskStarter
	// Cite returns the current git blob of each workspace-relative path for note_write (ADR-0010 D1),
	// refusing a path that is not a file of the run's workspace. nil refuses notes with cites.
	Cite func(ctx context.Context, paths []string) ([]taskstate.Cite, error)
	// Snapshots take the pre- and post-state of side-effect tools in the run's workspace (plan T1-S4).
	// nil records every such execution with its pre-state unavailable.
	Snapshots Snapshots
	// Jobs lists the run's active background jobs for the completion gate (ADR-0019 §3); nil means the
	// run has no jobs to wait for.
	Jobs RunJobs
	// Hooks are the host's versioned lifecycle hooks (spec §20); nil runs none.
	Hooks *verify.Registry
	// Verifier decides semantic verdicts in an independent context (spec §24); nil means no semantic
	// level is decided.
	Verifier verify.Verifier
	// Reviewer is the single, opt-in, read-only independent review (spec §22); nil disables it.
	Reviewer      verify.Reviewer
	DebugBoundary func(context.Context, DebugBoundary) error
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
	// spend is what the run used against opts.Budget (ADR-0019 §6).
	spend *Spend
	// reviewed: the single independent review of this run already ran.
	reviewed bool
	// attempts are this run's repair attempts. The durable record keeps them as history, but the
	// budget they count against is the run's own: a new turn starts with a fresh one.
	attempts []verify.Attempt
	// review and its fingerprint apply only to the workspace state that was reviewed.
	review            *verify.ReviewResult
	reviewFingerprint string
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
		opts.MaxSteps = config.DefaultMaxSteps
	}
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = SystemPrompt(opts.Interactive, "")
	}
	// A host list written before a tool was renamed keeps its meaning: a denylist naming "shell" denies
	// "bash" (ADR-0019 §0).
	opts.ToolAllowlist = protocol.CanonicalToolNames(opts.ToolAllowlist)
	opts.ToolDenylist = protocol.CanonicalToolNames(opts.ToolDenylist)
	// A run with a deadline ends at it: the context carries ErrRunDeadline as its cause, so exit can tell
	// it from the host's cancellation.
	clock := newRunClock(opts.Deadline, opts.DeadlineReserve, time.Now())
	if clock.set() {
		var cancelDeadline context.CancelFunc
		ctx, cancelDeadline = context.WithDeadlineCause(ctx, clock.deadline, ErrRunDeadline)
		defer cancelDeadline()
	}
	// The exit path names the run it ends; the id rides the context so every exit reports it.
	ctx = withRunID(ctx, opts.RunID)
	// A run's bounded hook denials are its own: a new turn gets its own limits.
	if l.Hooks != nil {
		l.Hooks.ResetDenials(sessionID)
	}

	var (
		state = SessionState{ID: sessionID}
		usage protocol.Usage
		spend = &Spend{}
	)
	// Registered before the panic recovery so it runs last: every return, a panic's included, reports
	// the run's spend.
	defer func() {
		result.Spend = *spend
		if spend.ModelRequests > 0 || spend.ToolCalls > 0 {
			l.emit("budget.spend", sessionID, spend.view(opts.Budget))
		}
	}()
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
	r := &run{ts: ts, wf: wf, units: ts.Units(), saved: saved, spend: spend}
	if err := r.load(ctx, l.Store, sessionID, opts.Withheld); err != nil {
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
	if clock.set() {
		notice := clock.startNotice(time.Now())
		seq, _, err := l.Store.AppendMessage(ctx, sessionID, notice, nil)
		if err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		r.appendRow(seq, notice)
	}

	for turn := 0; opts.MaxSteps < 0 || turn < opts.MaxSteps; turn++ {
		if err := ctx.Err(); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
		}
		step := startStep + int64(turn)
		// Host edits queued since the last step become visible before this step's request is built.
		if err := l.applyInbox(ctx, sessionID, r); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		if notice, ok := clock.reserveNotice(time.Now()); ok {
			seq, _, err := l.Store.AppendMessage(ctx, sessionID, notice, nil)
			if err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
			}
			r.appendRow(seq, notice)
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
		// No request is sent for a reply whose tool calls the budget could not execute (ADR-0019 §6).
		if cause := spend.admitTools(opts.Budget); cause != nil {
			return l.exit(ctx, state, usage, *cause)
		}

		allowlist := opts.ToolAllowlist
		if r.wf != nil {
			allowlist = r.wf.EffectiveAllowlist(allowlist)
		}
		specs := StepSpecs(l.Tools, StepToolsInput{
			Allowlist:  allowlist,
			Denylist:   opts.ToolDenylist,
			Delegation: opts.Delegation,
			Tasks:      l.Tasks != nil,
			TaskAgents: opts.TaskAgents,
			Claims:     l.verificationOffered(opts),
		})
		// The model profile presents the same catalog in its chosen form; authority is unchanged.
		specs = presentSpecs(specs, opts.Presentation.ToolDescriptions)
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
		sent := time.Now()
		answered, cause := l.send(ctx, sessionID, step, opts, r.ts.Version, cands, r, in, len(groups), specs, &usage, &stepDecisions)
		clock.observeModel(time.Since(sent), answered.Elapsed)
		if cause != nil {
			return l.exit(ctx, state, usage, *cause)
		}
		response := answered.Response
		addUsage(&usage, response.Usage)
		assistant := response.Message
		assistant.Role = "assistant"
		calls := assistant.ToolCalls
		if opts.MaxToolCallsPerStep > 0 && len(calls) > opts.MaxToolCallsPerStep {
			calls = calls[:opts.MaxToolCallsPerStep]
			l.emit("tools.deferred", sessionID, map[string]any{"step": step, "requested": len(response.Message.ToolCalls), "executing": len(calls)})
		}
		if room := spend.toolRoom(opts.Budget); room >= 0 && len(calls) > room {
			// The calls past the budget are not stored or run: the next step's admission ends the run.
			calls = calls[:room]
			l.emit("tools.deferred", sessionID, map[string]any{"step": step, "requested": len(response.Message.ToolCalls), "executing": len(calls), "reason": KindBudgetToolCalls})
		}
		calls = preserveToolArguments(calls)
		assistant.ToolCalls = calls
		seq, err := l.Store.AppendAnswered(ctx, sessionID, answered.RequestID, assistant)
		if err != nil {
			if errors.Is(err, session.ErrFenced) {
				// The store committed the envelope as unknown and kept the message out: nothing more
				// is sent or run for this session (R-1).
				return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindFenced, Err: err})
			}
			// The answer is not stored, so the request ends unknown, not answered. A failed
			// settlement is left to Reconcile.
			l.settle(sessionID, answered.RequestID, reasonAnswerNotSaved)
			return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
		}
		r.appendAssistant(seq, assistant, answered.Model.Ref, response.Echo)
		if err := l.debugBoundary(ctx, DebugBoundary{Event: "AfterModelCall", Step: step}); err != nil {
			return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
		}
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
			cut := response.FinishReason == protocol.FinishLength
			if strings.TrimSpace(assistant.Content) == "" {
				if cut {
					return l.exit(ctx, state, usage, outputLimitExit(response))
				}
				return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindEmptyResponse, Err: errors.New("model returned neither tool calls nor final content")})
			}
			if err := l.debugBoundary(ctx, DebugBoundary{Event: "BeforeDelivery", Step: step}); err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
			}
			// Background jobs the run started come first: their results may be what the rest of the gate
			// needs (ADR-0019 §3).
			jobsCorrection := l.jobsCorrection(ctx, sessionID, step, opts)
			var correction string
			// decided are the required checks this completion already decided: the verification gate
			// reuses them, so a check is never run twice for one proposal.
			var decided []evaluator.Verdict
			switch {
			case jobsCorrection != "":
				correction = jobsCorrection
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
					_, all, cause := l.requiredChecksAndVerdicts(ctx)
					if cause != nil {
						return l.exit(ctx, state, usage, *cause)
					}
					decided = all
				}
			case len(r.units.Incomplete()) > 0:
				remaining := r.units.Incomplete()
				correction = workUnitCompletionCorrection(remaining)
				l.emit("work_unit.blocked_final", sessionID, map[string]any{"step": step, "remaining": remaining})
			default:
				// The completion gate's last condition: every required check decided passed (§4.7).
				failed, all, cause := l.requiredChecksAndVerdicts(ctx)
				if cause != nil {
					return l.exit(ctx, state, usage, *cause)
				}
				decided = all
				if unapproved := unapprovedChecks(failed); len(unapproved) > 0 {
					// Only the user can approve a check definition (ADR-0017): the model cannot clear
					// this, so the run ends here instead of looping on a correction.
					l.emit("checks.blocked_final", sessionID, map[string]any{"step": step, "checks": verdictViews(failed)})
					return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindSpecUnapproved,
						Err: fmt.Errorf("%w: %s", ErrChecksUnapproved, strings.Join(unapproved, ", "))})
				}
				if len(failed) > 0 {
					correction = checksCorrection(failed)
					l.emit("checks.blocked_final", sessionID, map[string]any{"step": step, "checks": verdictViews(failed)})
				}
			}
			if correction == "" && cut {
				// A reply cut at the output limit is never the final answer: without a tool call it
				// holds an unfinished thought (TB2 W13: such replies ended runs as completed). It ends
				// the run only where the reply would have completed it; a completion-gate correction
				// above still applies and the run continues as it did before.
				return l.exit(ctx, state, usage, outputLimitExit(response))
			}
			if correction == "" {
				// The verification levels are opt-in: a run without a host request, a required claim or
				// an explicit verifier keeps the pre-existing completion path. When they are on, they
				// run before the evidence rule, so a delivery or execution pass never becomes a
				// semantic pass.
				if l.verificationRequested(opts, r) {
					var cause *ExitCause
					if correction, cause = l.verificationGate(ctx, sessionID, r, opts, step, assistant.Content, &usage, decided); cause != nil {
						return l.exit(ctx, state, usage, *cause)
					}
				} else {
					correction = l.completionHooks(ctx, sessionID, opts, step)
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
				blocked := protocol.Message{Role: "user", Origin: protocol.OriginCompletionBlocked,
					Content: presentFeedback(correction, opts.Presentation.Feedback)}
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
			l.observeExitHook(ctx, sessionID, opts.RunID, verifyEventOnClose, map[string]any{"step": step, "status": StatusCompleted})
			l.emit("session.complete", sessionID, map[string]any{"step": step})
			return Result{SessionID: sessionID, Status: StatusCompleted, Final: final, Usage: usage, Workflow: workflowSummary(r.wf)}, nil
		}

		// Under a length finish the reply's last call is the one being written when the limit cut the
		// reply, and nothing shows it was finished: arguments that parse as a whole JSON object do not,
		// because servers that parse a model's own call format (DeepSeek's DSML) or return a parsed
		// input object (Anthropic) close the object themselves. It is answered with a tool error instead
		// of run, and the model can send it again. Earlier calls were finished: generation had moved on.
		// When MaxToolCallsPerStep dropped the tail, the last call is not among calls.
		cutIndex := -1
		if response.FinishReason == protocol.FinishLength && len(calls) == len(response.Message.ToolCalls) {
			cutIndex = len(calls) - 1
		}
		for i, call := range calls {
			if err := ctx.Err(); err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
			}
			if err := l.debugBoundary(ctx, DebugBoundary{Event: "BeforeToolCall", Step: step, Tool: call.Name, ToolCallID: call.ID}); err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
			}
			if call.Name == toolStartTask || call.Name == toolSpawnSubagent {
				if err := l.debugBoundary(ctx, DebugBoundary{Event: "BeforeSubagentStart", Step: step, Tool: call.Name, ToolCallID: call.ID}); err != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
				}
			}
			args, argsErr := parseToolArguments(call.Arguments)
			var refusal string
			switch {
			case i == cutIndex:
				refusal = "not executed: the reply reached the output token limit while this call was being written; send the call again"
			case argsErr != nil:
				refusal = "invalid tool arguments: " + argsErr.Error()
			}
			if refusal != "" {
				rejected := protocol.ToolResult{OK: false, Content: clock.annotate(toolErrorContent(refusal), time.Now())}
				if _, err := l.persistToolResult(ctx, sessionID, opts.RunID, call, rejected, nil, nil, nil, r); err != nil {
					return l.exit(ctx, state, usage, *storeExit(err))
				}
				for _, point := range []string{"AfterToolCall", "ToolFailure"} {
					if err := l.debugBoundary(ctx, DebugBoundary{Event: point, Step: step, Tool: call.Name, ToolCallID: call.ID, Failed: true}); err != nil {
						return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
					}
				}
				continue
			}
			denied := !containsTool(specs, call.Name) || (r.wf != nil && !r.wf.IsToolAllowed(call.Name))
			var (
				toolResult protocol.ToolResult
				x          *execution
				post       *session.ExecutionResult
				edges      []session.DerivedEdge
				cb         *callbackDispatcher
			)
			if denied {
				content := "tool is not allowed in the current working set"
				if replacement, ok := protocol.ReplacedToolName(call.Name); ok && containsTool(specs, replacement) {
					// A resumed session's history may still show the old name.
					content = toolErrorContent(fmt.Sprintf("tool %s was renamed to %s: call %s with the same arguments", call.Name, replacement, replacement))
				}
				toolResult = protocol.ToolResult{OK: false, Denied: true, Permission: "workflow", Content: content}
			} else {
				// Lifecycle hooks run before any admission or execution (spec §20). A deny answers the
				// call without running it; a mutable hook may replace arguments, never policy. Each
				// replacement is validated against the tool's schema before a later hook sees it, and
				// safety hooks decide the final arguments.
				pre := l.runHook(ctx, sessionID, opts.RunID, verifyEventPreToolUse, HookRequest{
					Step: step, ToolCallID: call.ID, ToolName: call.Name, ToolArgs: args,
					ValidateArgs: func(candidate map[string]any) error {
						spec, ok := toolSpecOf(specs, call.Name)
						if !ok {
							return nil
						}
						return validateToolArgs(spec, candidate)
					},
				})
				hookDenied := pre.Denied()
				if !hookDenied && pre.Args != nil {
					args = mergeArgs(args, pre.Args)
					// The tool validated the model's arguments before the hook ran; a replacement is
					// validated again against the same schema, so a hook cannot authorize input the
					// tool itself refuses.
					if spec, ok := toolSpecOf(specs, call.Name); ok {
						if err := validateToolArgs(spec, args); err != nil {
							hookDenied = true
							pre = verify.Outcome{Decision: verify.DecisionDeny,
								Feedback: []string{"the hook's replacement arguments are invalid for this tool: " + err.Error()}}
						}
					}
				}
				if !hookDenied && isEvalTool(call.Name) {
					evalPre := l.runHook(ctx, sessionID, opts.RunID, verifyEventPreEval, HookRequest{
						Step: step, ToolCallID: call.ID, ToolName: call.Name, ToolArgs: args,
					})
					if evalPre.Denied() {
						hookDenied, pre = true, evalPre
					}
				}
				if !hookDenied && isSubagentTool(call.Name) {
					subPre := l.runHook(ctx, sessionID, opts.RunID, verifyEventPreSubagent, HookRequest{
						Step: step, ToolCallID: call.ID, ToolName: call.Name, ToolArgs: args,
					})
					if subPre.Denied() {
						hookDenied, pre = true, subPre
					}
				}
				if hookDenied {
					toolResult = protocol.ToolResult{OK: false, Denied: true, Permission: "hook",
						Content: appendHookFeedback(toolErrorContent("denied by a lifecycle hook"), hookFeedback(pre))}
				} else {
					// A side-effect tool runs only after its admission is durable (plan T1-S4). From here
					// until the result commits, a crash or cancellation leaves the execution admitted, and
					// Reconcile makes it outcome_unknown; it is never run again.
					if tier := executionTierOf(call.Name, specs); tier != tierNone {
						var cause *ExitCause
						if x, cause = l.admitExecution(ctx, sessionID, opts, answered.RequestID, call, args, tier); cause != nil {
							return l.exit(ctx, state, usage, *cause)
						}
					}
					var toolErr error
					toolStarted := time.Now()
					spend.ToolCalls++
					l.emit("tool.start", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "args": args})
					if err := ctx.Err(); err != nil {
						return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
					}
					if call.Name == toolStartTask {
						toolResult = l.startTask(ctx, r, opts, args, time.Now().UTC())
						if ctx.Err() != nil {
							// Cancelled while starting: the call stays unanswered and the run ends. A child
							// that did start is still delivered through its relation.
							return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: ctx.Err()})
						}
					} else if isTaskTool(call.Name) {
						var cause *ExitCause
						toolResult, cause = l.executeTaskTool(ctx, r, call.Name, args, time.Now().UTC())
						if cause != nil {
							// Deciding a unit's checks was cancelled or failed: the call stays
							// unanswered and the run ends.
							return l.exit(ctx, state, usage, *cause)
						}
					} else {
						// The tool sees its call id (a job names the call that started it) and the time by
						// which it must be done (ADR-0019 §2); the run's own deadline stays the hard stop.
						toolCtx := protocol.WithToolCallID(ctx, call.ID)
						if until, ok := clock.toolDeadline(); ok {
							toolCtx = protocol.WithToolDeadline(toolCtx, until)
						}
						cancelCallbacks := context.CancelFunc(func() {})
						if caller := l.toolCallback(sessionID, opts.RunID, answered.RequestID, step, call, specs, spend, opts.Budget, answered.Lineage); caller != nil {
							toolCtx, cancelCallbacks = context.WithCancel(toolCtx)
							toolCtx = protocol.WithToolCallback(toolCtx, caller)
							if d, ok := caller.(*callbackDispatcher); ok {
								cb = d
							}
						}
						toolResult, toolErr = l.Tools.Execute(toolCtx, call.Name, args, func(output protocol.ToolOutput) {
							l.emit("tool.output", sessionID, map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "stream": output.Stream, "chunk": output.Chunk})
						})
						cancelCallbacks()
						if cb != nil {
							cb.close()
						}
						if ctx.Err() != nil {
							// The command tool reports a killed command as a result, not an error, and its
							// call is answered before the run ends: one terminal row with the partial
							// output and its execution settled, so the transcript and the execution are
							// terminal rather than waiting for the next Start. Every other tool keeps the
							// older contract: its stopped call stays unanswered and Reconcile closes it.
							if call.Name == protocol.ToolBash {
								if closeErr := l.closeStoppedCall(ctx, sessionID, opts.RunID, call, x, toolResult, toolErr, r); closeErr != nil {
									// The run was cancelled; its closing write failed. The cause stays the
									// cancellation the run really had (a cancelled run's storage error is the
									// cancellation, loop exit rule), and the failure is joined so it is reported
									// rather than hidden. The transcript keeps the gap Reconcile closes later.
									cause := ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: errors.Join(ctx.Err(), closeErr)}
									if errors.Is(context.Cause(ctx), ErrRunDeadline) {
										cause = ExitCause{Status: StatusInterrupted, Kind: KindDeadline, Err: errors.Join(ErrRunDeadline, closeErr)}
									}
									return l.exit(ctx, state, usage, cause)
								}
							}
							return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: ctx.Err()})
						}
						if toolErr != nil {
							toolResult = protocol.ToolResult{OK: false, Content: fmt.Sprintf(`{"error":%q}`, toolErr.Error())}
						}
						if toolResult.MutatesWorkspace {
							// A change to the workspace is a new obligation, whether or not the call succeeded.
							r.ts.Reopen(time.Now().UTC())
							r.mutated = true
						}
					}
					clock.observeTool(call.Name, time.Since(toolStarted))
					if x != nil {
						post, edges = l.postExecution(ctx, x, toolResult, toolErr, answered.RequestID, answered.Lineage)
					}
					if (x != nil || toolResult.MutatesWorkspace) && l.Checks != nil {
						// No check episode closed before a side-effect tool is reused after it (ADR-0017).
						l.Checks.Mutated()
					}
				}
			}
			// Post hooks observe every call, a denied one included.
			if isEvalTool(call.Name) {
				evalPost := l.runHook(ctx, sessionID, opts.RunID, verifyEventPostEval, HookRequest{
					Step: step, ToolCallID: call.ID, ToolName: call.Name, ToolArgs: args, ToolResult: &toolResult,
				})
				if feedback := evalPost.FeedbackText(); feedback != "" {
					toolResult.Content = appendHookFeedback(toolResult.Content, feedback)
				}
			}
			if isSubagentTool(call.Name) {
				subPost := l.runHook(ctx, sessionID, opts.RunID, verifyEventPostSubagent, HookRequest{
					Step: step, ToolCallID: call.ID, ToolName: call.Name, ToolArgs: args, ToolResult: &toolResult,
				})
				if feedback := subPost.FeedbackText(); feedback != "" {
					toolResult.Content = appendHookFeedback(toolResult.Content, feedback)
				}
			}
			postHook := l.runHook(ctx, sessionID, opts.RunID, verifyEventPostToolUse, HookRequest{
				Step: step, ToolCallID: call.ID, ToolName: call.Name, ToolArgs: args, ToolResult: &toolResult,
			})
			if feedback := postHook.FeedbackText(); feedback != "" {
				toolResult.Content = appendHookFeedback(toolResult.Content, feedback)
			}
			end := map[string]any{"step": step, "toolCallId": call.ID, "name": call.Name, "ok": toolResult.OK, "denied": toolResult.Denied}
			if x != nil {
				end["executionId"], end["outcome"], end["coverage"], end["snapshotNanos"] = x.id, string(post.Outcome), string(post.Coverage), x.snapshot.Nanoseconds()
			}
			l.emit("tool.end", sessionID, end)
			workflowTransition := false
			if r.wf != nil {
				update, wfErr := r.wf.ObserveTool(workflow.ToolObservation{Tool: call.Name, Args: args, Result: map[string]any{"ok": toolResult.OK, "denied": toolResult.Denied, "permission": toolResult.Permission, "content": toolResult.Content}, Step: step})
				if wfErr != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindConfig, Err: wfErr})
				}
				if len(update.ChangedFacts) > 0 {
					l.emit("workflow.facts", sessionID, map[string]any{"step": step, "facts": update.ChangedFacts})
				}
				if update.Transition != nil {
					workflowTransition = true
					l.emit("workflow.transition", sessionID, map[string]any{"step": step, "from": update.Transition.From, "to": update.Transition.To, "reason": update.Transition.Reason})
				}
				r.ts.SetWorkflow(r.wf)
			}
			// The Task State change this result caused commits with its row (design §4.4), with the
			// edges of the entries it wrote and of the sessions a subagent or thread tool carried (T1-S5, T3-S1).
			change, err := r.change(taskstate.ReasonToolPrefix + call.Name)
			if err != nil {
				return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
			}
			if change != nil {
				before, err := taskstate.Decode(0, r.saved)
				if err != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusFailed, Kind: KindStorage, Err: err})
				}
				edges = append(edges, modelStateEdges(before, change.Next, answered.RequestID, answered.Lineage)...)
			}
			edges = append(edges, l.sessionResultEdges(ctx, sessionID, call, toolResult, answered.RequestID, answered.Lineage)...)
			toolResult.Content = clock.annotate(toolResult.Content, time.Now())
			messageSeq, err := l.persistToolResult(ctx, sessionID, opts.RunID, call, toolResult, change, post, edges, r)
			if err != nil {
				return l.exit(ctx, state, usage, *storeExit(err))
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
			points := []string{"AfterToolCall"}
			if toolResult.Denied {
				points = append(points, "PermissionDenied")
			} else if !toolResult.OK {
				points = append(points, "ToolFailure")
			}
			if workflowTransition {
				points = append(points, "WorkflowTransition")
			}
			for _, point := range points {
				if err := l.debugBoundary(ctx, DebugBoundary{Event: point, Step: step, Tool: call.Name, ToolCallID: call.ID, Failed: !toolResult.OK, Denied: toolResult.Denied}); err != nil {
					return l.exit(ctx, state, usage, ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: err})
				}
			}
			if cb != nil && (len(cb.journal.Entries) > 0 || cb.journal.Dropped > 0) {
				// Nested calls have no tool call of their own: a bounded journal replaces unmatched rows.
				l.checkpoint(sessionID, "agent.callbacks", map[string]any{
					"step": step, "toolCallId": call.ID, "tool": call.Name, "journal": cb.journal,
				})
			}
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
	before := r.ts.Clone()
	r.ts.Apply(edits)
	change := &TaskStateChange{Reason: taskstate.ReasonUser, Next: r.ts, InboxThrough: edits[len(edits)-1].Seq, Edges: HostStateEdges(before, r.ts)}
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
// that did not pass, or the exit when deciding was cancelled or failed. A check set the host approved
// and that was changed since (emptied, a check removed or made optional) fails as a whole with one
// spec_unapproved verdict, whatever it requires now (ADR-0017).
func (l *Loop) requiredChecks(ctx context.Context) ([]evaluator.Verdict, *ExitCause) {
	failed, _, cause := l.requiredChecksAndVerdicts(ctx)
	return failed, cause
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

// systemMessages is the request's fixed system prefix: the default prompt, then each labelled layer
// in order (the global instructions, the project instructions, the skills prompt), then the agent's
// instructions. Instructions never replace or precede the default prompt, and every layer is a
// message of its own so the model can attribute it to its source.
func systemMessages(opts Options) []protocol.Message {
	system := []protocol.Message{{Role: "system", Content: opts.SystemPrompt}}
	for _, layer := range opts.SystemLayers {
		if strings.TrimSpace(layer.Text) == "" {
			continue
		}
		system = append(system, protocol.Message{Role: "system", Content: layer.Text})
	}
	if strings.TrimSpace(opts.Instructions) != "" {
		system = append(system, protocol.Message{Role: "system", Content: opts.Instructions})
	}
	return system
}

// persistToolResult commits a tool row with, when change is non-nil, the Task State version it caused,
// the execution's post record (post is nil for a call that was not admitted) and its derived edges,
// in one transaction; the run's state then carries the stored version.
func (l *Loop) persistToolResult(ctx context.Context, sessionID, runID string, call protocol.ToolCall, result protocol.ToolResult, change *TaskStateChange, post *session.ExecutionResult, edges []session.DerivedEdge, r *run) (int64, error) {
	row := protocol.Message{Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: result.Content}
	seq, version, err := l.Store.CommitToolResult(ctx, ToolResultCommit{SessionID: sessionID, RunID: runID, Message: row, Change: change, Execution: post, Edges: edges})
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

// closeStoppedCall answers the call a cancelled or expired run stopped mid-execution: one tool row with
// the interrupted result, and, for an admitted call, its execution settled outcome_unknown in the same
// transaction. It runs without the run's cancellation and is bounded like the terminal write, so a run
// that is cancelled ends with a paired transcript and a terminal execution; Reconcile then finds no gap
// to close at the next Start (ADR-0018).
//
// A failed write is returned, never swallowed: the caller reports it with the run's exit, and the
// transcript keeps the gap Reconcile closes at the next Start.
func (l *Loop) closeStoppedCall(ctx context.Context, sessionID, runID string, call protocol.ToolCall, x *execution, result protocol.ToolResult, toolErr error, r *run) error {
	content := session.InterruptedToolResult
	if toolErr == nil && strings.TrimSpace(result.Content) != "" {
		// The partial output the tool had read, with the reason it stopped.
		content = result.Content
	}
	row := protocol.Message{Role: "tool", ToolCallID: call.ID, Name: call.Name, Content: content, Origin: protocol.OriginToolInterrupted}
	var post *session.ExecutionResult
	if x != nil {
		post = &session.ExecutionResult{ExecutionID: x.id, Outcome: session.OutcomeUnknown, Coverage: session.CoverageUnavailable}
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finishTimeout)
	defer cancel()
	seq, _, err := l.Store.CommitToolResult(writeCtx, ToolResultCommit{SessionID: sessionID, RunID: runID, Message: row, Execution: post})
	if err != nil {
		l.emit("tool.end", sessionID, map[string]any{"toolCallId": call.ID, "name": call.Name, "ok": false, "stopped": true, "error": err.Error()})
		return fmt.Errorf("persist the interrupted result of call %s: %w", call.ID, err)
	}
	r.appendRow(seq, row)
	end := map[string]any{"toolCallId": call.ID, "name": call.Name, "ok": false, "stopped": true, "interrupted": true}
	if x != nil {
		end["executionId"], end["outcome"] = x.id, string(session.OutcomeUnknown)
	}
	l.emit("tool.end", sessionID, end)
	return nil
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

// outputLimitExit ends a run whose reply, without a tool call, was cut at the output token limit.
func outputLimitExit(response protocol.ProviderResponse) ExitCause {
	return ExitCause{Status: StatusInterrupted, Kind: KindOutputLimit,
		Err: fmt.Errorf("the model reply reached the output token limit (%d completion tokens) without a tool call or a finished answer", response.Usage.CompletionTokens)}
}

// exit ends the run with an interrupted or failed cause: finish, then the
// terminal event. A storage or provider error that arrives with the run context
// already cancelled is the cancellation, not a failure; a cancellation caused by
// the run's deadline is KindDeadline.
func (l *Loop) exit(ctx context.Context, state SessionState, usage protocol.Usage, cause ExitCause) (Result, error) {
	if ctx.Err() != nil && (cause.Kind == KindCancelled || cause.Kind == KindStorage || strings.HasPrefix(cause.Kind, "provider:")) {
		if errors.Is(context.Cause(ctx), ErrRunDeadline) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			cause = ExitCause{Status: StatusInterrupted, Kind: KindDeadline, Err: ErrRunDeadline}
		} else if cause.Kind != KindCancelled {
			cause = ExitCause{Status: StatusInterrupted, Kind: KindCancelled, Err: ctx.Err()}
		}
	}
	if err := l.finish(ctx, state, usage, cause, nil); err != nil {
		return finishFailure(state, usage, cause, err)
	}
	message := ""
	if cause.Err != nil {
		message = cause.Err.Error()
	}
	runID := runIDFrom(ctx)
	// Exit hooks observe an event that already happened. They get a context that is not the run's
	// own: the run's cancellation or deadline is what ended it, so a hook on that context could
	// never do its work. It is bounded, and it still ends when the host cancels the session.
	hookCtx, cancelHook := context.WithTimeout(context.WithoutCancel(ctx), exitHookTimeout)
	defer cancelHook()
	switch {
	case cause.Kind == KindDeadline:
		l.observeExitHook(hookCtx, state.ID, runID, verifyEventOnTimeout, exitHookDetail(cause.Kind, message, time.Now()))
	case cause.Status == StatusInterrupted && cause.Kind == KindCancelled:
		l.observeExitHook(hookCtx, state.ID, runID, verifyEventOnCancel, exitHookDetail(cause.Kind, message, time.Now()))
	case cause.Status == StatusFailed || cause.Status == StatusInterrupted:
		// Every other ending that is not a completion — max steps, an exhausted verification gate, a
		// budget limit, an empty reply — is a failure of the run to finish its task, and its exit
		// hooks observe it.
		l.observeExitHook(hookCtx, state.ID, runID, verifyEventOnFailure, exitHookDetail(cause.Kind, message, time.Now()))
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
