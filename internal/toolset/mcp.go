package toolset

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edynasty/LumenCortex/protocol"
)

func (s *Set) registerMCPTools() {
	bindings, err := s.mcp.AgentTools(s.workspace)
	if err != nil {
		return
	}
	for _, binding := range bindings {
		binding := binding
		permission := "exec"
		if binding.ReadOnly {
			permission = "read"
		}
		description := binding.Description
		if description == "" {
			description = fmt.Sprintf("MCP tool %s from server %s.", binding.ToolName, binding.ServerID)
		} else {
			description = fmt.Sprintf("%s (MCP server: %s)", description, binding.ServerID)
		}
		params := binding.InputSchema
		if params == nil {
			params = map[string]any{"type": "object"}
		}
		s.add(protocol.ToolSpec{
			Name:        binding.Name,
			Description: description,
			Permission:  permission,
			Parameters:  params,
		}, func(ctx context.Context, args map[string]any, _ func(protocol.ToolOutput)) (protocol.ToolResult, error) {
			result, err := s.mcp.CallTool(ctx, s.workspace, binding.ServerID, binding.ToolName, args)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			raw, err := json.Marshal(result)
			if err != nil {
				return protocol.ToolResult{}, err
			}
			return protocol.ToolResult{
				OK:      !result.IsError,
				Content: string(raw),
			}, nil
		})
	}
}
