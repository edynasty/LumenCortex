package runtime

import (
	"context"
	"errors"

	"github.com/edynasty/LumenCortex/internal/mcptemplates"
)

// MCPTemplate is one official MCP server template Core ships: its shape, what it needs, what it costs
// and what was verified. Templates are data (internal/mcptemplates/templates/*.json), applied as
// disabled entries, so a host lists them without any of them being active.
type MCPTemplate = mcptemplates.Describe

// MCPTemplates lists every shipped template.
func MCPTemplates() []MCPTemplate { return mcptemplates.DescribeAll() }

// ApplyMCPTemplate writes a template into the global or project mcp.json as a disabled entry. The user
// enables it afterwards (SetMCPServerEnabled), which is the explicit opt-in a third-party server needs.
func (e *Engine) ApplyMCPTemplate(ctx context.Context, scope, id string) (MCPConfig, error) {
	if e.mcpRegistry == nil {
		return MCPConfig{}, errors.New("mcp registry is unavailable")
	}
	cfg, err := mcptemplates.Apply(e.mcpRegistry, id, scope)
	if err != nil {
		return MCPConfig{}, err
	}
	e.events.publish("mcp.template.applied", "", map[string]any{
		"serverId": cfg.ID,
		"template": cfg.Template,
		"scope":    scope,
		"enabled":  false,
	})
	return cfg, nil
}
