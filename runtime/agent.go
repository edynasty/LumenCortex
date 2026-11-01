package runtime

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/repository"
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
	// Agent selects the catalog agent (ADR-0009). When it differs from the session's pinned agent it
	// re-pins the session (rule agent.user) and, unless ModelRef names a model, re-pins the model to
	// the agent's first one; empty keeps the pinned agent, or pins "agent" on a session that has
	// none. The agent's ceilings cap Policy and Delegation, and its instructions are appended to the
	// system prompt. Child runs ignore it: their agent is fixed when they are created.
	Agent string `json:"agent,omitempty"`
	// ModelRef selects the session's model. When it differs from the session's pinned model it
	// re-pins the session (rule model.user); empty keeps the pinned model, or pins the agent's first
	// model (else the configured default) on a session that has none.
	ModelRef config.ModelRef `json:"modelRef,omitempty"`
	// Effort overrides the configured default reasoning effort for this run: "" (the default),
	// none, minimal, low, medium, high, xhigh, max, or auto (classify this turn before its first request; the
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
	// Deadline is the run's absolute deadline (ADR-0019 §2); nil means none. The model sees the time
	// left, tools are bounded so that a reply still fits, and the run ends interrupted with kind
	// "deadline" when it passes. A read-only subagent inherits the time by which its spawning tool call
	// must be done. DeadlineReserveSeconds is the time before the deadline from which the model is asked
	// to deliver; 0 takes a tenth of the budget within 30-120 s.
	Deadline               *time.Time `json:"deadline,omitempty"`
	DeadlineReserveSeconds int        `json:"deadlineReserveSeconds,omitempty"`

	subagents toolset.SubagentController
	// childAgent is a child run's default agent: explore for a read-only subagent, worker for an
	// execution task. "" for a thread.
	childAgent       string
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
	if opts, err = e.constrainChild(ctx, sessionID, opts); err != nil {
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
	engine    *Engine
	loop      agent.Loop
	loopOpts  agent.Options
	sessionID string
	// info is the session as the Start transaction committed it: running, with the
	// new identity and the previous outcome cleared.
	info SessionInfo
}

// execute runs the loop as a tracked run of the Engine, so that a revocation in this process can
// cancel it and wait for it to end (T3-S1).
func (r *preparedRun) execute(ctx context.Context) (AgentResult, error) {
	ctx, done := r.engine.trackRun(ctx, r.sessionID)
	defer done()
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

// newRunID is a 128-bit random run identity in lower-case hex.
func newRunID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("run id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// prepareRun is the Start sequence after the run lock is held:
//
//	Reconcile → run id → validate and build → one transaction (Input, identity, running)
//
// Everything that can reject the Start happens before the transaction, and the
// transaction is atomic, so a rejected or failed Start leaves no new transcript
// row and the session row as it was. The loop starts on a session that is already
// running.
//
// The run's identity (runID) is drawn right after Reconcile and before anything that can call a model
// (the Auto-effort judge): a fresh 128-bit random value per Start, kept in memory and passed to the
// loop as agent.Options.RunID. It is never written to sessions; request and execution rows carry it
// as a nullable column, and a restart, a takeover or a new Start is a new run.
func (e *Engine) prepareRun(ctx context.Context, sessionID string, opts AgentOptions) (*preparedRun, error) {
	if _, err := e.store.Reconcile(ctx, sessionID); err != nil {
		return nil, err
	}
	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	// The events the loop publishes carry no run id; the diagnostics bridge attaches this one so a
	// trace can group them by run (diagnostics.go).
	e.noteRunStarted(sessionID, runID)
	if opts.Input != "" && strings.TrimSpace(opts.Input) == "" {
		return nil, errors.New("input must not be blank")
	}
	row, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	delegation, err := normalizeDelegation(opts.Delegation)
	if err != nil {
		return nil, err
	}
	policy := opts.Policy
	if policy == "" {
		policy = toolset.PolicyReadOnly
	}
	child := opts.parentSessionID != ""
	defaultAgent, parentAgent := config.AgentDefault, ""
	if child {
		// A child of unknown kind gets the read-only explore agent: the least authority.
		defaultAgent = cmp.Or(opts.childAgent, config.AgentExplore)
		// The parent's agent only supplies a list to inherit; without a parent row the child falls
		// back through failover.
		parent, err := e.store.Get(ctx, opts.parentSessionID)
		if err != nil && !errors.Is(err, session.ErrNotFound) {
			return nil, err
		}
		parentAgent = parent.AgentID
	}
	plan, err := e.planModels(startRequest{
		model: opts.ModelRef, storedModel: config.ModelRef(row.ModelRef), effort: opts.Effort,
		agent: strings.TrimSpace(opts.Agent), storedAgent: row.AgentID, defaultAgent: defaultAgent, child: child, parentAgent: parentAgent,
		grant: agent.AgentGrant{Policy: policy, Delegation: delegation},
	})
	if err != nil {
		return nil, err
	}
	// The agent's ceilings only narrow what the host granted (ADR-0009).
	policy, delegation = plan.grant.Policy, plan.grant.Delegation
	state, err := agentStore{store: e.store}.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if child {
		// A child's authority is at most its parent's grant, and no child delegates further (I10).
		delegation = agent.DelegationNone
	}
	if delegation == agent.DelegationIsolated && opts.tasks == nil {
		return nil, errors.New("delegation isolated starts tasks that outlive the run: start the run with RunSupervisor.Start")
	}
	agentWorkspace, err := e.agentWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	ev, err := e.sessionEvaluator(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	checks := runChecks{engine: e, ev: ev, specs: plan.checks, store: e.store, sessionID: sessionID}
	// The seeds are validated here, before anything is written; the task results of the parent's
	// execution children are read here and commit with their cursor in the Start transaction. The
	// thread's constraints are decided here too, by the checks bound to them, in the session's workspace
	// (plan T4-S3): that runs commands, so only once the seeds were accepted, and the decisions commit
	// with the Start's Task State version. A pass of those checks counts for the rest of this run.
	taskState, results, err := e.startTaskState(ctx, sessionID, opts, func(ctx context.Context, state *taskstate.State) error {
		return e.decideConstraints(ctx, runID, state, checks)
	})
	if err != nil {
		return nil, err
	}
	runner := e.shell
	if agentWorkspace != e.workspace {
		runner = shell.New(agentWorkspace)
	}
	// The workspace's manager exists whether or not a server is running: the toolset offers the lsp_*
	// tools only when a server can serve the workspace, and the manager starts it on the first call.
	lspManager, err := e.lspManagerFor(agentWorkspace)
	if err != nil {
		lspManager = nil
	}
	mcpRegistry := e.mcpRegistry
	if opts.disableMCP {
		mcpRegistry = nil
	}
	// Execution tasks and read-only subagents are never interactive (ADR-0006 D3): a child run works its
	// goal to the completion gate whatever its options say.
	interactive := opts.Interactive && opts.parentSessionID == ""
	systemPrompt := opts.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = agent.SystemPrompt(interactive, policyAuthority(policy))
	}
	// The system prefix is assembled once, from this Start's configuration: the base prompt, the
	// instruction files (project ones only in a trusted workspace), the skills catalog and the
	// agent's instructions (ADR-0013). Engine.EffectivePrompt reports the same layers for the same
	// inputs. It comes before the toolset because read_skill serves exactly the skills it lists.
	prompt := assemblePrompt(promptInput{
		workspace: e.workspace, globalDir: plan.globalDir, trusted: plan.trusted,
		base:              systemPrompt,
		agentID:           plan.agentID,
		agentSource:       plan.agentSource,
		agentInstructions: plan.instructions,
		skills:            e.skillRegistry,
	})
	// A wake-up starts a later run on this session. A child run (a subagent or an execution task) has a
	// lifecycle of its own that its parent owns, so it gets no wake-up tools: the thread does.
	var wakeupBinding toolset.WakeupController
	if opts.parentSessionID == "" {
		wakeupBinding = e.wakeupsFor(sessionID, policy)
	}
	tools, err := toolset.New(toolset.Options{
		Workspace: agentWorkspace,
		Policy:    policy,
		Shell:     runner,
		LSP:       lspManager,
		MCP:       mcpRegistry,
		Subagents: opts.subagents,
		// Children get no spawn tool, so the list matters only to a thread's run.
		SubagentAgents: plan.spawnAgents,
		Threads:        threadReader{engine: e, sessionID: sessionID, workspace: agentWorkspace},
		Skills:         newSkillReader(prompt.Skills.Included),
		Notebook:       newNotebookAccess(e, sessionID, runID),
		Protected:      e.protected,
		ShellTimeout:   time.Duration(e.executionLimits().ShellTimeoutSeconds) * time.Second,
		// The run's background job queue: its session, its run id and the workspace its commands run in.
		// The completion gate and the run's end use the same binding through the Engine (ADR-0019 §3).
		Jobs:    e.runJobs(sessionID, runID, agentWorkspace),
		Wakeups: wakeupBinding,
		// A tool's own log lines name the session and the run they belong to.
		Logger: e.log.With("session_id", sessionID, "run_id", runID),
	})
	if err != nil {
		return nil, err
	}
	// Side-effect tools are snapshotted where they act: the run's workspace, the worktree of a session
	// that owns one (plan T1-S4).
	snapshots, err := repository.New(agentWorkspace)
	if err != nil {
		return nil, err
	}
	// A revocation that reached the session is projected here, under the run lock and before anything
	// is admitted for this Start (the auto judge included): the run withholds the rows that carry
	// revoked content, or the Start is refused when the thread cannot be projected.
	withheld, err := e.projectRun(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	// Auto effort is resolved once per Start, under the run lock and before the Start transaction:
	// the previous request and last reply are in the transcript, and a cancelled judge call leaves
	// no new row or running session behind.
	effortDefault, effortOverride, effortCeiling := plan.effort, opts.Effort, config.Effort("")
	decisions := []agent.Decision{plan.agentDecision, plan.decision}
	var candidateEfforts map[config.ModelRef]agent.CandidateEffort
	if opts.Effort == "" {
		candidateEfforts = make(map[config.ModelRef]agent.CandidateEffort, len(plan.effortFor))
		for ref, e := range plan.effortFor {
			candidateEfforts[ref] = agent.CandidateEffort{Default: e}
		}
	}
	if plan.needsAuto(opts.Effort) {
		auto, err := e.resolveAutoEffort(ctx, sessionID, runID, opts.Input, plan.auto)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, auto.decision)
		if opts.Effort == config.EffortAuto {
			// An auto override applies the one level to every candidate.
			effortOverride, effortDefault, effortCeiling, candidateEfforts = "", auto.level, auto.ceiling, nil
		}
		if effortDefault == config.EffortAuto {
			effortDefault, effortCeiling = auto.level, auto.ceiling
		}
		for ref, c := range candidateEfforts {
			if c.Default == config.EffortAuto {
				candidateEfforts[ref] = agent.CandidateEffort{Default: auto.level, AutoCeiling: auto.ceiling}
			}
		}
	}
	if text := plan.instructions; strings.TrimSpace(text) != "" {
		decisions = append(decisions, instructionsDecision(text))
	}
	loopOpts := agent.Options{
		MaxSteps:            opts.MaxSteps,
		MaxToolCallsPerStep: opts.MaxToolCallsPerStep,
		ToolAllowlist:       opts.ToolAllowlist,
		ToolDenylist:        opts.ToolDenylist,
		SystemPrompt:        systemPrompt,
		Instructions:        plan.instructions,
		MaxTokens:           opts.MaxTokens,
		Temperature:         opts.Temperature,
		Session:             plan.session,
		Failover:            plan.failover,
		EffortDefault:       effortDefault,
		Escalation:          plan.escalation,
		Effort:              effortOverride,
		EffortAutoCeiling:   effortCeiling,
		CandidateEfforts:    candidateEfforts,
		Decisions:           decisions,
		Interactive:         interactive,
		Delegation:          delegation,
		TaskAgents:          plan.taskAgents,
		RunID:               runID,
		Withheld:            withheld,
		DeadlineReserve:     time.Duration(opts.DeadlineReserveSeconds) * time.Second,
	}
	if opts.Deadline != nil {
		loopOpts.Deadline = *opts.Deadline
	}

	loopOpts.SystemLayers = systemLayers(prompt.Layers)
	if prompt.SkillsErr != nil {
		e.events.publish("skills.error", sessionID, map[string]any{"error": prompt.SkillsErr.Error()})
	} else if len(prompt.Skills.Included) > 0 || len(prompt.Skills.Withheld) > 0 {
		data := map[string]any{"skills": skillIDs(prompt.Skills.Included)}
		if len(prompt.Skills.Withheld) > 0 {
			// A project skill is repository content: it is read only in a trusted workspace (ADR-0013).
			data["excluded"] = skillIDs(prompt.Skills.Withheld)
			data["reason"] = "untrusted workspace"
		}
		e.events.publish("skills.loaded", sessionID, data)
	}

	// Start's one durable write: Input, the session model identity, the Start's Task State version and
	// the running transition commit together or not at all (§3.2). Nothing above it wrote.
	start := session.RunStart{Provider: &plan.resolved.ProviderID, Model: &plan.resolved.ModelID, TaskState: taskState, Results: results}
	// The working notebooks of the session (ADR-0020) become one short index row in this Start: their
	// titles, versions and sections, never a body. A session without notebooks gets no row.
	if infos, err := e.store.ListNotebooks(ctx, sessionID); err != nil {
		return nil, err
	} else if row, ok := notebookIndexRow(infos); ok {
		start.Notices = append(start.Notices, row)
	}
	if agent.WritesModelRef(plan.rule) {
		ref := string(plan.session)
		start.ModelRef = &ref
	}
	if agent.WritesAgent(plan.agentRule) {
		start.AgentID = &plan.agentID
	}
	if opts.Input != "" {
		start.Input = &protocol.Message{Role: "user", Content: opts.Input}
	}
	loopOpts.Resumed = state.Status == "interrupted"
	if testHookBeforeBeginRun != nil {
		testHookBeforeBeginRun()
	}
	committed, seqs, err := e.store.BeginRun(ctx, sessionID, start)
	if err != nil {
		return nil, err
	}
	noticeFrom := len(seqs) - len(start.Notices)
	for i, seq := range seqs {
		switch {
		case i < len(results):
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user", "origin": protocol.OriginTaskResult})
		case i >= noticeFrom:
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user", "origin": protocol.OriginNotebook})
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
		engine:    e,
		sessionID: sessionID,
		info:      sessionInfo(committed),
		loopOpts:  loopOpts,
		loop: agent.Loop{
			Models:    plan.models,
			Health:    e.health,
			Store:     agentStore{store: e.store},
			Tools:     tools,
			Snapshots: snapshots,
			Evidence:  e.runEvidence(sessionID, checks),
			Checks:    checks,
			Tasks:     taskStarterFor(delegation, opts.tasks),
			Cite:      citeIn(agentWorkspace),
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
		return "This run can read and edit files in the workspace and run commands with bash."
	case toolset.PolicyWorkspace:
		return "This run can read and edit files in the workspace; it cannot run commands."
	default:
		return "This run is read-only: it can read, search and inspect the workspace, read Skills, use MCP tools marked read-only, and record notes and plan work units; it cannot change project files or run commands. Describe a needed change instead of making it."
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
	if _, err := e.putTaskState(ctx, sessionID, taskstate.ReasonUser, state, 0, nil); err != nil {
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

// Crash points for tests that kill the process (nil outside tests): the request is admitted but not
// yet sent, and the model has answered but the answer is not stored. A third point, mid-call, is a
// provider that never returns. For tools: the execution is admitted but the tool has not run, the
// tool has run but its result is not committed, and the result is committed. Mid-execution is a
// tool that never returns.
var (
	testHookAfterAdmit       func()
	testHookBeforeAnswer     func()
	testHookAfterExecAdmit   func()
	testHookBeforeToolCommit func()
	testHookAfterToolCommit  func()
	// testHookBeforeBeginRun runs after the Start decided and projected everything, just before the
	// Start transaction.
	testHookBeforeBeginRun func()
)

func (s agentStore) AdmitExecution(ctx context.Context, a session.ExecutionAdmission) error {
	err := s.store.AdmitExecution(ctx, a)
	if err == nil && testHookAfterExecAdmit != nil {
		testHookAfterExecAdmit()
	}
	return err
}

func (s agentStore) CommitToolResult(ctx context.Context, c agent.ToolResultCommit) (int64, int64, error) {
	write, err := agentTaskStateChange(c.Change)
	if err != nil {
		return 0, 0, err
	}
	if testHookBeforeToolCommit != nil {
		testHookBeforeToolCommit()
	}
	seq, version, err := s.store.CommitToolResult(ctx, session.ToolCommit{
		SessionID: c.SessionID, RunID: c.RunID, Message: c.Message, TaskState: write, Execution: c.Execution, Edges: c.Edges,
	})
	if err == nil && testHookAfterToolCommit != nil {
		testHookAfterToolCommit()
	}
	return seq, version, err
}

func (s agentStore) AdmitRequest(ctx context.Context, a session.RequestAdmission) (session.RequestEnvelope, error) {
	env, err := s.store.AdmitRequest(ctx, a)
	if err == nil && testHookAfterAdmit != nil {
		testHookAfterAdmit()
	}
	return env, err
}

func (s agentStore) AppendAnswered(ctx context.Context, sessionID, requestID string, message protocol.Message) (int64, error) {
	if testHookBeforeAnswer != nil {
		testHookBeforeAnswer()
	}
	return s.store.AppendAnswered(ctx, sessionID, requestID, message)
}

func (s agentStore) SettleRequest(ctx context.Context, requestID string, st session.Settlement) error {
	return s.store.SettleRequest(ctx, requestID, st)
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

func (s agentStore) NewestMessageSeq(ctx context.Context, id string) (int64, error) {
	return s.store.NewestMessageSeq(ctx, id)
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

// citeIn records the current blob of each path a note cites, in the run's workspace (its worktree for
// a session that owns one). A path that is not a file there refuses the note (ADR-0010 D1).
func citeIn(workspace string) func(context.Context, []string) ([]taskstate.Cite, error) {
	return func(ctx context.Context, paths []string) ([]taskstate.Cite, error) {
		repo, err := repository.New(workspace)
		if err != nil {
			return nil, err
		}
		blobs, err := repo.FileBlobs(ctx, paths)
		if err != nil {
			return nil, err
		}
		cites := make([]taskstate.Cite, 0, len(paths))
		for _, p := range paths {
			if blobs[p] == "" {
				return nil, fmt.Errorf("cited path %q is not a file in this workspace", p)
			}
			cites = append(cites, taskstate.Cite{Path: p, Blob: blobs[p]})
		}
		return cites, nil
	}
}
