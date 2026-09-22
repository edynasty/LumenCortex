package runtime

import (
	"context"
	"errors"

	"github.com/edynasty/LumenCortex/internal/mcp"
)

type MCPConfig = mcp.Config
type MCPStatus = mcp.Status
type MCPAgentTool = mcp.AgentTool

func (e *Engine) MCPConfigs() []MCPConfig {
	if e.mcpRegistry == nil {
		return []MCPConfig{}
	}
	return e.mcpRegistry.Configs()
}

func (e *Engine) MCPConfigsScope(scope string) ([]MCPConfig, error) {
	if e.mcpRegistry == nil {
		return []MCPConfig{}, nil
	}
	return e.mcpRegistry.ConfigsScope(scope)
}

func (e *Engine) MCPUpsertConfig(cfg MCPConfig) error {
	return e.MCPUpsertConfigScope(mcp.ScopeProject, cfg)
}

func (e *Engine) MCPUpsertConfigScope(scope string, cfg MCPConfig) error {
	if e.mcpRegistry == nil {
		return errors.New("mcp registry is unavailable")
	}
	return e.mcpRegistry.UpsertScope(scope, cfg)
}

func (e *Engine) MCPDeleteConfig(id string) error {
	return e.MCPDeleteConfigScope(mcp.ScopeProject, id)
}

func (e *Engine) MCPDeleteConfigScope(scope, id string) error {
	if e.mcpRegistry == nil {
		return errors.New("mcp registry is unavailable")
	}
	return e.mcpRegistry.DeleteScope(scope, id)
}

func (e *Engine) MCPStart(ctx context.Context, sessionID, serverID string) (MCPStatus, error) {
	if e.mcpRegistry == nil {
		return MCPStatus{}, errors.New("mcp registry is unavailable")
	}
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return MCPStatus{}, err
	}
	status, err := e.mcpRegistry.Start(ctx, serverID, workspace)
	if err == nil {
		e.events.publish("mcp.started", sessionID, map[string]any{
			"serverId": serverID,
			"workspace": workspace,
			"pid": status.PID,
			"tools": status.Tools,
		})
	}
	return status, err
}

func (e *Engine) MCPStop(ctx context.Context, sessionID, serverID string) error {
	if e.mcpRegistry == nil {
		return errors.New("mcp registry is unavailable")
	}
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := e.mcpRegistry.Stop(serverID, workspace); err != nil {
		return err
	}
	e.events.publish("mcp.stopped", sessionID, map[string]any{
		"serverId": serverID,
		"workspace": workspace,
	})
	return nil
}

func (e *Engine) MCPStatuses(ctx context.Context, sessionID string) ([]MCPStatus, error) {
	if e.mcpRegistry == nil {
		return []MCPStatus{}, nil
	}
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return e.mcpRegistry.Statuses(workspace)
}

func (e *Engine) MCPTools(ctx context.Context, sessionID string) ([]MCPAgentTool, error) {
	if e.mcpRegistry == nil {
		return []MCPAgentTool{}, nil
	}
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return e.mcpRegistry.AgentTools(workspace)
}

func (e *Engine) MCPRefreshTools(ctx context.Context, sessionID, serverID string) ([]mcp.Tool, error) {
	if e.mcpRegistry == nil {
		return nil, errors.New("mcp registry is unavailable")
	}
	workspace, err := e.lspWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return e.mcpRegistry.RefreshTools(ctx, serverID, workspace)
}
