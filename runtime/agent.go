package runtime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

type AgentOptions struct {
	ProviderName        string          `json:"providerName,omitempty"`
	Policy              string          `json:"policy,omitempty"`
	MaxSteps            int             `json:"maxSteps,omitempty"`
	RecentMessages      int             `json:"recentMessages,omitempty"`
	MaxToolCallsPerStep int             `json:"maxToolCallsPerStep,omitempty"`
	ToolAllowlist       []string        `json:"toolAllowlist,omitempty"`
	SystemPrompt        string          `json:"systemPrompt,omitempty"`
	Workflow            json.RawMessage `json:"workflow,omitempty"`
	MaxTokens           int             `json:"maxTokens,omitempty"`
	Temperature         *float64        `json:"temperature,omitempty"`
}

type AgentResult struct {
	SessionID   string         `json:"sessionId"`
	Status      string         `json:"status"`
	Final       string         `json:"final,omitempty"`
	Usage       protocol.Usage `json:"usage"`
	WaitingGate bool           `json:"waitingGate,omitempty"`
	Workflow    any            `json:"workflow,omitempty"`
}

func (e *Engine) RunAgent(ctx context.Context, sessionID string, provider protocol.Provider, opts AgentOptions) (AgentResult, error) {
	if provider == nil {
		return AgentResult{}, errors.New("provider is required")
	}
	if _, _, err := e.Session(ctx, sessionID); err != nil {
		return AgentResult{}, err
	}
	model := provider.Model()
	identity := session.Patch{Model: &model}
	if opts.ProviderName != "" {
		providerName := opts.ProviderName
		identity.Provider = &providerName
	}
	if err := e.store.Update(ctx, sessionID, identity); err != nil {
		return AgentResult{}, err
	}
	policy := opts.Policy
	if policy == "" {
		policy = toolset.PolicyReadOnly
	}
	tools, err := toolset.New(toolset.Options{Workspace: e.workspace, Policy: policy, Shell: e.shell})
	if err != nil {
		return AgentResult{}, err
	}
	loop := agent.Loop{
		Provider: provider,
		Store:    agentStore{store: e.store},
		Tools:    tools,
		Emit: func(event agent.Event) {
			e.events.publish(event.Type, event.SessionID, event.Data)
			_ = e.store.Journal(context.Background(), event.Type, map[string]any{
				"sessionId": event.SessionID,
				"data":      event.Data,
			})
		},
	}
	result, err := loop.Run(ctx, sessionID, agent.Options{
		MaxSteps:            opts.MaxSteps,
		RecentMessages:      opts.RecentMessages,
		MaxToolCallsPerStep: opts.MaxToolCallsPerStep,
		ToolAllowlist:       opts.ToolAllowlist,
		SystemPrompt:        opts.SystemPrompt,
		WorkflowJSON:        opts.Workflow,
		MaxTokens:           opts.MaxTokens,
		Temperature:         opts.Temperature,
	})
	return AgentResult{
		SessionID: result.SessionID,
		Status: result.Status,
		Final: result.Final,
		Usage: result.Usage,
		WaitingGate: result.WaitingGate,
		Workflow: result.Workflow,
	}, err
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

func (e *Engine) ApproveWorkflowGate(ctx context.Context, sessionID, gateID, actor string) (any, error) {
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
	metadata["workflow"] = runtime.Snapshot()
	metadataRaw, _ := json.Marshal(metadata)
	status := "running"
	if len(runtime.WaitingHumanGates()) > 0 {
		status = "waiting_gate"
	}
	if runtime.CanFinish() {
		status = "running"
	}
	if err := e.store.Update(ctx, sessionID, session.Patch{Status: &status, Metadata: rawMessagePtr(metadataRaw), ClearError: true}); err != nil {
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

func (s agentStore) NextStep(ctx context.Context, id string) (int64, error) {
	return s.store.NextStep(ctx, id)
}

func (s agentStore) Update(ctx context.Context, id string, patch agent.SessionPatch) error {
	value := session.Patch{
		Status: patch.Status,
		Final: patch.Final,
		ClearError: patch.ClearError,
	}
	if patch.Metadata != nil {
		raw, err := json.Marshal(patch.Metadata)
		if err != nil {
			return err
		}
		value.Metadata = rawMessagePtr(raw)
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
