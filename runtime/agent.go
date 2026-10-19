package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

type AgentOptions struct {
	// Input is a user message appended to the session's transcript under the run
	// lock, after Reconcile. It is the only way a host adds to an existing
	// transcript.
	Input string `json:"input,omitempty"`
	// ModelRef selects the session's model. When it differs from the session's pinned model it
	// re-pins the session (rule model.user); empty keeps the pinned model, or pins the
	// configured default on a session that has none.
	ModelRef config.ModelRef `json:"modelRef,omitempty"`
	// Effort overrides the configured default reasoning effort for this run: "" (the default),
	// none, low, medium, high, max, or auto (classify this turn before its first request; the
	// resolved level then acts as the run's default and escalation still raises it).
	Effort              config.Effort   `json:"effort,omitempty"`
	Policy              string          `json:"policy,omitempty"`
	MaxSteps            int             `json:"maxSteps,omitempty"`
	MaxToolCallsPerStep int             `json:"maxToolCallsPerStep,omitempty"`
	ToolAllowlist       []string        `json:"toolAllowlist,omitempty"`
	ToolDenylist        []string        `json:"toolDenylist,omitempty"`
	SystemPrompt        string          `json:"systemPrompt,omitempty"`
	Workflow            json.RawMessage `json:"workflow,omitempty"`
	WorkUnits           json.RawMessage `json:"workUnits,omitempty"`
	MaxTokens           int             `json:"maxTokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
	// Interactive marks a conversation turn (ADR-0006 D3): without a host workflow, a text answer ends
	// the run completed and leaves open work units and checks to the thread's resolution; a run that
	// changed the workspace still has its required checks decided. The default, false, keeps the full
	// completion gate. Subagent runs are never interactive.
	Interactive bool `json:"interactive,omitempty"`
	// Delegation grants the start_task tool (ADR-0006 D4): "" or "none" (the default) registers none,
	// "propose" records proposals the user starts with RunSupervisor.StartProposedTask, "isolated"
	// starts execution tasks directly and needs RunSupervisor.Start. It is independent of Policy: a
	// read-only conversation starts write-capable tasks only with this grant. Children never hold it.
	Delegation string `json:"delegation,omitempty"`

	subagents        toolset.SubagentController
	tasks            agent.TaskStarter
	parentSessionID  string
	disableSubagents bool
	disableMCP       bool
}

type AgentResult struct {
	SessionID   string         `json:"sessionId"`
	Status      string         `json:"status"`
	Final       string         `json:"final,omitempty"`
	Usage       protocol.Usage `json:"usage"`
	WaitingGate bool           `json:"waitingGate,omitempty"`
	Workflow    any            `json:"workflow,omitempty"`
}

// RunAgent runs the agent loop to its exit on the calling goroutine. It holds the
// session's run lock for the whole call and returns ErrRunHeld, having written
// nothing, when another Engine holds it. RunSupervisor.Start is the asynchronous
// entry point with the same contract.
func (e *Engine) RunAgent(ctx context.Context, sessionID string, opts AgentOptions) (AgentResult, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return AgentResult{}, err
	}
	defer lock.Release()
	if opts, err = e.constrainExecutionChild(ctx, sessionID, opts); err != nil {
		return AgentResult{}, err
	}
	_, lease, err := e.leaseWorkspace(ctx, sessionID)
	if err != nil {
		return AgentResult{}, err
	}
	defer lease.Release()
	run, err := e.prepareRun(ctx, sessionID, opts)
	if err != nil {
		return AgentResult{}, err
	}
	return run.execute(ctx)
}

// preparedRun is a Start whose pre-run writes are done: the loop only has to execute.
type preparedRun struct {
	loop      agent.Loop
	loopOpts  agent.Options
	sessionID string
	// info is the session as the Start transaction committed it: running, with the
	// new identity and the previous outcome cleared.
	info SessionInfo
}

func (r *preparedRun) execute(ctx context.Context) (AgentResult, error) {
	result, err := r.loop.Run(ctx, r.sessionID, r.loopOpts)
	return AgentResult{
		SessionID:   result.SessionID,
		Status:      result.Status,
		Final:       result.Final,
		Usage:       result.Usage,
		WaitingGate: result.WaitingGate,
		Workflow:    result.Workflow,
	}, err
}

// prepareRun is the Start sequence after the run lock is held:
//
//	Reconcile → validate and build → one transaction (Input, identity, running)
//
// Everything that can reject the Start happens before the transaction, and the
// transaction is atomic, so a rejected or failed Start leaves no new transcript
// row and the session row as it was. The loop starts on a session that is already
// running.
func (e *Engine) prepareRun(ctx context.Context, sessionID string, opts AgentOptions) (*preparedRun, error) {
	if _, err := e.store.Reconcile(ctx, sessionID); err != nil {
		return nil, err
	}
	if opts.Input != "" && strings.TrimSpace(opts.Input) == "" {
		return nil, errors.New("input must not be blank")
	}
	row, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	plan, err := e.planModels(opts.ModelRef, config.ModelRef(row.ModelRef), opts.Effort)
	if err != nil {
		return nil, err
	}
	state, err := agentStore{store: e.store}.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	delegation, err := normalizeDelegation(opts.Delegation)
	if err != nil {
		return nil, err
	}
	if opts.parentSessionID != "" {
		// A child's authority is at most its parent's grant, and no child delegates further (I10).
		delegation = agent.DelegationNone
	}
	if delegation == agent.DelegationIsolated && opts.tasks == nil {
		return nil, errors.New("delegation isolated starts tasks that outlive the run: start the run with RunSupervisor.Start")
	}
	// The seeds are validated here, before anything is written; the task results of the parent's
	// execution children are read here and commit with their cursor in the Start transaction.
	taskState, results, err := e.startTaskState(ctx, sessionID, opts)
	if err != nil {
		return nil, err
	}
	policy := opts.Policy
	if policy == "" {
		policy = toolset.PolicyReadOnly
	}
	agentWorkspace, err := e.agentWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	runner := e.shell
	if agentWorkspace != e.workspace {
		runner = shell.New(agentWorkspace)
	}
	var lspManager *lsp.Manager
	e.lspMu.Lock()
	if candidate := e.lspManagers[agentWorkspace]; candidate != nil && candidate.Status().Running {
		lspManager = candidate
	}
	e.lspMu.Unlock()
	mcpRegistry := e.mcpRegistry
	if opts.disableMCP {
		mcpRegistry = nil
	}
	tools, err := toolset.New(toolset.Options{
		Workspace: agentWorkspace,
		Policy:    policy,
		Shell:     runner,
		LSP:       lspManager,
		MCP:       mcpRegistry,
		Subagents: opts.subagents,
	})
	if err != nil {
		return nil, err
	}
	ev, err := e.sessionEvaluator(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	checks := runChecks{ev: ev, specs: plan.checks}
	// Execution tasks and read-only subagents are never interactive (ADR-0006 D3): a child run works its
	// goal to the completion gate whatever its options say.
	interactive := opts.Interactive && opts.parentSessionID == ""
	systemPrompt := opts.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = agent.SystemPrompt(interactive, policyAuthority(policy))
	}
	// Auto effort is resolved once per Start, under the run lock and before the Start transaction:
	// the previous request and last reply are in the transcript, and a cancelled judge call leaves
	// no new row or running session behind.
	runEffort := opts.Effort
	if runEffort == "" {
		runEffort = plan.effort
	}
	var auto *autoResolution
	if runEffort == config.EffortAuto {
		resolved, err := e.resolveAutoEffort(ctx, sessionID, opts.Input, plan.auto)
		if err != nil {
			return nil, err
		}
		auto = &resolved
	}
	effortDefault, effortOverride, effortCeiling := plan.effort, opts.Effort, config.Effort("")
	decisions := []agent.Decision{plan.decision}
	if auto != nil {
		effortDefault, effortOverride = auto.level, ""
		effortCeiling = auto.ceiling
		decisions = append(decisions, auto.decision)
	}
	loopOpts := agent.Options{
		MaxSteps:            opts.MaxSteps,
		MaxToolCallsPerStep: opts.MaxToolCallsPerStep,
		ToolAllowlist:       opts.ToolAllowlist,
		ToolDenylist:        opts.ToolDenylist,
		SystemPrompt:        systemPrompt,
		MaxTokens:           opts.MaxTokens,
		Temperature:         opts.Temperature,
		Session:             plan.session,
		Failover:            plan.failover,
		EffortDefault:       effortDefault,
		Escalation:          plan.escalation,
		Effort:              effortOverride,
		EffortAutoCeiling:   effortCeiling,
		Decisions:           decisions,
		Interactive:         interactive,
		Delegation:          delegation,
	}

	if e.skillRegistry != nil {
		skillPrompt, loadedSkills, skillErr := e.skillRegistry.Prompt()
		if skillErr != nil {
			e.events.publish("skills.error", sessionID, map[string]any{"error": skillErr.Error()})
		} else {
			loopOpts.AdditionalSystemPrompt = skillPrompt
			if len(loadedSkills) > 0 {
				ids := make([]string, 0, len(loadedSkills))
				for _, skill := range loadedSkills {
					ids = append(ids, skill.ID)
				}
				e.events.publish("skills.loaded", sessionID, map[string]any{"skills": ids})
			}
		}
	}

	// Start's one durable write: Input, the session model identity, the Start's Task State version and
	// the running transition commit together or not at all (§3.2). Nothing above it wrote.
	start := session.RunStart{Provider: &plan.resolved.ProviderID, Model: &plan.resolved.ModelID, TaskState: taskState, Results: results}
	if agent.WritesModelRef(plan.rule) {
		ref := string(plan.session)
		start.ModelRef = &ref
	}
	if opts.Input != "" {
		start.Input = &protocol.Message{Role: "user", Content: opts.Input}
	}
	loopOpts.Resumed = state.Status == "interrupted"
	committed, seqs, err := e.store.BeginRun(ctx, sessionID, start)
	if err != nil {
		return nil, err
	}
	for i, seq := range seqs {
		switch {
		case i < len(results):
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user", "origin": protocol.OriginTaskResult})
		case start.Input != nil:
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user"})
		default:
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user", "origin": protocol.OriginResume})
		}
	}
	if len(results) > 0 {
		e.events.publish("task.delivered", sessionID, map[string]any{"count": len(results)})
	}

	return &preparedRun{
		sessionID: sessionID,
		info:      sessionInfo(committed),
		loopOpts:  loopOpts,
		loop: agent.Loop{
			Models:   plan.models,
			Health:   e.health,
			Store:    agentStore{store: e.store},
			Tools:    tools,
			Evidence: e.runEvidence(sessionID, checks),
			Checks:   checks,
			Tasks:    taskStarterFor(delegation, opts.tasks),
			Emit: func(event agent.Event) {
				e.events.publish(event.Type, event.SessionID, event.Data)
				_ = e.store.Journal(context.Background(), event.Type, map[string]any{
					"sessionId": event.SessionID,
					"data":      event.Data,
				})
				if opts.parentSessionID != "" {
					data := subagentEventData(event.Type, event.SessionID, event.Data)
					e.events.publish("subagent.event", opts.parentSessionID, data)
					_ = e.store.Journal(context.Background(), "subagent.event", map[string]any{
						"sessionId": opts.parentSessionID,
						"data":      data,
					})
				}
			},
		},
	}, nil
}

// policyAuthority states what a run under policy may do, for the default system prompt. toolset.New has
// already rejected any other policy.
func policyAuthority(policy string) string {
	switch policy {
	case toolset.PolicyFull:
		return "This run can read and edit files in the workspace and run shell commands."
	case toolset.PolicyWorkspace:
		return "This run can read and edit files in the workspace; it cannot run shell commands."
	default:
		return "This run is read-only: it can read, search and inspect the workspace but cannot change files or run commands. Describe a needed change instead of making it."
	}
}

// WorkflowSummary returns the summary of the workflow in the session's newest Task State, nil when it
// has none.
func (e *Engine) WorkflowSummary(ctx context.Context, sessionID string) (any, error) {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	wf, err := state.RestoreWorkflow()
	if err != nil || wf == nil {
		return nil, err
	}
	return wf.Summary(), nil
}

// ApproveWorkflowGate approves a human gate of the session's workflow. It takes
// the session's run lock and returns ErrRunHeld while a run holds it. It writes
// the next Task State version (reason user) only: the session's status stays
// (waiting_gate) and the host continues with a Start.
func (e *Engine) ApproveWorkflowGate(ctx context.Context, sessionID, gateID, actor string) (any, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	wf, err := state.RestoreWorkflow()
	if err != nil {
		return nil, err
	}
	if wf == nil {
		return nil, errNoWorkflow
	}
	transition, err := wf.Approve(gateID, actor)
	if err != nil {
		return nil, err
	}
	state.SetWorkflow(wf)
	if _, err := e.putTaskState(ctx, sessionID, taskstate.ReasonUser, state, 0); err != nil {
		return nil, err
	}
	e.events.publish("workflow.approved", sessionID, map[string]any{
		"gate": gateID, "actor": actor, "transition": transition, "workflow": wf.Summary(),
	})
	return wf.Summary(), nil
}

func rawMessagePtr(value []byte) *json.RawMessage {
	raw := json.RawMessage(append([]byte(nil), value...))
	return &raw
}

type agentStore struct {
	store *session.Store
}

func (s agentStore) Get(ctx context.Context, id string) (agent.SessionState, error) {
	value, err := s.store.Get(ctx, id)
	if err != nil {
		return agent.SessionState{}, err
	}
	usage := protocol.Usage{}
	if len(value.Usage) > 0 {
		_ = json.Unmarshal(value.Usage, &usage)
	}
	final := ""
	if value.Final != nil {
		final = *value.Final
	}
	return agent.SessionState{ID: value.ID, Goal: value.Goal, Status: value.Status, Final: final, Usage: usage}, nil
}

func (s agentStore) Transcript(ctx context.Context, id string, fromSeq int64, fn func(int64, protocol.Message) error) error {
	return s.store.EachMessageFrom(ctx, id, fromSeq, func(item session.Message) error {
		var message protocol.Message
		if err := json.Unmarshal(item.JSON, &message); err != nil {
			return err
		}
		if message.Role == "" {
			message.Role = item.Role
		}
		return fn(item.Seq, message)
	})
}

func (s agentStore) AppendMessage(ctx context.Context, id string, message protocol.Message, change *agent.TaskStateChange) (int64, int64, error) {
	write, err := agentTaskStateChange(change)
	if err != nil {
		return 0, 0, err
	}
	return s.store.AppendMessageTaskState(ctx, id, message.Role, message, write)
}

func (s agentStore) AppendStep(ctx context.Context, id string, step int64, value any) error {
	return s.store.AppendStep(ctx, id, step, value)
}

func (s agentStore) AppendCheckpoint(ctx context.Context, id, reason string, value any) error {
	_, err := s.store.AppendCheckpoint(ctx, id, reason, value)
	return err
}

func (s agentStore) NextStep(ctx context.Context, id string) (int64, error) {
	return s.store.NextStep(ctx, id)
}

func (s agentStore) TaskState(ctx context.Context, id string) (taskstate.State, error) {
	row, err := s.store.LatestTaskState(ctx, id)
	if err != nil {
		return taskstate.State{}, err
	}
	return taskstate.Decode(row.Version, row.JSON)
}

func (s agentStore) InboxEdits(ctx context.Context, id string) ([]taskstate.QueuedEdit, error) {
	rows, err := s.store.InboxEdits(ctx, id)
	if err != nil {
		return nil, err
	}
	return queuedEdits(rows), nil
}

func (s agentStore) PutTaskState(ctx context.Context, id string, change agent.TaskStateChange) (int64, error) {
	write, err := agentTaskStateChange(&change)
	if err != nil {
		return 0, err
	}
	return s.store.PutTaskState(ctx, id, *write)
}

func (s agentStore) Update(ctx context.Context, id string, patch agent.SessionPatch) error {
	value := session.Patch{
		Status:     patch.Status,
		Final:      patch.Final,
		ClearFinal: patch.ClearFinal,
		ClearError: patch.ClearError,
	}
	if patch.Usage != nil {
		raw, err := json.Marshal(patch.Usage)
		if err != nil {
			return err
		}
		value.Usage = rawMessagePtr(raw)
	}
	if patch.Error != nil {
		raw, err := json.Marshal(patch.Error)
		if err != nil {
			return err
		}
		value.Error = rawMessagePtr(raw)
	}
	return s.store.Update(ctx, id, value)
}
