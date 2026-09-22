package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

const (
	MaxConcurrentSubagentsPerParent = 4
	MaxSubagentTreeDepth             = 4
	MaxSubagentTreeNodes             = 100
)

var ErrSubagentLimitExceeded = errors.New("subagent concurrency limit exceeded for parent session")

type SubagentNode struct {
	SessionID       string         `json:"sessionId"`
	ParentSessionID string         `json:"parentSessionId,omitempty"`
	Goal            string         `json:"goal"`
	Status          string         `json:"status"`
	Final           string         `json:"final,omitempty"`
	Active          bool           `json:"active"`
	CreatedAt       time.Time      `json:"createdAt"`
	UpdatedAt       time.Time      `json:"updatedAt"`
	Runtime         SessionRuntime `json:"runtime"`
	Children        []SubagentNode `json:"children,omitempty"`
}

type SessionCheckpoint = session.Checkpoint

type subagentController struct {
	supervisor *RunSupervisor
	parentID   string
	provider   protocol.Provider
	parentOpts AgentOptions
}

func (c *subagentController) Spawn(ctx context.Context, goal string) (any, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, errors.New("subagent goal is required")
	}
	if c.supervisor == nil || c.supervisor.engine == nil {
		return nil, ErrRunSupervisorClosed
	}
	engine := c.supervisor.engine

	activeChildren, err := c.activeChildren(ctx)
	if err != nil {
		return nil, err
	}
	if activeChildren >= MaxConcurrentSubagentsPerParent {
		return nil, ErrSubagentLimitExceeded
	}

	parent, err := engine.store.Get(ctx, c.parentID)
	if err != nil {
		return nil, err
	}
	identity, err := engine.SessionRuntime(ctx, c.parentID)
	if err != nil {
		return nil, err
	}

	metadata := map[string]any{
		"subagent": map[string]any{
			"parentSessionId": c.parentID,
			"mode":            "read-only",
		},
	}
	if identity.Kind == RuntimeWorktree {
		metadata["runtime"] = identity
	}

	handle, err := engine.NewSession(ctx, SessionOptions{
		Goal:     goal,
		Provider: parent.Provider,
		Model:    parent.Model,
		Metadata: metadata,
	})
	if err != nil {
		return nil, err
	}

	relationMetadata, _ := json.Marshal(map[string]any{
		"mode": "read-only",
	})
	if err := engine.store.AddRelation(ctx, session.Relation{
		ParentSessionID: c.parentID,
		ChildSessionID:  handle.ID,
		Kind:            "subagent",
		Metadata:        relationMetadata,
	}); err != nil {
		return nil, err
	}
	_, _ = engine.store.AppendCheckpoint(context.Background(), c.parentID, "subagent.spawn", map[string]any{
		"childSessionId": handle.ID,
		"goal":           goal,
	})

	childOpts := c.childOptions()
	if _, err := c.supervisor.Start(ctx, handle.ID, c.provider, childOpts); err != nil {
		status := "interrupted"
		errorRaw, _ := json.Marshal(map[string]any{
			"kind":    "subagent_start_failed",
			"message": err.Error(),
		})
		_ = engine.store.Update(context.Background(), handle.ID, session.Patch{
			Status: &status,
			Error:  rawMessagePtr(errorRaw),
		})
		_, _ = engine.store.AppendCheckpoint(context.Background(), c.parentID, "subagent.start_failed", map[string]any{
			"childSessionId": handle.ID,
			"error":          err.Error(),
		})
		return nil, err
	}

	engine.events.publish("subagent.spawned", c.parentID, map[string]any{
		"childSessionId": handle.ID,
		"goal":           goal,
	})
	return c.summary(ctx, handle.ID)
}

func (c *subagentController) List(ctx context.Context) (any, error) {
	return c.supervisor.SubagentTree(ctx, c.parentID)
}

func (c *subagentController) childOptions() AgentOptions {
	maxSteps := c.parentOpts.MaxSteps
	if maxSteps <= 0 || maxSteps > 12 {
		maxSteps = 12
	}
	recent := c.parentOpts.RecentMessages
	if recent <= 0 || recent > 8 {
		recent = 8
	}
	maxCalls := c.parentOpts.MaxToolCallsPerStep
	if maxCalls <= 0 || maxCalls > 6 {
		maxCalls = 6
	}
	return AgentOptions{
		ProviderName:        c.parentOpts.ProviderName,
		Policy:              toolset.PolicyReadOnly,
		MaxSteps:            maxSteps,
		RecentMessages:      recent,
		MaxToolCallsPerStep: maxCalls,
		SystemPrompt: "You are a focused read-only LumenCortex subagent. Investigate the assigned goal using repository search, file reads, Git inspection, and language services. Do not modify files, run shell commands, or spawn additional agents. Return concise evidence and actionable findings to the parent task.",
		MaxTokens:           c.parentOpts.MaxTokens,
		Temperature:         c.parentOpts.Temperature,
		parentSessionID:     c.parentID,
		disableSubagents:    true,
		disableMCP:          true,
	}
}

func (c *subagentController) activeChildren(ctx context.Context) (int, error) {
	relations, err := c.supervisor.engine.store.ChildRelations(ctx, c.parentID, 100)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, relation := range relations {
		if relation.Kind == "subagent" && c.supervisor.Active(relation.ChildSessionID) {
			count++
		}
	}
	return count, nil
}

func (c *subagentController) summary(ctx context.Context, childID string) (SubagentNode, error) {
	value, err := c.supervisor.engine.store.Get(ctx, childID)
	if err != nil {
		return SubagentNode{}, err
	}
	identity, err := c.supervisor.engine.SessionRuntime(ctx, childID)
	if err != nil {
		return SubagentNode{}, err
	}
	final := ""
	if value.Final != nil {
		final = *value.Final
	}
	return SubagentNode{
		SessionID:       value.ID,
		ParentSessionID: c.parentID,
		Goal:            value.Goal,
		Status:          value.Status,
		Final:           final,
		Active:          c.supervisor.Active(value.ID),
		CreatedAt:       value.CreatedAt,
		UpdatedAt:       value.UpdatedAt,
		Runtime:         identity,
	}, nil
}

func (s *RunSupervisor) SubagentTree(ctx context.Context, parentSessionID string) ([]SubagentNode, error) {
	if s == nil || s.engine == nil {
		return nil, ErrRunSupervisorClosed
	}
	remaining := MaxSubagentTreeNodes
	return s.subagentChildren(ctx, parentSessionID, 0, &remaining)
}

func (s *RunSupervisor) subagentChildren(ctx context.Context, parentSessionID string, depth int, remaining *int) ([]SubagentNode, error) {
	if depth >= MaxSubagentTreeDepth || *remaining <= 0 {
		return []SubagentNode{}, nil
	}
	relations, err := s.engine.store.ChildRelations(ctx, parentSessionID, 100)
	if err != nil {
		return nil, err
	}
	out := make([]SubagentNode, 0, len(relations))
	for _, relation := range relations {
		if relation.Kind != "subagent" || *remaining <= 0 {
			continue
		}
		value, err := s.engine.store.Get(ctx, relation.ChildSessionID)
		if err != nil {
			return nil, err
		}
		identity, err := s.engine.SessionRuntime(ctx, value.ID)
		if err != nil {
			return nil, err
		}
		final := ""
		if value.Final != nil {
			final = *value.Final
		}
		(*remaining)--
		node := SubagentNode{
			SessionID:       value.ID,
			ParentSessionID: parentSessionID,
			Goal:            value.Goal,
			Status:          value.Status,
			Final:           final,
			Active:          s.Active(value.ID),
			CreatedAt:       value.CreatedAt,
			UpdatedAt:       value.UpdatedAt,
			Runtime:         identity,
		}
		children, err := s.subagentChildren(ctx, value.ID, depth+1, remaining)
		if err != nil {
			return nil, err
		}
		node.Children = children
		out = append(out, node)
	}
	return out, nil
}

func (e *Engine) SessionCheckpoints(ctx context.Context, sessionID string, limit int) ([]SessionCheckpoint, error) {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	return e.store.Checkpoints(ctx, sessionID, limit)
}

func (e *Engine) checkpointSubagentStop(parentID, childID string, runErr error) {
	payload := map[string]any{"childSessionId": childID}
	if value, err := e.store.Get(context.Background(), childID); err == nil {
		payload["status"] = value.Status
		if value.Final != nil {
			payload["final"] = *value.Final
		}
	}
	if runErr != nil {
		payload["error"] = runErr.Error()
	}
	_, _ = e.store.AppendCheckpoint(context.Background(), parentID, "subagent.stopped", payload)
	e.events.publish("subagent.stopped", parentID, payload)
}

func subagentEventData(eventType, childID string, data map[string]any) map[string]any {
	out := map[string]any{
		"childSessionId": childID,
		"eventType":      eventType,
	}
	if data != nil {
		out["data"] = data
	}
	return out
}

func validateSubagentParent(parentID string) error {
	if strings.TrimSpace(parentID) == "" {
		return fmt.Errorf("subagent parent session id is required")
	}
	return nil
}
