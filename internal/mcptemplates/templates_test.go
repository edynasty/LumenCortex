package mcptemplates

import (
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/internal/mcp"
)

// Every shipped template is a valid, disabled mcp.json entry that names itself and carries no
// credential.
func TestShippedTemplatesAreValid(t *testing.T) {
	templates := All()
	if len(templates) == 0 {
		t.Fatal("no templates shipped")
	}
	for _, template := range templates {
		if err := Validate(template); err != nil {
			t.Fatalf("%s: %v", template.ID, err)
		}
		if template.Homepage == "" || template.Auth == "" || template.Capability == "" {
			t.Fatalf("%s: homepage, auth and capability are required", template.ID)
		}
		if strings.Contains(strings.ToLower(template.Verified), "verified:") {
			t.Fatalf("%s: the verified note must state what was and was not run", template.ID)
		}
	}
	// The five capability templates the runtime ships.
	want := map[string]bool{"exa": false, "github": false, "browser": false, "computer-use": false, "codegraph": false}
	for _, template := range templates {
		if _, ok := want[template.ID]; ok {
			want[template.ID] = true
		}
	}
	for id, found := range want {
		if !found {
			t.Fatalf("template %s is missing", id)
		}
	}
}

// A template with a stored credential is refused, and a reference is accepted.
func TestTemplateCredentialMustBeAReference(t *testing.T) {
	template, ok := Get("github")
	if !ok {
		t.Fatal("github template missing")
	}
	stored := template
	stored.Config.Headers = map[string]string{"Authorization": "Bearer ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := Validate(stored); err == nil {
		t.Fatal("a stored token must be refused")
	}
	referenced := template
	referenced.Config.Headers = map[string]string{"Authorization": "Bearer {env:GITHUB_TOKEN}"}
	if err := Validate(referenced); err != nil {
		t.Fatalf("an environment reference must be accepted: %v", err)
	}
	// A non-credential header value is not refused for being long.
	ordinary := template
	ordinary.Config.Headers = map[string]string{"X-MCP-Toolsets": "repos,issues,pull_requests,actions"}
	if err := Validate(ordinary); err != nil {
		t.Fatalf("an ordinary header must be accepted: %v", err)
	}
}

// A template is always applied disabled, and enabling it is a separate step.
func TestTemplateMustBeDisabled(t *testing.T) {
	template, ok := Get("browser")
	if !ok {
		t.Fatal("browser template missing")
	}
	enabled := template
	enabled.Config.Disabled = false
	if err := Validate(enabled); err == nil {
		t.Fatal("an enabled template must be refused")
	}
}

// An unknown id is reported, never silently ignored.
func TestUnknownTemplate(t *testing.T) {
	if _, ok := Get("nope"); ok {
		t.Fatal("an unknown template resolved")
	}
	if _, ok := Get("../templates/browser"); ok {
		t.Fatal("a path was accepted as a template id")
	}
}

// The describe view names the endpoint, the tools and the verification for a host to show.
func TestDescribeAll(t *testing.T) {
	describes := DescribeAll()
	if len(describes) != len(All()) {
		t.Fatalf("describe = %d, templates = %d", len(describes), len(All()))
	}
	for _, describe := range describes {
		if describe.Endpoint == "" || describe.Verified == "" {
			t.Fatalf("%s: endpoint and verified note are required", describe.ID)
		}
	}
	computerUse := describeByID(t, describes, "computer-use")
	if !strings.Contains(computerUse.Endpoint, "unix://") {
		t.Fatalf("computer-use endpoint = %q, want the local socket", computerUse.Endpoint)
	}
	github := describeByID(t, describes, "github")
	if !strings.HasPrefix(github.Endpoint, "https://api.githubcopilot.com/mcp/") {
		t.Fatalf("github endpoint = %q", github.Endpoint)
	}
	exa := describeByID(t, describes, "exa")
	if !strings.HasPrefix(exa.Endpoint, "https://mcp.exa.ai/mcp") {
		t.Fatalf("exa endpoint = %q", exa.Endpoint)
	}
	if len(exa.Tools) != 2 {
		t.Fatalf("exa default tools = %v, want search and fetch only", exa.Tools)
	}
	codegraph := describeByID(t, describes, "codegraph")
	if !codegraph.Experimental {
		t.Fatal("codegraph must be marked experimental")
	}
}

func describeByID(t *testing.T, describes []Describe, id string) Describe {
	t.Helper()
	for _, describe := range describes {
		if describe.ID == id {
			return describe
		}
	}
	t.Fatalf("template %s not described", id)
	return Describe{}
}

// Every template's include list names only tools the template's own config could expose, and each
// included tool that the template overrides carries a permission the runtime can enforce.
func TestTemplateIncludeAndPermissions(t *testing.T) {
	for _, template := range All() {
		cfg := template.Config
		include := map[string]bool{}
		for _, name := range cfg.Include {
			include[name] = true
		}
		for name, override := range cfg.Tools {
			if !include[name] {
				t.Fatalf("%s: tools[%q] overrides a tool the include list does not expose", template.ID, name)
			}
			switch override.Permission {
			case mcp.PermissionRead, mcp.PermissionWrite, mcp.PermissionExec:
			default:
				t.Fatalf("%s: tools[%q].permission = %q", template.ID, name, override.Permission)
			}
		}
	}
}
