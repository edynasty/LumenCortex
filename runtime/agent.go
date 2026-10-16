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
	// none, low, medium, high or max.
	Effort              config.Effort   `json:"effort,omitempty"`
	Policy              string          `json:"policy,omitempty"`
	MaxSteps            int             `json:"maxSteps,omitempty"`
	RecentMessages      int             `json:"recentMessages,omitempty"`
	MaxToolCallsPerStep int             `json:"maxToolCallsPerStep,omitempty"`
	ToolAllowlist       []string        `json:"toolAllowlist,omitempty"`
	ToolDenylist        []string        `json:"toolDenylist,omitempty"`
	SystemPrompt        string          `json:"systemPrompt,omitempty"`
	Workflow            json.RawMessage `json:"workflow,omitempty"`
	WorkUnits           json.RawMessage `json:"workUnits,omitempty"`
	MaxTokens           int             `json:"maxTokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`

	subagents        toolset.SubagentController
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
	// The seeds are validated here, before anything is written.
	taskState, err := e.startTaskState(ctx, sessionID, opts)
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
	loopOpts := agent.Options{
		MaxSteps:            opts.MaxSteps,
		RecentMessages:      opts.RecentMessages,
		MaxToolCallsPerStep: opts.MaxToolCallsPerStep,
		ToolAllowlist:       opts.ToolAllowlist,
		ToolDenylist:        opts.ToolDenylist,
		SystemPrompt:        opts.SystemPrompt,
		MaxTokens:           opts.MaxTokens,
		Temperature:         opts.Temperature,
		Session:             plan.session,
		Failover:            plan.failover,
		EffortDefault:       plan.effort,
		Escalation:          plan.escalation,
		Effort:              opts.Effort,
		Decisions:           []agent.Decision{plan.decision},
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
	start := session.RunStart{Provider: &plan.resolved.ProviderID, Model: &plan.resolved.ModelID, TaskState: taskState}
	if agent.WritesModelRef(plan.rule) {
		ref := string(plan.session)
		start.ModelRef = &ref
	}
	if opts.Input != "" {
		start.Input = &protocol.Message{Role: "user", Content: opts.Input}
	}
	loopOpts.Resumed = state.Status == "interrupted"
	committed, seq, err := e.store.BeginRun(ctx, sessionID, start)
	if err != nil {
		return nil, err
	}
	if start.Input != nil {
		e.events.publish("session.message", sessionID, map[string]any{"seq": seq, "role": "user"})
	}

	return &preparedRun{
		sessionID: sessionID,
		info:      sessionInfo(committed),
		loopOpts:  loopOpts,
		loop: agent.Loop{
			Models:    plan.models,
			Health:    e.health,
			Store:     agentStore{store: e.store},
			Tools:     tools,
			Workspace: e.agentWorkspaceEvidence(sessionID),
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

func (s agentStore) RecentMessages(ctx context.Context, id string, fromSeq int64, limit int) ([]protocol.Message, error) {
	items, err := s.store.RecentMessagesFrom(ctx, id, fromSeq, limit)
	if err != nil {
		return nil, err
	}
	out := make([]protocol.Message, 0, len(items))
	for _, item := range items {
		var message protocol.Message
		if err := json.Unmarshal(item.JSON, &message); err != nil {
			return nil, err
		}
		if message.Role == "" {
			message.Role = item.Role
		}
		out = append(out, message)
	}
	return out, nil
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
