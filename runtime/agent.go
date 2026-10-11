package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/cognition"
	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

type ProviderBinding = agent.ProviderBinding
type DecisionProvider = cognition.DecisionProvider

type AgentOptions struct {
	// Input is a user message appended to the session's transcript under the run
	// lock, after Reconcile. It is the only way a host adds to an existing
	// transcript.
	Input               string          `json:"input,omitempty"`
	ProviderName        string          `json:"providerName,omitempty"`
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
	CognitionEnabled    bool            `json:"cognitionEnabled,omitempty"`
	CategoryProviders   map[string][]ProviderBinding `json:"-"`
	DecisionProviders   []DecisionProvider `json:"-"`
	DecisionPolicy      string             `json:"-"`

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
func (e *Engine) RunAgent(ctx context.Context, sessionID string, provider protocol.Provider, opts AgentOptions) (AgentResult, error) {
	if provider == nil {
		return AgentResult{}, errors.New("provider is required")
	}
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return AgentResult{}, err
	}
	defer lock.Release()
	run, err := e.prepareRun(ctx, sessionID, provider, opts)
	if err != nil {
		return AgentResult{}, err
	}
	return run.execute(ctx)
}

// preparedRun is a Start whose pre-run writes are done: the loop only has to execute.
type preparedRun struct {
	loop     agent.Loop
	loopOpts agent.Options
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
func (e *Engine) prepareRun(ctx context.Context, sessionID string, provider protocol.Provider, opts AgentOptions) (*preparedRun, error) {
	if _, err := e.store.Reconcile(ctx, sessionID); err != nil {
		return nil, err
	}
	if opts.Input != "" && strings.TrimSpace(opts.Input) == "" {
		return nil, errors.New("input must not be blank")
	}
	state, err := agentStore{store: e.store}.Get(ctx, sessionID)
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
		Policy: policy,
		Shell: runner,
		LSP: lspManager,
		MCP: mcpRegistry,
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
		WorkflowJSON:        opts.Workflow,
		WorkUnitsJSON:       opts.WorkUnits,
		MaxTokens:           opts.MaxTokens,
		Temperature:         opts.Temperature,
		CognitionEnabled:    opts.CognitionEnabled,
	}
	if err := agent.CheckStart(state.Metadata, loopOpts); err != nil {
		return nil, err
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

	var decisionLayer *cognition.DecisionLayer
	if len(opts.DecisionProviders) > 0 {
		decisionLayer = &cognition.DecisionLayer{
			Providers: opts.DecisionProviders,
			Policy: opts.DecisionPolicy,
			Health: e.cognitionHealth,
		}
	}

	// Start's one durable write: Input, provider/model identity and the running
	// transition commit together or not at all (§3.2). Nothing above it wrote.
	model := provider.Model()
	start := session.RunStart{Model: &model}
	if opts.ProviderName != "" {
		providerName := opts.ProviderName
		start.Provider = &providerName
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
			Provider: provider,
			ProviderName: opts.ProviderName,
			ProviderChains: opts.CategoryProviders,
			ProviderHealth: e.cognitionHealth,
			DecisionLayer: decisionLayer,
			Store:    agentStore{store: e.store},
			Tools:    tools,
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

func (e *Engine) WorkflowSummary(ctx context.Context, sessionID string) (any, error) {
	current, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{}
	if len(current.Metadata) > 0 {
		if err := json.Unmarshal(current.Metadata, &metadata); err != nil {
			return nil, err
		}
	}
	rawSnapshot, ok := metadata["workflow"]
	if !ok {
		return nil, nil
	}
	raw, err := json.Marshal(rawSnapshot)
	if err != nil {
		return nil, err
	}
	var snapshot workflow.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.Definition == nil {
		return nil, errors.New("persisted workflow has no definition")
	}
	runtime, err := workflow.New(snapshot.Definition, &workflow.State{
		CurrentAction: snapshot.CurrentAction,
		Facts:         snapshot.Facts,
		FactSources:   snapshot.FactSources,
		History:       snapshot.History,
		Status:        snapshot.Status,
	})
	if err != nil {
		return nil, err
	}
	return runtime.Summary(), nil
}

// ApproveWorkflowGate approves a human gate of the session's workflow. It takes
// the session's run lock and returns ErrRunHeld while a run holds it. It changes
// the workflow state only: the session's status stays (waiting_gate) and the host
// continues with a Start.
func (e *Engine) ApproveWorkflowGate(ctx context.Context, sessionID, gateID, actor string) (any, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	current, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	metadata := map[string]any{}
	if len(current.Metadata) > 0 {
		if err := json.Unmarshal(current.Metadata, &metadata); err != nil {
			return nil, err
		}
	}
	rawSnapshot, ok := metadata["workflow"]
	if !ok {
		return nil, errors.New("session has no active workflow")
	}
	raw, err := json.Marshal(rawSnapshot)
	if err != nil {
		return nil, err
	}
	var snapshot workflow.Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return nil, err
	}
	if snapshot.Definition == nil {
		return nil, errors.New("persisted workflow has no definition")
	}
	runtime, err := workflow.New(snapshot.Definition, &workflow.State{
		CurrentAction: snapshot.CurrentAction,
		Facts:         snapshot.Facts,
		FactSources:   snapshot.FactSources,
		History:       snapshot.History,
		Status:        snapshot.Status,
	})
	if err != nil {
		return nil, err
	}
	transition, err := runtime.Approve(gateID, actor)
	if err != nil {
		return nil, err
	}
	workflowJSON, err := json.Marshal(runtime.Snapshot())
	if err != nil {
		return nil, err
	}
	if err := e.store.SetHostMetadataKey(ctx, sessionID, "workflow", workflowJSON); err != nil {
		return nil, err
	}
	e.events.publish("workflow.approved", sessionID, map[string]any{
		"gate": gateID, "actor": actor, "transition": transition, "workflow": runtime.Summary(),
	})
	return runtime.Summary(), nil
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
	metadata := map[string]any{}
	if len(value.Metadata) > 0 {
		if err := json.Unmarshal(value.Metadata, &metadata); err != nil {
			return agent.SessionState{}, err
		}
	}
	usage := protocol.Usage{}
	if len(value.Usage) > 0 {
		_ = json.Unmarshal(value.Usage, &usage)
	}
	final := ""
	if value.Final != nil {
		final = *value.Final
	}
	return agent.SessionState{
		ID: value.ID, Goal: value.Goal, Status: value.Status,
		Metadata: metadata, Final: final, Usage: usage,
	}, nil
}

func (s agentStore) RecentMessages(ctx context.Context, id string, limit int) ([]protocol.Message, error) {
	items, err := s.store.RecentMessages(ctx, id, limit)
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

func (s agentStore) AppendMessage(ctx context.Context, id string, message protocol.Message) (int64, error) {
	return s.store.AppendMessage(ctx, id, message.Role, message)
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

func (s agentStore) SetMetadataKey(ctx context.Context, id, key string, value json.RawMessage) error {
	return s.store.SetHostMetadataKey(ctx, id, key, value)
}

func (s agentStore) Update(ctx context.Context, id string, patch agent.SessionPatch) error {
	value := session.Patch{
		Status: patch.Status,
		Final: patch.Final,
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
