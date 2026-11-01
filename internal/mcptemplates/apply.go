package mcptemplates

import (
	"fmt"
	"strings"

	"github.com/edynasty/LumenCortex/internal/mcp"
)

// Apply writes a template into the MCP configuration of a scope as a disabled entry and returns what
// it wrote. Enabling is a separate, explicit step: a template never turns a server on, and an existing
// entry with the same id is never overwritten, so a user's edit is not lost by applying a template
// twice.
func Apply(registry *mcp.Registry, id, scope string) (mcp.Config, error) {
	template, ok := Get(id)
	if !ok {
		return mcp.Config{}, fmt.Errorf("%w: %q", ErrUnknownTemplate, id)
	}
	if err := Validate(template); err != nil {
		return mcp.Config{}, err
	}
	if scope != mcp.ScopeGlobal && scope != mcp.ScopeProject {
		return mcp.Config{}, fmt.Errorf("mcp template scope must be %q or %q", mcp.ScopeGlobal, mcp.ScopeProject)
	}
	existing, err := registry.ConfigsScope(scope)
	if err != nil {
		return mcp.Config{}, err
	}
	for _, cfg := range existing {
		if cfg.ID != template.ID {
			continue
		}
		if cfg.Template == template.ID {
			return mcp.Config{}, fmt.Errorf("mcp template %q is already applied in scope %q", id, scope)
		}
		return mcp.Config{}, fmt.Errorf("mcp server %q is already configured in scope %q", id, scope)
	}
	cfg := template.Config
	cfg.Disabled = true
	cfg.Template = template.ID
	if err := registry.UpsertScope(scope, cfg); err != nil {
		return mcp.Config{}, err
	}
	return cfg, nil
}

// Describe is the human summary a CLI or a Plugins page shows for a template.
type Describe struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Capability   string   `json:"capability"`
	Transport    string   `json:"transport"`
	Endpoint     string   `json:"endpoint"`
	Tools        []string `json:"tools"`
	Auth         string   `json:"auth"`
	Cost         string   `json:"cost,omitempty"`
	Requires     string   `json:"requires,omitempty"`
	Verified     string   `json:"verified"`
	Experimental bool     `json:"experimental,omitempty"`
	Limitations  []string `json:"limitations,omitempty"`
}

// Describe summarises every template: what it needs, what it exposes and what was verified.
func DescribeAll() []Describe {
	templates := All()
	out := make([]Describe, 0, len(templates))
	for _, template := range templates {
		cfg := template.Config
		endpoint := cfg.URL
		if cfg.Transport == "" || cfg.Transport == mcp.TransportStdio {
			endpoint = strings.TrimSpace(cfg.Command + " " + strings.Join(cfg.Args, " "))
		}
		tools := cfg.Include
		if len(tools) == 0 {
			tools = nil
		}
		out = append(out, Describe{
			ID:           template.ID,
			Name:         template.Name,
			Capability:   template.Capability,
			Transport:    string(cfg.Transport),
			Endpoint:     endpoint,
			Tools:        tools,
			Auth:         template.Auth,
			Cost:         template.Cost,
			Requires:     template.Requires,
			Verified:     template.Verified,
			Experimental: template.Experimental,
			Limitations:  template.Limitations,
		})
	}
	return out
}
