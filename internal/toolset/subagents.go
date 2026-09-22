package toolset

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

const MaxSubagentGoalBytes = 16 << 10

type SubagentController interface {
	Spawn(ctx context.Context, goal string) (any, error)
	List(ctx context.Context) (any, error)
	Wait(ctx context.Context, childSessionID string, timeout time.Duration) (any, error)
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
	s.add(protocol.ToolSpec{
		Name:        "wait_subagent",
		Description: "Wait up to a bounded timeout for one direct child agent and return its latest durable result. Use this after spawn_subagent when the parent needs the child's findings before continuing.",
		Permission:  "read",
		Parameters: obj(map[string]any{
			"child_session_id": str(),
			"timeout_seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 60},
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
