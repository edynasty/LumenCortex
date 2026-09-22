package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

const MaxSubagentGoalBytes = 16 << 10

type SubagentController interface {
	Spawn(ctx context.Context, goal string) (any, error)
	List(ctx context.Context) (any, error)
}

func (s *Set) registerSubagentTools() {
	if s.subagents == nil {
		return
	}
	s.add(protocol.ToolSpec{
		Name:        "spawn_subagent",
		Description: "Spawn one bounded read-only child agent for a focused research, search, diagnostic, or analysis task. Child agents cannot modify files or recursively spawn more agents.",
		Permission:  "read",
		Parameters: obj(map[string]any{
			"goal": str(),
		}, "goal"),
	}, s.spawnSubagent)
	s.add(protocol.ToolSpec{
		Name:        "list_subagents",
		Description: "List durable child-agent sessions and their current statuses for this task.",
		Permission:  "read",
		Parameters:  obj(map[string]any{}),
	}, s.listSubagents)
}

func (s *Set) spawnSubagent(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	goal := strings.TrimSpace(stringArg(args, "goal"))
	if goal == "" {
		return protocol.ToolResult{}, errors.New("subagent goal is required")
	}
	if len(goal) > MaxSubagentGoalBytes {
		return protocol.ToolResult{}, errors.New("subagent goal exceeds 16 KiB limit")
	}
	result, err := s.subagents.Spawn(ctx, goal)
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
