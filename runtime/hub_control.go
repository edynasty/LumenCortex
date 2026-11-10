package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
)

// The Agent Hub is a control plane, not a second runtime (AGENT_HUB_ARCHITECTURE §3): it projects the
// state RunSupervisor, the run's DebugControl, Task State, jobs, DAP and Eval already hold, and routes a
// host command to their existing APIs. It keeps no execution state of its own; a command either reaches
// the live runtime or is refused.

// Hub verbs. Every one names an existing mechanism; none grants authority a run did not have.
const (
	HubVerbStart   = "start"
	HubVerbPause   = "pause"
	HubVerbResume  = "resume"
	HubVerbStep    = "step"
	HubVerbCancel  = "cancel"
	HubVerbRetry   = "retry"
	HubVerbSteer   = "steer"
	HubVerbHandoff = "handoff"
	HubVerbAttach  = "attach"
	HubVerbDetach  = "detach"
)

// Hub command outcomes.
const (
	HubAccepted  = "accepted"
	HubRejected  = "rejected"
	HubDuplicate = "duplicate"
)

// Hub agent states. pause_requested and paused come from the run's DebugControl; the rest are the
// session's durable status, with interrupted(paused) the projection of a pause that survived a restart.
const (
	HubStateIdle            = "idle"
	HubStateRunning         = "running"
	HubStatePauseRequested  = "pause_requested"
	HubStatePaused          = "paused"
	HubStateCancelling      = "cancelling"
	HubStateCompleted       = "completed"
	HubStateFailed          = "failed"
	HubStateInterrupted     = "interrupted"
	HubStateInterruptedStop = "interrupted(paused)"
	HubStateWaitingGate     = "waiting_gate"
)

var (
	// ErrHubStaleRevision: the command names an older revision than the agent's; the host must refresh
	// its view instead of retrying (the E5 C4 rule, extended to every verb).
	ErrHubStaleRevision = errors.New("hub command is stale; refresh the agent view")
	// ErrHubRunInactive: the verb needs a live run of the session and there is none.
	ErrHubRunInactive = errors.New("hub command needs a live run of this session")
	// ErrHubRejected: the runtime refused the command in the agent's current state.
	ErrHubRejected = errors.New("hub command rejected")
	// ErrHubCommandInvalid: the command is malformed or reuses a command id for another effect.
	ErrHubCommandInvalid = errors.New("invalid hub command")
)

// HubSpend is the session's recorded usage. Core does not persist a price per request, so the cost is
// reported as unknown instead of estimated here (ADR-0019 §6).
type HubSpend struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"promptTokens"`
	CompletionTokens int64 `json:"completionTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	CostKnown        bool  `json:"costKnown"`
}

// HubTool is the tool a run is executing now.
type HubTool struct {
	Name       string `json:"name"`
	ToolCallID string `json:"toolCallId,omitempty"`
}

// HubAgent is one agent of the Hub's tree, projected from live and durable state. It carries metadata
// only: no goal body, no model content and no reasoning.
type HubAgent struct {
	SessionID       string `json:"sessionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	RunID           string `json:"runId,omitempty"`
	// Role is main (the root), subagent or execution_task, from the session's relation.
	Role     string              `json:"role"`
	Agent    string              `json:"agent,omitempty"`
	Model    string              `json:"model,omitempty"`
	State    string              `json:"state"`
	Live     bool                `json:"live"`
	Boundary agent.DebugBoundary `json:"boundary"`
	// CurrentTool is set only while a tool is executing.
	CurrentTool *HubTool          `json:"currentTool,omitempty"`
	StartedAt   time.Time         `json:"startedAt,omitempty"`
	Spend       *HubSpend         `json:"spend,omitempty"`
	Debug       []DebugSnapshot   `json:"debug,omitempty"`
	Eval        []ExecutionKernel `json:"eval,omitempty"`
	Jobs        []JobInfo         `json:"jobs,omitempty"`
	Holdings    []HubTaskLease    `json:"holdings,omitempty"`
	// Revision is the command concurrency token: the live run's debug revision, or 0 with no live run.
	Revision uint64 `json:"revision"`
	// RetryOf is the run this run was started to retry, for the run after an `agent.retry`.
	RetryOf string `json:"retryOf,omitempty"`
	// Paused is the durable pause record. It outlives the process, so a restart shows
	// interrupted(paused) instead of a silently idle session.
	Paused *session.RunPause `json:"paused,omitempty"`
}

// HubView is the root's whole observable state: the agent tree, the task DAG with its claims, and the
// cursor a subscriber resumes from.
type HubView struct {
	Root        string         `json:"root"`
	GeneratedAt time.Time      `json:"generatedAt"`
	Cursor      uint64         `json:"cursor"`
	Agents      []HubAgent     `json:"agents"`
	Tasks       []HubTask      `json:"tasks"`
	Leases      []HubTaskLease `json:"leases,omitempty"`
}

// HubCommand is a host command. CommandID makes it idempotent: the same id always returns the result the
// first execution produced. ExpectedRevision must equal the addressed agent's Revision (0 when it has no
// live run), so a delayed UI action cannot drive a run it has not seen.
type HubCommand struct {
	SessionID        string          `json:"sessionId"`
	RunID            string          `json:"runId,omitempty"`
	CommandID        string          `json:"commandId"`
	ExpectedRevision uint64          `json:"expectedRevision"`
	Verb             string          `json:"verb"`
	Args             json.RawMessage `json:"args,omitempty"`
}

// HubCommandResult is what a command produced: the outcome, the agent's state after it, and why it was
// refused.
type HubCommandResult struct {
	Status    string   `json:"status"`
	Verb      string   `json:"verb"`
	CommandID string   `json:"commandId"`
	Agent     HubAgent `json:"agent"`
	Error     string   `json:"error,omitempty"`
	Note      string   `json:"note,omitempty"`
	// Target is the session a handoff handed the work to, empty for every other verb.
	Target string `json:"target,omitempty"`
}

// HubArgs is the payload of the verbs that take one. Unused fields are ignored per verb.
type HubArgs struct {
	Input    string `json:"input,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Policy   string `json:"policy,omitempty"`
	MaxSteps int    `json:"maxSteps,omitempty"`
	// Text is the free-text steer instruction.
	Text string `json:"text,omitempty"`
	// Unit selects the step granularity: agent, model, tool or turn (default model).
	Unit string `json:"unit,omitempty"`
	// Target is the session a handoff hands the work to.
	Target string `json:"target,omitempty"`
	// Note is the handoff note the target reads; the runtime also writes it into Task State.
	Note string `json:"note,omitempty"`
}

// View builds the root's Hub projection. It holds no lock across a command: every part is read from the
// component that owns it.
func (s *RunSupervisor) View(ctx context.Context, root string) (HubView, error) {
	out := HubView{Root: root, GeneratedAt: time.Now().UTC(), Agents: []HubAgent{}, Tasks: []HubTask{}}
	if s == nil || s.engine == nil {
		return out, ErrRunSupervisorClosed
	}
	e := s.engine
	if err := e.runtimeEventAccess(ctx, root); err != nil {
		return out, err
	}
	state, err := e.loadTaskState(ctx, root)
	if err != nil {
		return out, err
	}
	tasks, err := e.hubTasks(ctx, root, state)
	if err != nil {
		return out, err
	}
	leases := e.hubLeaseViews(ctx, root)
	for i := range tasks {
		if lease, ok := leases[tasks[i].ID]; ok {
			copied := lease
			tasks[i].Lease = &copied
			if tasks[i].State == "ready" || tasks[i].State == "pending" {
				tasks[i].State = "leased"
			}
		}
	}
	out.Tasks = tasks
	if out.Leases, err = e.store.TaskLeases(ctx, root); err != nil {
		return out, err
	}
	tree, err := s.SubagentTree(ctx, root)
	if err != nil {
		return out, err
	}
	ids := make([]string, 0, len(tree)+1)
	ids = append(ids, root)
	for _, node := range tree {
		ids = append(ids, node.SessionID)
	}
	pauses, err := e.store.RunPauses(ctx, ids)
	if err != nil {
		return out, err
	}
	roles := make(map[string]string, len(tree)+1)
	parents := make(map[string]string, len(tree)+1)
	for _, node := range tree {
		parents[node.SessionID] = node.ParentSessionID
		roles[node.SessionID] = node.Kind
	}
	for _, id := range ids {
		view, err := s.hubAgent(ctx, id, roles[id], parents[id], pauses[id])
		if err != nil {
			if errors.Is(err, session.ErrNotFound) {
				continue
			}
			return out, err
		}
		if view.Role == "" {
			view.Role = "main"
		}
		out.Agents = append(out.Agents, view)
	}
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: root, Limit: 1})
	if err != nil {
		return out, err
	}
	out.Cursor = page.HighWatermark
	return out, nil
}

// hubAgent projects one session. role and parent come from the tree walk; paused is its durable pause
// record, if any.
func (s *RunSupervisor) hubAgent(ctx context.Context, sessionID, role, parent string, paused session.RunPause) (HubAgent, error) {
	e := s.engine
	row, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return HubAgent{}, err
	}
	out := HubAgent{SessionID: sessionID, ParentSessionID: parent, Role: role,
		Agent: row.AgentID, Model: row.Model, State: HubStateIdle}
	if paused.SessionID != "" {
		copied := paused
		out.Paused = &copied
	}
	switch row.Status {
	case "created":
		out.State = HubStateIdle
	case "running":
		out.State = HubStateRunning
	case "completed":
		out.State = HubStateCompleted
	case "failed":
		out.State = HubStateFailed
	case "interrupted":
		out.State = HubStateInterrupted
	case "waiting_gate":
		out.State = HubStateWaitingGate
	}
	if usage := sessionUsage(row); usage != nil {
		out.Spend = usage
	}
	s.mu.RLock()
	run := s.runs[sessionID]
	startedAt := time.Time{}
	cancelRequested := false
	if run != nil {
		startedAt, cancelRequested = run.startedAt, run.cancelRequested
	}
	s.mu.RUnlock()
	if run != nil {
		out.Live, out.StartedAt = true, startedAt
		if cancelRequested {
			out.State = HubStateCancelling
		}
	}
	e.runsMu.Lock()
	tracked := e.runs[sessionID]
	var snapshot *agent.DebugSnapshot
	if tracked != nil && tracked.debug != nil {
		copied := tracked.debug.Snapshot()
		snapshot = &copied
		out.RetryOf = tracked.retryOf
	}
	e.runsMu.Unlock()
	if snapshot != nil {
		out.RunID, out.Revision, out.Boundary = snapshot.RunID, snapshot.Revision, snapshot.Boundary
		switch snapshot.State {
		case "running":
			if out.State == HubStateRunning || out.State == HubStateIdle {
				out.State = HubStateRunning
			}
		case "pause_requested", "paused":
			out.State = snapshot.State
		}
		if snapshot.Boundary.Event == "BeforeToolCall" && snapshot.State != "paused" {
			out.CurrentTool = &HubTool{Name: snapshot.Boundary.Tool, ToolCallID: snapshot.Boundary.ToolCallID}
		}
	}
	if !out.Live && out.Paused != nil && out.State == HubStateInterrupted {
		out.State = HubStateInterruptedStop
	}
	// Program debugging and Eval kernels are optional subsystems: a session without the grant reports
	// none instead of failing the whole projection.
	if e.dapManager != nil {
		if sessions, err := e.DebugSessions(ctx, sessionID); err == nil {
			out.Debug = sessions
		}
	}
	if e.evalManager != nil {
		if kernels, err := e.ExecutionKernels(ctx, sessionID); err == nil {
			out.Eval = kernels
		}
	}
	if e.jobs != nil {
		if jobs, err := e.Jobs(ctx, JobQuery{SessionID: sessionID, Limit: 20}); err == nil {
			out.Jobs = jobs
		}
	}
	if leases, err := e.store.TaskLeases(ctx, sessionID); err == nil {
		for _, lease := range leases {
			if lease.Expired {
				continue
			}
			out.Holdings = append(out.Holdings, lease)
		}
	}
	return out, nil
}

// Command routes one host command to the live runtime and records its result under its CommandID, so a
// retry returns the first result instead of a second effect.
func (s *RunSupervisor) Command(ctx context.Context, c HubCommand) (HubCommandResult, error) {
	if s == nil || s.engine == nil {
		return HubCommandResult{}, ErrRunSupervisorClosed
	}
	e := s.engine
	if err := validateHubCommand(c); err != nil {
		return HubCommandResult{Status: HubRejected, Verb: c.Verb, CommandID: c.CommandID, Error: err.Error()}, err
	}
	if err := e.runtimeEventAccess(ctx, c.SessionID); err != nil {
		return HubCommandResult{}, err
	}
	if record, err := e.store.HubCommand(ctx, c.CommandID); err == nil {
		return duplicateHubResult(c, record)
	} else if !errors.Is(err, session.ErrNotFound) {
		return HubCommandResult{}, err
	}
	result, err := s.applyHubCommand(ctx, c)
	if err != nil && errors.Is(err, session.ErrNotFound) {
		return HubCommandResult{}, err
	}
	result.CommandID, result.Verb = c.CommandID, c.Verb
	if stored, marshalErr := json.Marshal(result); marshalErr == nil {
		_ = e.store.RecordHubCommand(ctx, session.HubCommandRecord{
			CommandID: c.CommandID, SessionID: c.SessionID, RunID: result.Agent.RunID,
			Verb: c.Verb, Result: stored,
		})
	}
	data := map[string]any{"verb": c.Verb, "commandId": c.CommandID, "status": result.Status}
	if result.Error != "" {
		data["error"] = result.Error
	}
	if result.Status == HubAccepted {
		e.events.publish("hub.command.accepted", c.SessionID, data)
	} else {
		e.events.publish("hub.command.rejected", c.SessionID, data)
	}
	return result, err
}

// duplicateHubResult returns the recorded result of a repeated CommandID. A reused id that names another
// verb or session is an error, never a silent replay.
func duplicateHubResult(c HubCommand, record session.HubCommandRecord) (HubCommandResult, error) {
	var stored HubCommandResult
	if err := json.Unmarshal(record.Result, &stored); err != nil {
		return HubCommandResult{}, fmt.Errorf("%w: stored result is unreadable", ErrHubCommandInvalid)
	}
	if record.SessionID != c.SessionID || record.Verb != c.Verb {
		return HubCommandResult{Status: HubRejected, Verb: c.Verb, CommandID: c.CommandID,
			Error: "command id already names another command"}, fmt.Errorf("%w: command id reused", ErrHubCommandInvalid)
	}
	stored.Status = HubDuplicate
	return stored, nil
}

func validateHubCommand(c HubCommand) error {
	if c.SessionID == "" || len(c.SessionID) > 128 {
		return fmt.Errorf("%w: session is required", ErrHubCommandInvalid)
	}
	if strings.TrimSpace(c.CommandID) == "" || len(c.CommandID) > 128 {
		return fmt.Errorf("%w: commandId is required", ErrHubCommandInvalid)
	}
	switch c.Verb {
	case HubVerbStart, HubVerbPause, HubVerbResume, HubVerbStep, HubVerbCancel,
		HubVerbRetry, HubVerbSteer, HubVerbHandoff, HubVerbAttach, HubVerbDetach:
		return nil
	}
	return fmt.Errorf("%w: unknown verb %q", ErrHubCommandInvalid, c.Verb)
}

// applyHubCommand executes one verb. The revision check is the same rule for every verb: a live run's
// Revision must match, and a session without a live run accepts revision 0 only.
func (s *RunSupervisor) applyHubCommand(ctx context.Context, c HubCommand) (HubCommandResult, error) {
	e := s.engine
	var args HubArgs
	if len(c.Args) > 0 {
		if err := json.Unmarshal(c.Args, &args); err != nil {
			return rejectedHub(c, fmt.Errorf("%w: args: %v", ErrHubCommandInvalid, err)), fmt.Errorf("%w: args", ErrHubCommandInvalid)
		}
	}
	pauses, err := e.store.RunPauses(ctx, []string{c.SessionID})
	if err != nil {
		return HubCommandResult{}, err
	}
	view, err := s.hubAgent(ctx, c.SessionID, "", "", pauses[c.SessionID])
	if err != nil {
		return HubCommandResult{}, err
	}
	if view.Live {
		if c.ExpectedRevision != view.Revision {
			return rejectedHub(c, ErrHubStaleRevision), ErrHubStaleRevision
		}
		if c.RunID != "" && view.RunID != "" && c.RunID != view.RunID {
			return rejectedHub(c, ErrHubStaleRevision), ErrHubStaleRevision
		}
	} else if c.ExpectedRevision != 0 {
		return rejectedHub(c, ErrHubStaleRevision), ErrHubStaleRevision
	}
	if view.Live && c.RunID == "" {
		// The command names the run it was formed against by its revision; carry the run id so a command
		// that reaches a replacement run is refused rather than applied to it.
		c.RunID = view.RunID
	}
	switch c.Verb {
	case HubVerbPause:
		return s.hubDebug(ctx, c, "pause")
	case HubVerbResume:
		return s.hubDebug(ctx, c, "continue")
	case HubVerbStep:
		action := "step_model"
		switch args.Unit {
		case "", "model":
		case "agent":
			action = "step_agent"
		case "tool":
			action = "step_tool"
		case "turn":
			action = "step_turn"
		default:
			return rejectedHub(c, fmt.Errorf("%w: step unit %q", ErrHubCommandInvalid, args.Unit)), ErrHubCommandInvalid
		}
		return s.hubDebug(ctx, c, action)
	case HubVerbCancel:
		if !view.Live {
			return rejectedHub(c, ErrHubRunInactive), ErrHubRunInactive
		}
		if !s.Cancel(c.SessionID) {
			return rejectedHub(c, ErrHubRunInactive), ErrHubRunInactive
		}
		return s.hubResult(ctx, c, HubAccepted, "cancel requested")
	case HubVerbStart:
		return s.hubStart(ctx, c, args)
	case HubVerbRetry:
		return s.hubRetry(ctx, c)
	case HubVerbSteer:
		if strings.TrimSpace(args.Text) == "" {
			return rejectedHub(c, fmt.Errorf("%w: steer needs text", ErrHubCommandInvalid)), ErrHubCommandInvalid
		}
		if _, err := e.SteerAgent(ctx, c.SessionID, c.RunID, args.Text, c.CommandID); err != nil {
			return rejectedHub(c, err), err
		}
		return s.hubResult(ctx, c, HubAccepted, "steer queued")
	case HubVerbHandoff:
		target, err := s.Handoff(ctx, HubHandoff{Source: c.SessionID, Target: args.Target, Agent: args.Agent, Note: args.Note, CommandID: c.CommandID})
		if err != nil {
			return rejectedHub(c, err), err
		}
		result, rerr := s.hubResult(ctx, c, HubAccepted, "handoff started")
		result.Target = target
		return result, rerr
	case HubVerbAttach, HubVerbDetach:
		// Observation is SubscribeRuntimeEvents plus View(cursor); these verbs exist so a UI can name the
		// intent, and never touch execution.
		return s.hubResult(ctx, c, HubAccepted, "observation is SubscribeRuntimeEvents with the view cursor")
	}
	return rejectedHub(c, fmt.Errorf("%w: unknown verb", ErrHubCommandInvalid)), ErrHubCommandInvalid
}

// hubDebug routes a pause/resume/step through the run's DebugControl, the same API the debugger host
// uses, so the boundary semantics stay in one place.
func (s *RunSupervisor) hubDebug(ctx context.Context, c HubCommand, action string) (HubCommandResult, error) {
	if _, err := s.engine.AgentDebugSnapshot(ctx, c.SessionID); err != nil {
		return HubCommandResult{}, err
	}
	_, err := s.engine.ControlAgentDebug(ctx, AgentDebugCommand{
		SessionID: c.SessionID, RunID: c.RunID, Revision: c.ExpectedRevision, Action: action,
	})
	switch {
	case errors.Is(err, agent.ErrDebugStale):
		return rejectedHub(c, ErrHubStaleRevision), ErrHubStaleRevision
	case errors.Is(err, agent.ErrDebugState):
		return rejectedHub(c, ErrHubRunInactive), ErrHubRunInactive
	case err != nil:
		return rejectedHub(c, err), err
	}
	return s.hubResult(ctx, c, HubAccepted, "")
}

func (s *RunSupervisor) hubStart(ctx context.Context, c HubCommand, args HubArgs) (HubCommandResult, error) {
	if s.Active(c.SessionID) {
		return rejectedHub(c, ErrRunHeld), ErrRunHeld
	}
	opts := AgentOptions{Input: args.Input, Agent: args.Agent, Policy: args.Policy, MaxSteps: args.MaxSteps, Interactive: true}
	if args.Input == "" {
		opts.Interactive = true
	}
	if _, err := s.Start(ctx, c.SessionID, opts); err != nil {
		return rejectedHub(c, err), err
	}
	return s.hubResult(ctx, c, HubAccepted, "started")
}

// hubRetry starts a new run from the session's last safe state with the options of its previous run. No
// tool is replayed: a committed tool result is transcript and execution, not a step to redo.
func (s *RunSupervisor) hubRetry(ctx context.Context, c HubCommand) (HubCommandResult, error) {
	if s.Active(c.SessionID) {
		return rejectedHub(c, ErrRunHeld), ErrRunHeld
	}
	s.mu.RLock()
	prior, ok := s.lastRuns[c.SessionID]
	s.mu.RUnlock()
	if !ok {
		return rejectedHub(c, errors.New("no recorded run to retry; start the session with options first")),
			fmt.Errorf("%w: no recorded run to retry", ErrHubRejected)
	}
	opts := prior.opts
	opts.Input = ""
	opts.subagents, opts.tasks, opts.ownerRun = nil, nil, nil
	opts.Debug = nil
	if _, err := s.Start(ctx, c.SessionID, opts); err != nil {
		return rejectedHub(c, err), err
	}
	s.mu.Lock()
	if run := s.runs[c.SessionID]; run != nil {
		run.retryOf = prior.runID
	}
	s.mu.Unlock()
	e := s.engine
	e.events.publish("agent.retry", c.SessionID, map[string]any{"retryOf": prior.runID})
	_, _ = e.store.AppendCheckpoint(context.Background(), c.SessionID, "agent.retry", map[string]any{"retryOf": prior.runID})
	return s.hubResult(ctx, c, HubAccepted, "retry started")
}

// hubResult reads the agent back after a command, so the caller always gets the state its command
// produced.
func (s *RunSupervisor) hubResult(ctx context.Context, c HubCommand, status, note string) (HubCommandResult, error) {
	pauses, err := s.engine.store.RunPauses(ctx, []string{c.SessionID})
	if err != nil {
		return HubCommandResult{}, err
	}
	view, err := s.hubAgent(ctx, c.SessionID, "", "", pauses[c.SessionID])
	if err != nil {
		return HubCommandResult{}, err
	}
	if view.Role == "" {
		view.Role = "main"
	}
	return HubCommandResult{Status: status, Verb: c.Verb, CommandID: c.CommandID, Agent: view, Note: note}, nil
}

func rejectedHub(c HubCommand, err error) HubCommandResult {
	return HubCommandResult{Status: HubRejected, Verb: c.Verb, CommandID: c.CommandID, Error: err.Error()}
}

// SteerAgent queues one free-text instruction for the session's run. The run applies it at the next
// request window it builds, which includes the window behind a BeforeModelCall pause (F-STEER-1); the
// text is durable and ordered with the other host edits. Only the text is stored: no reasoning, no model
// output.
func (e *Engine) SteerAgent(ctx context.Context, sessionID, runID, text, commandID string) (SteerMessage, error) {
	if err := e.runtimeEventAccess(ctx, sessionID); err != nil {
		return SteerMessage{}, err
	}
	if strings.TrimSpace(text) == "" {
		return SteerMessage{}, errors.New("a steer message needs text")
	}
	message, err := e.store.QueueSteerMessage(ctx, sessionID, runID, commandID, text)
	if err != nil {
		return SteerMessage{}, err
	}
	e.events.publish("agent.steer.queued", sessionID, map[string]any{
		"steerSeq": message.Seq, "commandId": message.CommandID, "bytes": len(message.Body), "runId": runID,
	})
	return message, nil
}

// SteerMessage is one queued free-text instruction.
type SteerMessage = session.SteerMessage

// AckWorkerMessages acknowledges a recipient's pushed messages up to and including through, so an
// unacked redelivery stops. A message stays readable with worker_inbox.
func (e *Engine) AckWorkerMessages(ctx context.Context, root, recipient string, through int64) (int, error) {
	if err := e.runtimeEventAccess(ctx, root); err != nil {
		return 0, err
	}
	n, err := e.store.AckWorkerMessages(ctx, root, recipient, through)
	if err == nil {
		e.events.publish("worker.message.acked", root, map[string]any{"workerId": recipient, "through": through, "count": n})
	}
	return n, err
}

// WakeAgent starts an idle session with input, the host's explicit choice to wake a worker whose queue
// holds work. It never wakes a session that is closed or fenced: Start refuses both.
func (s *RunSupervisor) Wake(ctx context.Context, sessionID, input string) (SessionInfo, error) {
	return s.Start(ctx, sessionID, AgentOptions{Input: input})
}

// sessionUsage reads the stored usage of a session row.
func sessionUsage(row session.Session) *HubSpend {
	if len(row.Usage) == 0 {
		return nil
	}
	var usage struct {
		PromptTokens     int64 `json:"promptTokens"`
		CompletionTokens int64 `json:"completionTokens"`
		TotalTokens      int64 `json:"totalTokens"`
		Requests         int64 `json:"requests"`
	}
	if err := json.Unmarshal(row.Usage, &usage); err != nil {
		return nil
	}
	return &HubSpend{Requests: usage.Requests, PromptTokens: usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens, TotalTokens: usage.TotalTokens}
}

// trackedRun returns the Engine's live run record of the session, nil when none.
func (e *Engine) trackedRun(sessionID string) *trackedRun {
	e.runsMu.Lock()
	defer e.runsMu.Unlock()
	return e.runs[sessionID]
}

// rememberRun records the session's run for the retry verb. The transient control objects of the Start
// (its subagent controller, task starter and owner run) belong to that invocation and are dropped, so a
// retry cannot resume a dead parent's children.
func (s *RunSupervisor) rememberRun(sessionID, runID string, opts AgentOptions) {
	if runID == "" {
		return
	}
	opts.subagents, opts.tasks, opts.ownerRun = nil, nil, nil
	opts.Debug = nil
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastRuns == nil {
		s.lastRuns = map[string]hubRunRecord{}
	}
	if _, exists := s.lastRuns[sessionID]; !exists && len(s.lastRuns) >= maxHubRunRecords {
		for other := range s.lastRuns {
			delete(s.lastRuns, other)
			break
		}
	}
	s.lastRuns[sessionID] = hubRunRecord{runID: runID, opts: opts}
}

// noteDebugState projects a pause into durable state and the event stream: paused is persisted so a
// restart shows interrupted(paused), and the events let a subscriber follow the transitions without
// polling the snapshot.
func (e *Engine) noteDebugState(sessionID, runID string, snapshot agent.DebugSnapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	switch snapshot.State {
	case "paused":
		err := e.store.RecordRunPause(ctx, session.RunPause{
			SessionID: sessionID, RunID: runID, Reason: snapshot.Reason,
			Boundary: snapshot.Boundary.Event, Step: snapshot.Boundary.Step,
		})
		if err != nil {
			e.events.publish("hub.state.error", sessionID, map[string]any{"phase": "pause", "error": err.Error()})
			return
		}
		e.events.publish("agent.paused", sessionID, map[string]any{
			"runId": runID, "reason": snapshot.Reason, "boundary": snapshot.Boundary.Event,
			"step": snapshot.Boundary.Step, "revision": snapshot.Revision,
		})
	case "running", "closed":
		if err := e.store.ClearRunPause(ctx, sessionID); err != nil {
			e.events.publish("hub.state.error", sessionID, map[string]any{"phase": "resume", "error": err.Error()})
			return
		}
		if snapshot.State == "running" {
			e.events.publish("agent.resumed", sessionID, map[string]any{"runId": runID, "revision": snapshot.Revision})
		}
	}
}
