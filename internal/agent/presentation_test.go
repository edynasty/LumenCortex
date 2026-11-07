package agent

import (
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/protocol"
)

// A model profile changes presentation only: the same tool keeps its schema, permission and
// mutating flag, so two models can do exactly the same things.
func TestPresentSpecsChangesWordingNotAuthority(t *testing.T) {
	parameters := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string"},
		},
		"required": []any{"path"},
	}
	specs := []protocol.ToolSpec{{
		Name: "write_file", Description: "Create or replace a UTF-8 file inside the workspace. This second sentence is dropped by the compact profile.",
		Permission: "write", MutatesWorkspace: true, Parameters: parameters,
	}}
	detailed := presentSpecs(specs, config.ProfileDetailed)
	if len(detailed) != 1 || detailed[0].Description != specs[0].Description {
		t.Fatalf("detailed changed the catalog: %+v", detailed)
	}
	compact := presentSpecs(specs, config.ProfileCompact)
	if len(compact) != 1 {
		t.Fatalf("compact=%+v", compact)
	}
	if compact[0].Description == specs[0].Description || !strings.Contains(compact[0].Description, "UTF-8 file") {
		t.Fatalf("compact description=%q", compact[0].Description)
	}
	if strings.Contains(compact[0].Description, "second sentence") {
		t.Fatalf("compact kept the whole text: %q", compact[0].Description)
	}
	if compact[0].Name != "write_file" || compact[0].Permission != "write" || !compact[0].MutatesWorkspace {
		t.Fatalf("compact changed authority: %+v", compact[0])
	}
	if got, want := compact[0].Parameters, specs[0].Parameters; len(got) != len(want) || got["type"] != want["type"] {
		t.Fatalf("compact changed the schema: %v", got)
	}
	// An empty variant is the detailed form.
	if got := presentSpecs(specs, ""); got[0].Description != specs[0].Description {
		t.Fatalf("empty variant=%q", got[0].Description)
	}
}

func TestPresentFeedbackCompactDropsTails(t *testing.T) {
	detailed := "Completion gate rejected completion.\n- go-test: failed\n  output tail:\n```\nFAIL\n```\nFix the cause and propose completion again."
	compact := presentFeedback(detailed, config.ProfileCompact)
	if strings.Contains(compact, "FAIL") || strings.Contains(compact, "```") {
		t.Fatalf("compact kept a diagnostic tail: %q", compact)
	}
	if !strings.Contains(compact, "go-test") || !strings.Contains(compact, "Fix the cause") {
		t.Fatalf("compact dropped the reason: %q", compact)
	}
	if got := presentFeedback(detailed, config.ProfileDetailed); got != detailed {
		t.Fatalf("detailed changed feedback: %q", got)
	}
	if got := presentFeedback(detailed, ""); got != detailed {
		t.Fatalf("empty variant changed feedback: %q", got)
	}
	long := strings.Repeat("x", CompactFeedbackBytes*2)
	if got := presentFeedback(long, config.ProfileCompact); len(got) > CompactFeedbackBytes+8 {
		t.Fatalf("compact feedback is %d bytes", len(got))
	}
}
