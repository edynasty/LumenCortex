package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

const MaxSubagentGoalBytes = 16 << 10

// AgentCatalogLines are the "- id: description" lines a delegation tool's description lists, one per
// agent the tool may run (ADR-0009 A3). The model picks by these; nothing else of an agent is shown.
func AgentCatalogLines(agents []config.AgentInfo) string {
	var b strings.Builder
	for _, a := range agents {
		b.WriteString("\n- ")
		b.WriteString(a.ID)
		b.WriteString(": ")
		b.WriteString(strings.Join(strings.Fields(a.Description), " "))
	}
	return b.String()
}

// AgentIDs are the ids of agents, in catalog order.
func AgentIDs(agents []config.AgentInfo) []string {
	ids := make([]string, len(agents))
	for i, a := range agents {
		ids[i] = a.ID
	}
	return ids
}

// AgentParameter is a delegation tool's optional agent parameter: a closed enum of the eligible ids,
// so the model can only name an agent the launch kind may run.
func AgentParameter(agents []config.AgentInfo, defaultID string) map[string]any {
	return map[string]any{
		"type":        "string",
		"enum":        AgentIDs(agents),
		"description": "the agent that runs it; omit for " + defaultID,
	}
}

// SubagentController runs spawn_subagent's children. agentID is a catalog agent the loop's tool
// checked against the eligible list (read-only ceiling, delegation use).
type SubagentController interface {
	Spawn(ctx context.Context, goal, agentID string) (any, error)
	List(ctx context.Context) (any, error)
	Wait(ctx context.Context, childSessionID string, timeout time.Duration) (any, error)
}

func (s *Set) registerSubagentTools() {
	if s.subagents == nil {
		return
	}
	description := "Spawn one bounded read-only child agent for a focused research, search, diagnostic, or analysis task. Child agents cannot modify files or recursively spawn more agents."
	properties := map[string]any{"goal": str()}
	if len(s.subagentAgents) > 0 {
		description += " Agents that can run it:" + AgentCatalogLines(s.subagentAgents)
		properties["agent"] = AgentParameter(s.subagentAgents, config.AgentExplore)
	}
	s.add(protocol.ToolSpec{
		Name:        "spawn_subagent",
		Description: description,
		Permission:  "read",
		Parameters:  obj(properties, "goal"),
	}, s.spawnSubagent)
	s.add(protocol.ToolSpec{
		Name:        "list_subagents",
		Description: "List durable child-agent sessions and their current statuses for this task.",
		Permission:  "read",
		Parameters:  obj(map[string]any{}),
	}, s.listSubagents)
	s.add(protocol.ToolSpec{
		Name:        "wait_subagent",
		Description: "Wait up to a bounded timeout for one direct child agent and return its latest durable result. Use this after spawn_subagent when the parent needs the child's findings before continuing.",
		Permission:  "read",
		Parameters: obj(map[string]any{
			"child_session_id": str(),
			"timeout_seconds":  map[string]any{"type": "integer", "minimum": 1, "maximum": 60},
		}, "child_session_id"),
	}, s.waitSubagent)
}

func (s *Set) spawnSubagent(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	goal := strings.TrimSpace(stringArg(args, "goal"))
	if goal == "" {
		return protocol.ToolResult{}, errors.New("subagent goal is required")
	}
	if len(goal) > MaxSubagentGoalBytes {
		return protocol.ToolResult{}, errors.New("subagent goal exceeds 16 KiB limit")
	}
	// An agent outside the enum is refused before a child exists (AG5): no silent fallback to explore.
	agentID := strings.TrimSpace(stringArg(args, "agent"))
	if agentID == "" {
		agentID = config.AgentExplore
	} else if !slices.Contains(AgentIDs(s.subagentAgents), agentID) {
		return protocol.ToolResult{}, fmt.Errorf("spawn_subagent cannot run agent %q; choose one of: %s",
			agentID, strings.Join(AgentIDs(s.subagentAgents), ", "))
	}
	result, err := s.subagents.Spawn(ctx, goal, agentID)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

func (s *Set) listSubagents(ctx context.Context, _ map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	result, err := s.subagents.List(ctx)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

func (s *Set) waitSubagent(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	childID := strings.TrimSpace(stringArg(args, "child_session_id"))
	if childID == "" {
		return protocol.ToolResult{}, errors.New("child_session_id is required")
	}
	seconds := intArg(args, "timeout_seconds", 30)
	if seconds < 1 {
		seconds = 30
	}
	if seconds > 60 {
		seconds = 60
	}
	result, err := s.subagents.Wait(ctx, childID, time.Duration(seconds)*time.Second)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}
