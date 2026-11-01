// Package mcptemplates ships the official MCP server templates Core knows: the shape of a server
// entry (transport, command or URL, curated tool list, limits) plus the human facts a user needs
// before enabling one — what it needs, what it costs, what was verified.
//
// A template is data, not code: each one is a JSON document in templates/ embedded in the binary and
// applied as a disabled mcp.json entry, so enabling is an explicit act by the user (ADR-0021). A
// credential is never part of a template: a header or environment value is an "{env:NAME}" reference,
// resolved only when a request is made or a process starts.
package mcptemplates

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/edynasty/LumenCortex/internal/mcp"
)

//go:embed templates/*.json
var templateFiles embed.FS

// Template is one MCP server template.
type Template struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Capability is the semantic capability the server provides ("web.search", "code.host.read",
	// "browser.control", "computer.control", "code.graph"). Several servers may provide one capability.
	Capability  string `json:"capability"`
	Description string `json:"description"`
	// Homepage is the official project or documentation page.
	Homepage string `json:"homepage"`
	// Auth says how the server authenticates, in words a user can act on.
	Auth string `json:"auth"`
	// Cost warns about external charges, in words; empty means no cost the user must know about.
	Cost string `json:"cost,omitempty"`
	// Requires says what must exist on the machine before the server can start (a binary, a running
	// local daemon, network access), in words.
	Requires string `json:"requires,omitempty"`
	// Verified records what was checked against the upstream source and when, and what was not.
	Verified string `json:"verified"`
	// Experimental marks a template whose upstream server does not meet every Core constraint; the
	// reason is in Description and Limitations.
	Experimental bool `json:"experimental,omitempty"`
	// Limitations are the known constraints of the upstream server, one per entry.
	Limitations []string `json:"limitations,omitempty"`
	// Config is the mcp.json entry the template applies. It is always disabled: enabling is explicit.
	Config mcp.Config `json:"config"`
}

// All returns every template, ordered by id.
func All() []Template {
	out := make([]Template, 0, len(templateIDs()))
	for _, id := range templateIDs() {
		if template, ok := load(id); ok {
			out = append(out, template)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Get returns one template by id.
func Get(id string) (Template, bool) {
	return load(strings.TrimSpace(id))
}

// IDs lists the shipped template ids in file order.
func IDs() []string { return templateIDs() }

func templateIDs() []string {
	entries, err := fs.ReadDir(templateFiles, "templates")
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(entry.Name(), ".json"))
	}
	sort.Strings(ids)
	return ids
}

// ErrUnknownTemplate reports a template id Core does not ship.
var ErrUnknownTemplate = fmt.Errorf("unknown mcp template")

func load(id string) (Template, bool) {
	if !validTemplateID(id) {
		return Template{}, false
	}
	raw, err := templateFiles.ReadFile("templates/" + id + ".json")
	if err != nil {
		return Template{}, false
	}
	var template Template
	if err := json.Unmarshal(raw, &template); err != nil {
		return Template{}, false
	}
	if template.ID != id {
		return Template{}, false
	}
	return template, true
}

func validTemplateID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return false
	}
	return true
}

// Validate checks a template against the runtime's own configuration rules, so a shipped template can
// never describe a server the loader would refuse: the entry must be a valid mcp.Config, disabled, and
// name this template; a credential must be an "{env:NAME}" reference or an absent value.
func Validate(template Template) error {
	if !validTemplateID(template.ID) {
		return fmt.Errorf("template id %q must match [a-z0-9-] and be at most 32 bytes", template.ID)
	}
	if strings.TrimSpace(template.Name) == "" || strings.TrimSpace(template.Description) == "" {
		return fmt.Errorf("template %s: name and description are required", template.ID)
	}
	if strings.TrimSpace(template.Verified) == "" {
		return fmt.Errorf("template %s: the verified note is required", template.ID)
	}
	cfg := template.Config
	if cfg.ID == "" || cfg.ID != template.ID {
		return fmt.Errorf("template %s: the config id must equal the template id", template.ID)
	}
	if !cfg.Disabled {
		return fmt.Errorf("template %s: a template is applied disabled; enabling is explicit", template.ID)
	}
	if cfg.Template != template.ID {
		return fmt.Errorf("template %s: the config must name its template", template.ID)
	}
	if err := mcp.ValidateConfig(cfg); err != nil {
		return fmt.Errorf("template %s: %w", template.ID, err)
	}
	for name, value := range cfg.Headers {
		if err := requireReference("header "+name, value); err != nil {
			return fmt.Errorf("template %s: %w", template.ID, err)
		}
	}
	for name, value := range cfg.Env {
		if err := requireReference("env "+name, value); err != nil {
			return fmt.Errorf("template %s: %w", template.ID, err)
		}
	}
	return nil
}

// requireReference refuses a credential-looking value that is not an "{env:NAME}" reference. Values
// that cannot be a credential (a header name, a toolset list, "true") are allowed; the check is
// deliberately narrow so it catches a pasted token without refusing ordinary configuration.
func requireReference(field, value string) error {
	if strings.Contains(value, "{env:") {
		if !strings.Contains(value, "}") {
			return fmt.Errorf("%s is an unterminated environment reference", field)
		}
		return nil
	}
	lower := strings.ToLower(field)
	credentialLike := strings.Contains(lower, "authorization") || strings.Contains(lower, "token") ||
		strings.Contains(lower, "api-key") || strings.Contains(lower, "apikey") || strings.Contains(lower, "secret") ||
		strings.Contains(lower, "password")
	if credentialLike {
		return fmt.Errorf("%s must be an {env:NAME} reference, never a stored value", field)
	}
	if len(value) >= 20 && looksLikeToken(value) {
		return fmt.Errorf("%s looks like a stored credential; use an {env:NAME} reference", field)
	}
	return nil
}

// looksLikeToken reports a long unbroken run of token characters, the shape of a pasted secret.
func looksLikeToken(value string) bool {
	run := 0
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			run++
			if run >= 20 {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}
