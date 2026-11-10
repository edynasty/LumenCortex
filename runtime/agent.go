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
	"github.com/edynasty/LumenCortex/evalapi"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/verify"
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
	// resolved level then acts as the run's default).
	Effort config.Effort `json:"effort,omitempty"`
	Policy string        `json:"policy,omitempty"`
	// MaxSteps: -1 means unlimited; zero inherits/defaults; positive is an explicit bound.
	MaxSteps            int             `json:"maxSteps,omitempty"`
	MaxToolCallsPerStep int             `json:"maxToolCallsPerStep,omitempty"`
	ToolAllowlist       []string        `json:"toolAllowlist,omitempty"`
	ToolDenylist        []string        `json:"toolDenylist,omitempty"`
	SystemPrompt        string          `json:"systemPrompt,omitempty"`
	Workflow            json.RawMessage `json:"workflow,omitempty"`
	WorkUnits           json.RawMessage `json:"workUnits,omitempty"`
	// Delivery is an explicit host contract persisted once and reused on resume.
	Delivery    *DeliveryContract `json:"delivery,omitempty"`
	MaxTokens   int               `json:"maxTokens,omitempty"`
	Temperature *float64          `json:"temperature,omitempty"`
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
	// Deadline is an optional absolute execution deadline; nil sets no global time limit.
	// Children inherit it and caller cancellation. A child's explicit deadline may only shorten it.
	// DeadlineReserveSeconds is a delivery hint, not an earlier automatic execution stop.
	Deadline               *time.Time `json:"deadline,omitempty"`
	DeadlineReserveSeconds int        `json:"deadlineReserveSeconds,omitempty"`
	// Budget is this run's budget (ADR-0019 §6): each non-zero field overrides the global file's
	// execution.budget, which bounds every run, child runs included. The result's spend reports what
	// the run used.
	Budget *config.Budget `json:"budget,omitempty"`
	// Retry overrides global execution.retry field by field; response-step recovery is finite.
	// Immediate children inherit a copy. Deferred proposal Starts and explicit Resume use current
	// execution policy instead of persisting this transient override in task state.
	Retry *config.Retry `json:"retry,omitempty"`
	// ChildExecution overrides execution controls of immediately spawned children, not authority.
	// A host may also set AgentOptions directly when explicitly starting/resuming an existing child.
	ChildExecution *ChildExecutionOptions `json:"childExecution,omitempty"`
	// EvalEnabled opts this run into the host's execution runtimes. It is off unless the host enabled
	// Eval in Engine Options and set this flag: disabled keeps the legacy tool catalog unchanged.
	EvalEnabled bool `json:"evalEnabled,omitempty"`
	// Verification configures the completion gate's semantic level, claim coverage and bounded repair
	// loop (spec §24-25). nil keeps the existing gate.
	Verification *VerificationOptions `json:"verification,omitempty"`
	// Review opts this run into the single, read-only independent review (spec §22). nil or
	// Enabled false runs none, and no model request is made for a review.
	Review *ReviewOptions `json:"review,omitempty"`
	// Debug configures safe-boundary observation; it never grants tool authority.
	Debug    *AgentDebugOptions `json:"debug,omitempty"`
	ownerRun *managedRun

	subagents toolset.SubagentController
	// sharedEvalSession and sharedEvalLease are the host's grant of a parent kernel to this child,
	// set by the launch, never by the child's model. The grant is created at the child's Start.
	sharedEvalSession string
	sharedEvalLease   time.Duration
	// childAgent is a child run's default agent: explore for a read-only subagent, worker for an
	// execution task. "" for a thread.
	childAgent       string
	tasks            agent.TaskStarter
	parentSessionID  string
	disableSubagents bool
	disableMCP       bool
}

// ChildExecutionOptions overrides inherited limits; zero/nil inherits, -1 removes a step/batch cap.
// Deadline cannot extend the parent's deadline or detach cancellation.
type ChildExecutionOptions struct {
	MaxSteps            int        `json:"maxSteps,omitempty"`
	MaxToolCallsPerStep int        `json:"maxToolCallsPerStep,omitempty"`
	Deadline            *time.Time `json:"deadline,omitempty"`
	// EvalEnabled grants a child the host's execution tools. nil or false keeps them off: a child
	// never inherits its parent's EvalEnabled, and its model can never grant itself anything.
	EvalEnabled *bool `json:"evalEnabled,omitempty"`
	// SharedKernel grants the child read-only artifact/result access to a parent's kernel.
	// The child executes in its own authority-domain kernel; the observer lease is
	// bounded by SharedKernelLeaseSeconds and grants no control of the parent's process.
	SharedKernel string `json:"sharedKernel,omitempty"`
	// SharedKernelLeaseSeconds bounds the grant's lease. Zero or negative takes five minutes; more
	// than an hour is capped at an hour.
	SharedKernelLeaseSeconds int `json:"sharedKernelLeaseSeconds,omitempty"`
}

// childEvalGrant is the host's explicit grant for a child's execution tools. It is read only from the
// parent's ChildExecution override, never from the parent's own EvalEnabled and never from model
// input. Absent means the child has no eval tools.
func childEvalGrant(parent AgentOptions) (bool, string, time.Duration) {
	override := parent.ChildExecution
	if override == nil || override.EvalEnabled == nil || !*override.EvalEnabled {
		return false, "", 0
	}
	lease := time.Duration(override.SharedKernelLeaseSeconds) * time.Second
	if lease <= 0 {
		lease = 5 * time.Minute
	}
	if lease > time.Hour {
		lease = time.Hour
	}
	return true, strings.TrimSpace(override.SharedKernel), lease
}

func inheritChildExecution(child, parent AgentOptions) AgentOptions {
	child.MaxSteps, child.MaxToolCallsPerStep = parent.MaxSteps, parent.MaxToolCallsPerStep
	child.DeadlineReserveSeconds = parent.DeadlineReserveSeconds
	child.Retry = cloneRetry(parent.Retry)
	if parent.Budget != nil {
		value := *parent.Budget
		child.Budget = &value
	}
	if parent.Deadline != nil {
		value := *parent.Deadline
		child.Deadline = &value
	}
	if override := parent.ChildExecution; override != nil {
		if override.MaxSteps != 0 {
			child.MaxSteps = override.MaxSteps
		}
		if override.MaxToolCallsPerStep != 0 {
			child.MaxToolCallsPerStep = override.MaxToolCallsPerStep
		}
		if override.Deadline != nil && (child.Deadline == nil || override.Deadline.Before(*child.Deadline)) {
			value := *override.Deadline
			child.Deadline = &value
		}
	}
	return child
}

func executionContext(ctx context.Context, opts AgentOptions) (context.Context, context.CancelFunc) {
	if opts.Deadline != nil {
		return context.WithDeadlineCause(ctx, *opts.Deadline, agent.ErrRunDeadline)
	}
	return context.WithCancel(ctx)
}

type AgentResult struct {
	SessionID   string         `json:"sessionId"`
	Status      string         `json:"status"`
	Final       string         `json:"final,omitempty"`
	Usage       protocol.Usage `json:"usage"`
	WaitingGate bool           `json:"waitingGate,omitempty"`
	Workflow    any            `json:"workflow,omitempty"`
	// Spend is what the run used against its budget: tool calls, model requests, and tokens and cost
	// split into known, estimated and unknown (ADR-0019 §6).
	Spend agent.Spend `json:"spend"`
	// Delivery is a separate verification outcome; it never changes Status or the Agent error.
	Delivery *DeliveryResult `json:"delivery,omitempty"`
}

// RunAgent runs the agent loop to its exit on the calling goroutine. It holds the
// session's run lock for the whole call and returns ErrRunHeld, having written
// nothing, when another Engine holds it. RunSupervisor.Start is the asynchronous
// entry point with the same contract.
func (e *Engine) RunAgent(ctx context.Context, sessionID string, opts AgentOptions) (AgentResult, error) {
	ctx, cancel := executionContext(ctx, opts)
	defer cancel()
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return AgentResult{}, err
	}
	ctx, done := e.trackRun(ctx, sessionID)
	defer done()
	defer lock.Release()
	if err := ctx.Err(); err != nil {
		return AgentResult{}, err
	}
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
	info        SessionInfo
	finishOwned func()
	// delivery is the pending record the Start committed, nil without a delivery contract.
	delivery *DeliveryResult
	// runID identifies this Start's run; parentSessionID is the child's parent, "" for a thread.
	runID           string
	parentSessionID string
	// evalEnabled reports that this run could have opened execution kernels: its owner is closed on
	// an abnormal end or when the run is a child.
	evalEnabled bool
}

// execute runs under the caller's tracked lifetime, including preparation and owned cleanup.
func (r *preparedRun) execute(ctx context.Context) (AgentResult, error) {
	result, err := r.loop.Run(ctx, r.sessionID, r.loopOpts)
	// Terminal observers also run after cancellation, under a separate bounded context.
	observeEnd := func(event LifecycleEvent) {
		hookCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = r.engine.DispatchExecutionHook(hookCtx, LifecycleRequest{Event: event, SessionID: r.parentSessionID, RunID: r.runID, Detail: map[string]any{"childSessionId": r.sessionID, "status": result.Status}})
	}
	if r.parentSessionID != "" {
		observeEnd(LifecycleBeforeSubagentEnd)
	}
	r.engine.endRunJobs(ctx, r.sessionID, r.loopOpts.RunID)
	if r.engine.dapManager != nil {
		err = errors.Join(err, r.engine.dapManager.CloseRun(r.sessionID, r.loopOpts.RunID))
	}
	if r.finishOwned != nil {
		r.finishOwned()
	}
	// A kernel belongs to the run's owner. A completed top-level run keeps it for the next run of the
	// same session (its variables are the point of a persistent runtime); an interrupted, failed or
	// waiting run, and every child run, closes its owner's kernels here. CloseOwner is idempotent.
	var cleanupErr error
	if r.evalEnabled {
		switch {
		case r.parentSessionID != "":
			// A child's observer lease ends with its run; the parent's kernel stays intact.
			// Both teardown steps are reported, never swallowed.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cleanupErr = errors.Join(r.engine.revokeEvalAgent(cleanupCtx, r.sessionID), r.engine.closeEvalOwner(r.sessionID))
			cancel()
		case result.Status != agent.StatusCompleted:
			cleanupErr = r.engine.closeEvalOwner(r.sessionID)
		}
	}
	if cleanupErr != nil {
		// A leaked worker is a safety failure: the run's outcome carries the cleanup error instead of
		// reporting a silent completion.
		err = errors.Join(err, fmt.Errorf("eval cleanup: %w", cleanupErr))
	}
	// Only a completed execution is verified; otherwise the result reports the pending record its
	// Start committed, so it never reads as "no contract". A run without a contract settles nothing.
	var delivery *DeliveryResult
	if r.delivery != nil {
		if result.Status == agent.StatusCompleted {
			delivery = r.engine.settleDelivery(ctx, r.sessionID, r.loopOpts.RunID)
		} else {
			recorded := *r.delivery
			delivery = &recorded
		}
	}
	if r.parentSessionID != "" {
		r.engine.runsMu.Lock()
		parent := r.engine.runs[r.parentSessionID]
		if parent != nil && parent.debug != nil {
			parent.debug.Observe(agent.DebugBoundary{Event: "AfterSubagentEnd", Agent: r.info.Agent, Failed: result.Status != agent.StatusCompleted})
		}
		r.engine.runsMu.Unlock()
		observeEnd(LifecycleAfterSubagentEnd)
	}
	return AgentResult{
		SessionID:   result.SessionID,
		Status:      result.Status,
		Final:       result.Final,
		Usage:       result.Usage,
		WaitingGate: result.WaitingGate,
		Workflow:    result.Workflow,
		Spend:       result.Spend,
		Delivery:    delivery,
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
func (e *Engine) prepareRun(ctx context.Context, sessionID string, opts AgentOptions) (prepared *preparedRun, err error) {
	if err := e.events.persistenceError(); err != nil {
		return nil, err
	}
	if _, err := e.store.Reconcile(ctx, sessionID); err != nil {
		return nil, err
	}
	runID, err := newRunID()
	if err != nil {
		return nil, err
	}
	debugControl, err := agent.NewDebugControl(runID, opts.Debug, func(snapshot agent.DebugSnapshot) {
		e.events.publish("debug.agent.state", sessionID, map[string]any{"runId": runID, "status": snapshot.State, "phase": snapshot.Boundary.Event, "step": snapshot.Boundary.Step, "toolCallId": snapshot.Boundary.ToolCallID, "name": snapshot.Boundary.Tool, "reason": snapshot.Reason})
		// A pause is durable state: the Hub shows interrupted(paused) after a restart instead of guessing.
		if snapshot.State == "paused" || snapshot.State == "running" || snapshot.State == "closed" {
			e.noteDebugState(sessionID, runID, snapshot)
		}
	})
	if err != nil {
		return nil, err
	}
	// The events the loop publishes carry no run id; the diagnostics bridge attaches this one so a
	// trace can group them by run (diagnostics.go).
	e.noteRunStarted(sessionID, runID)
	defer func() {
		if err != nil {
			e.events.forgetRun(sessionID, runID)
		}
	}()
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
		return nil, errors.New("delegation isolated needs RunSupervisor.Start to manage child execution")
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
	taskState, results, pendingDelivery, err := e.startTaskState(ctx, sessionID, runID, opts, func(ctx context.Context, state *taskstate.State) error {
		return e.decideConstraints(ctx, runID, state, checks)
	})
	if err != nil {
		return nil, err
	}
	runner := e.shell
	if agentWorkspace != e.workspace {
		runner = shell.New(agentWorkspace)
	}
	// The workspace's manager exists whether or not a server is running. In a trusted workspace the
	// toolset offers the lsp_* tools when a server can serve the workspace and the first call starts it;
	// in an untrusted one only a server the host started is used, and none is started (ADR-0013).
	lspAccess := e.lspForRun(agentWorkspace, plan.trusted)
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
	// A wake-up starts a later run on this session with this run's policy and host tool filters. A
	// child run (a subagent or an execution task) has a lifecycle of its own that its parent owns, so it
	// gets no wake-up tools: the thread does.
	var wakeupBinding toolset.WakeupController
	if opts.parentSessionID == "" {
		wakeupBinding = e.wakeupsFor(sessionID, policy, opts.ToolAllowlist, opts.ToolDenylist)
	}
	// A server whose tool list was never discovered cannot be offered, so a run that may use MCP
	// discovers the missing catalogs here, once per server, in parallel and under a bounded wait. A
	// server whose catalog is already recorded costs nothing: this is a no-op in the steady state
	// (ADR-0021 §3).
	if mcpRegistry != nil {
		e.discoverMCPCatalogs(ctx, mcpRegistry, agentWorkspace, opts.ToolAllowlist, opts.ToolDenylist)
	}
	execution := e.executionLimits()
	tools, err := toolset.New(toolset.Options{
		Workspace: agentWorkspace,
		Policy:    policy,
		Shell:     runner,
		LSP:       lspAccess,
		MCP:       mcpRegistry,
		Subagents: opts.subagents,
		// Children get no spawn tool, so the list matters only to a thread's run.
		SubagentAgents:  plan.spawnAgents,
		Threads:         threadReader{engine: e, sessionID: sessionID, workspace: agentWorkspace},
		Skills:          newSkillReader(prompt.Skills.Included),
		Notebook:        newNotebookAccess(e, sessionID, runID),
		Protected:       e.protected,
		ShellTimeout:    time.Duration(execution.ShellTimeoutSeconds) * time.Second,
		ShellMaxTimeout: time.Duration(execution.ShellMaxTimeoutSeconds) * time.Second,
		// The run's background job queue: its session, its run id and the workspace its commands run in.
		// The completion gate and the run's end use the same binding through the Engine (ADR-0019 §3).
		Jobs:    e.runJobs(sessionID, runID, agentWorkspace),
		Wakeups: wakeupBinding,
		// A tool's own log lines name the session and the run they belong to.
		Logger: e.log.With("session_id", sessionID, "run_id", runID),
		// A server with a session binding receives this id, so an action is attributed to the run that
		// drove it and the server-side session can be closed with the run.
		SessionID: sessionID,
		// The one editing engine of the workspace: a session's read snapshots and the collaboration
		// layer's write leases outlive this run.
		Editing: e.editing,
		// Edit outcomes are runtime events: edit.committed and edit.conflict carry the path and the
		// revisions, so Desktop's diff view and the durable history see every change.
		Events: func(kind, sid string, payload map[string]any) { e.events.publish(kind, sid, payload) },
	})
	if err != nil {
		return nil, err
	}
	// The execution runtimes' tools are bound only when the host enabled them for this run. Their
	// controller is the run's own; a cell's tool callback comes from the loop's context, never from
	// model input (internal/toolset/eval.go, runtime/eval.go).
	if err := e.executionTools(ctx, sessionID, opts, tools, runID); err != nil {
		return nil, err
	}
	if err := e.debugTools(ctx, sessionID, runID, policy, plan.trusted, tools); err != nil {
		return nil, err
	}
	workerRoot, err := e.bindWorkerMessages(ctx, sessionID, tools)
	if err != nil {
		return nil, err
	}
	// A child never executes inside the parent's kernel: a kernel is one actor's
	// authority domain, so the child gets its own kernel (its own session
	// identity) and the two sides exchange artifacts. The parent's kernel is
	// shared read-only, which is what authorizes the child to import the parent's
	// results and artifacts as explicit, bounded, digest-checked copies.
	if opts.parentSessionID != "" && opts.EvalEnabled && opts.sharedEvalSession != "" {
		grant := evalapi.Grant{AgentID: sessionID, Role: evalapi.RoleObserver,
			ExpiresAt: time.Now().Add(opts.sharedEvalLease)}
		if err := e.GrantEvalSession(ctx, opts.parentSessionID, opts.sharedEvalSession, grant); err != nil {
			return nil, err
		}
		// A Start that fails after the grant never reaches the run that ends it, so the grant is
		// revoked here instead of surviving until its lease expires. A successful Start keeps it for
		// the child's run, which revokes it on exit (preparedRun.execute).
		defer func() {
			if err == nil {
				return
			}
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if revokeErr := e.revokeEvalAgent(cleanupCtx, sessionID); revokeErr != nil {
				err = errors.Join(err, fmt.Errorf("eval grant cleanup: %w", revokeErr))
			}
		}()
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
		Effort:              effortOverride,
		EffortAutoCeiling:   effortCeiling,
		CandidateEfforts:    candidateEfforts,
		Decisions:           decisions,
		Interactive:         interactive,
		Delegation:          delegation,
		TaskAgents:          plan.taskAgents,
		RunID:               runID,
		WorkerRoot:          workerRoot,
		Withheld:            withheld,
		DeadlineReserve:     time.Duration(opts.DeadlineReserveSeconds) * time.Second,
	}
	if opts.Deadline != nil {
		loopOpts.Deadline = *opts.Deadline
	}
	// The session model's execution profile supplies the defaults its runs start with: a step or
	// per-step cap when the host set none, its own budget, and the presentation variant. None of it
	// changes policy, permissions or checks.
	if profile := plan.resolved.Profile; profile != nil {
		if loopOpts.MaxSteps == 0 && profile.MaxSteps > 0 {
			loopOpts.MaxSteps = profile.MaxSteps
		}
		if loopOpts.MaxToolCallsPerStep == 0 && profile.MaxToolCallsPerStep > 0 {
			loopOpts.MaxToolCallsPerStep = profile.MaxToolCallsPerStep
		}
		if profile.Budget != nil {
			loopOpts.Budget = *profile.Budget
		}
		loopOpts.Presentation = agent.PresentationOptions{
			ToolDescriptions: profile.ToolDescriptions,
			Feedback:         profile.Feedback,
		}
	}
	if global := execution.Budget; global != nil {
		loopOpts.Budget = *global
	}
	if opts.Budget != nil {
		loopOpts.Budget = loopOpts.Budget.Over(*opts.Budget)
	}
	var retry config.Retry
	if global := execution.Retry; global != nil {
		retry = *global
	}
	if opts.Retry != nil {
		retry = retry.Over(*opts.Retry)
	}
	loopOpts.Recovery = agent.RecoveryFrom(retry)
	reviewEnabled := opts.Review != nil && opts.Review.Enabled
	verificationWanted := opts.Verification != nil && (opts.Verification.RequireSemantic ||
		opts.Verification.RequireIndependent || opts.Verification.Review || reviewEnabled)
	if opts.Verification != nil {
		loopOpts.Verification = agent.VerificationOptions{
			RequireSemantic:    opts.Verification.RequireSemantic,
			RequireIndependent: opts.Verification.RequireIndependent,
			MaxRepairs:         opts.Verification.MaxRepairs,
			Review:             reviewEnabled,
		}
	} else if reviewEnabled {
		loopOpts.Verification = agent.VerificationOptions{Review: true}
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
	if opts.parentSessionID != "" {
		pre, hookErr := e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecycleBeforeSubagentStart, SessionID: opts.parentSessionID, RunID: runID, Detail: map[string]any{"childSessionId": sessionID}})
		if hookErr != nil {
			return nil, hookErr
		}
		if pre.Denied() {
			return nil, errors.New("subagent start denied: " + pre.FeedbackText())
		}
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
	if open, err := e.store.ListJobs(ctx, session.JobFilter{SessionID: sessionID, States: jobsIndexStates, Limit: maxJobsIndexEntries + 1}); err != nil {
		return nil, err
	} else if row, ok := jobsIndexRow(open); ok {
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
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user", "origin": start.Notices[i-noticeFrom].Origin})
		case start.Input != nil:
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user"})
		default:
			e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user", "origin": protocol.OriginResume})
		}
	}
	if len(results) > 0 {
		e.events.publish("task.delivered", sessionID, map[string]any{"count": len(results)})
	}

	var verifier verify.Verifier
	if verificationWanted {
		verifier = engineVerifier{engine: e, sessionID: sessionID, checks: checks}
	}
	var reviewer verify.Reviewer
	if reviewEnabled {
		// The reviewer runs over the real provider path, metered like any request. It is built only
		// when the host explicitly enabled it for this run.
		reviewer = providerReviewer{engine: e, sessionID: sessionID, runID: runID, model: opts.Review.Model}
	}
	e.runsMu.Lock()
	tracked := e.runs[sessionID]
	if tracked != nil {
		tracked.debug = debugControl
	}
	e.runsMu.Unlock()
	if tracked == nil {
		return nil, errors.New("agent run lifetime is unavailable")
	}
	if opts.parentSessionID != "" {
		_, _ = e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecycleAfterSubagentStart, SessionID: opts.parentSessionID, RunID: runID, Detail: map[string]any{"childSessionId": sessionID}})
	}
	return &preparedRun{
		engine:          e,
		sessionID:       sessionID,
		info:            sessionInfo(committed),
		delivery:        pendingDelivery,
		loopOpts:        loopOpts,
		runID:           runID,
		parentSessionID: opts.parentSessionID,
		evalEnabled:     opts.EvalEnabled,
		loop: agent.Loop{
			Models:    plan.models,
			Health:    e.health,
			Store:     agentStore{store: e.store},
			Tools:     tools,
			Snapshots: snapshots,
			Jobs:      e.jobGate(),
			Evidence:  e.runEvidence(sessionID, checks),
			Checks:    checks,
			Tasks:     taskStarterFor(delegation, opts.tasks),
			Cite:      citeIn(agentWorkspace),
			Hooks:     e.hookRegistryForRun(),
			Verifier:  verifier,
			Reviewer:  reviewer,
			DebugBoundary: func(ctx context.Context, point agent.DebugBoundary) error {
				point.Agent = plan.agentID
				if err := e.runtimeEventAccess(ctx, sessionID); err != nil {
					return err
				}
				// A worker renews its Task Lease claims at the model step boundary: the lease then expires
				// only for a worker that stopped making progress.
				if point.Event == "AfterModelCall" {
					e.renewHeldLeases(sessionID)
				}
				if err := debugControl.Boundary(ctx, point); err != nil {
					return err
				}
				return e.runtimeEventAccess(ctx, sessionID)
			},
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
	beforeAction := wf.CurrentAction
	transition, err := wf.Approve(gateID, actor)
	if err != nil {
		return nil, err
	}
	// A human gate changes authority-bearing workflow state even when the
	// action remains the same (for example a terminal action awaiting approval).
	detail := map[string]any{"from": beforeAction, "to": wf.CurrentAction, "gate": gateID}
	pre, hookErr := e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecycleBeforeWorkflowTransition, SessionID: sessionID, Detail: detail})
	if hookErr != nil {
		return nil, hookErr
	}
	if pre.Denied() {
		return nil, errors.New("workflow transition denied: " + pre.FeedbackText())
	}
	state.SetWorkflow(wf)
	if _, err := e.putTaskState(ctx, sessionID, taskstate.ReasonUser, state, 0, nil); err != nil {
		return nil, err
	}
	e.events.publish("workflow.approved", sessionID, map[string]any{
		"gate": gateID, "actor": actor, "transition": transition, "workflow": wf.Summary(),
	})
	_, _ = e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecycleAfterWorkflowTransition, SessionID: sessionID, Detail: detail})
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

// SettleExecution forwards a nested callback's admitted effect to the store: it settles the
// execution and writes its derived edges without a transcript row, so the transcript keeps exactly
// the top-level tool pairing.
func (s agentStore) SettleExecution(ctx context.Context, sessionID string, result session.ExecutionResult, edges ...session.DerivedEdge) error {
	return s.store.SettleExecution(ctx, sessionID, result, edges...)
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

// PendingSteers and MarkSteerApplied make the store the loop's steer inbox (agent.SteerStore): the loop
// injects a queued instruction at the request window it builds next.
func (s agentStore) PendingSteers(ctx context.Context, id string, limit int) ([]session.SteerMessage, error) {
	return s.store.PendingSteerMessages(ctx, id, limit)
}

func (s agentStore) MarkSteerApplied(ctx context.Context, id string, seq, appliedSeq int64) error {
	return s.store.MarkSteerApplied(ctx, id, seq, appliedSeq)
}

// PendingWorkerMessages and MarkWorkerMessagesDelivered make the store the loop's push message queue
// (agent.WorkerMessageStore): a busy recipient reads them in its next request without calling
// worker_inbox.
func (s agentStore) PendingWorkerMessages(ctx context.Context, root, recipient, runID string, limit int) ([]session.WorkerMessage, error) {
	page, err := s.store.PendingWorkerMessages(ctx, root, recipient, runID, limit)
	if err != nil {
		return nil, err
	}
	// A fenced sender's body is not readable: the row says so and the delivery still records.
	for i := range page {
		fenced, err := s.store.SessionFenced(ctx, page[i].SenderID)
		if err != nil {
			return nil, err
		}
		if fenced {
			page[i].Withdrawn = true
			page[i].Body = ""
		}
	}
	return page, nil
}

func (s agentStore) MarkWorkerMessagesDelivered(ctx context.Context, root, recipient, runID string, messages []session.DeliveredWorkerMessage) error {
	return s.store.MarkWorkerMessagesDelivered(ctx, root, recipient, runID, messages)
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

// cloneRetry isolates an inherited per-run override from its parent's mutable options.
func cloneRetry(r *config.Retry) *config.Retry {
	if r == nil {
		return nil
	}
	copy := *r
	return &copy
}
