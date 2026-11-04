package toolset

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/edynasty/LumenCortex/internal/mcp"
	"github.com/edynasty/LumenCortex/protocol"
)

// mcpErrorContent renders a server failure as the JSON object a model reads: what failed, on which
// server and tool, and why. No credential or environment value reaches it.
func mcpErrorContent(binding mcp.AgentTool, err error) string {
	raw, marshalErr := json.Marshal(map[string]any{
		"error":    err.Error(),
		"serverId": binding.ServerID,
		"tool":     binding.ToolName,
	})
	if marshalErr != nil {
		return `{"error":"mcp tool call failed"}`
	}
	return string(raw)
}

func (s *Set) registerMCPTools() {
	bindings, err := s.mcp.AgentTools(s.workspace)
	if err != nil {
		return
	}
	for _, binding := range bindings {
		binding := binding
		// AgentTools resolves the effective permission: the server annotation, or a user override
		// from a global or trusted-project entry (ADR-0013). Anything unannotated stays exec.
		permission := binding.Permission
		if permission == "" {
			permission = "exec"
			if binding.ReadOnly {
				permission = "read"
			}
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
			result, err := s.mcp.CallToolForSession(ctx, s.workspace, binding.ServerID, binding.ToolName, s.sessionID, args)
			if err != nil {
				// A failing server is a tool result, never a loop failure: the model can read the reason
				// and try something else, and the run continues.
				return protocol.ToolResult{OK: false, Content: mcpErrorContent(binding, err)}, nil
			}
			raw, err := json.Marshal(result)
			if err != nil {
				return protocol.ToolResult{OK: false, Content: mcpErrorContent(binding, err)}, nil
			}
			return protocol.ToolResult{
				OK:      !result.IsError,
				Content: string(raw),
			}, nil
		})
	}
}
