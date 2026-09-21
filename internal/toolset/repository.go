package toolset

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edynasty/LumenCortex/protocol"
)

func intArg(args map[string]any, key string, fallback int) int {
	value, ok := args[key]
	if !ok {
		return fallback
	}
	switch n := value.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return fallback
	}
}

func boolArg(args map[string]any, key string) bool {
	value, _ := args[key].(bool)
	return value
}

func (s *Set) searchText(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	result, err := s.repository.SearchText(ctx, fmt.Sprint(args["query"]), fmt.Sprint(args["path"]), intArg(args, "limit", 200))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, _ := json.Marshal(result)
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

func (s *Set) findFiles(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	result, err := s.repository.FindFiles(ctx, fmt.Sprint(args["pattern"]), fmt.Sprint(args["path"]), intArg(args, "limit", 200))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, _ := json.Marshal(result)
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

func (s *Set) gitStatus(ctx context.Context, _ map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	result, err := s.repository.GitStatus(ctx)
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, _ := json.Marshal(result)
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}

func (s *Set) gitDiff(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	result, err := s.repository.GitDiff(ctx, fmt.Sprint(args["path"]), boolArg(args, "staged"))
	if err != nil {
		return protocol.ToolResult{}, err
	}
	raw, _ := json.Marshal(result)
	return protocol.ToolResult{OK: true, Content: string(raw)}, nil
}
