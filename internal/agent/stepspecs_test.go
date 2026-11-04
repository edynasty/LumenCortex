package agent

import (
	"context"
	"slices"
	"testing"

	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// catalogFake is a toolset whose catalog holds tools the policy withholds: Specs applies the policy
// (it returns only visible names), Catalog returns every registered tool with its origin.
type catalogFake struct {
	visible []string
}

func (c catalogFake) Specs(allow []string) []protocol.ToolSpec {
	out := make([]protocol.ToolSpec, 0, len(c.visible))
	for _, name := range c.visible {
		if allow != nil && !slices.Contains(allow, name) {
			continue
		}
		out = append(out, protocol.ToolSpec{Name: name, Permission: "read"})
	}
	return out
}

func (catalogFake) Execute(context.Context, string, map[string]any, func(protocol.ToolOutput)) (protocol.ToolResult, error) {
	return protocol.ToolResult{}, nil
}

func (catalogFake) Catalog() []toolset.CatalogTool {
	return []toolset.CatalogTool{
		{Spec: protocol.ToolSpec{Name: "read_file", Permission: "read"}, Source: "builtin"},
		{Spec: protocol.ToolSpec{Name: "bash", Permission: "exec"}, Source: "builtin"},
		{Spec: protocol.ToolSpec{Name: "lsp_hover", Permission: "read"}, Source: "lsp"},
		{Spec: protocol.ToolSpec{Name: "mcp__helper__lookup", Permission: "read"}, Source: "mcp:helper"},
	}
}

// The report lists every tool a step could offer, names the filter that withheld each one, and its
// included entries are exactly the step's specs.
func TestStepToolsReportsWithheldTools(t *testing.T) {
	tools := catalogFake{visible: []string{"read_file", "lsp_hover", "mcp__helper__lookup"}}
	in := StepToolsInput{Allowlist: []string{"read_file", "lsp_hover", "mcp__helper__lookup", "work_unit_list", "note_write"}, Denylist: []string{"lsp_hover"}}

	specs := StepSpecs(tools, in)
	names := specNames(specs)
	slices.Sort(names)
	if !slices.Equal(names, []string{"mcp__helper__lookup", "note_write", "read_file", "work_unit_list"}) {
		t.Fatalf("step specs=%v", names)
	}

	status := map[string]StepTool{}
	for _, entry := range StepTools(tools, in) {
		status[entry.Spec.Name] = entry
	}
	if got := status["read_file"]; !got.Included || got.Reason != "" || got.Source != "builtin" {
		t.Fatalf("read_file=%+v", got)
	}
	if got := status["bash"]; got.Included || got.Reason != ToolReasonPolicy {
		t.Fatalf("shell=%+v", got)
	}
	if got := status["lsp_hover"]; got.Included || got.Reason != ToolReasonDenylist {
		t.Fatalf("lsp_hover=%+v", got)
	}
	if got := status["mcp__helper__lookup"]; !got.Included || got.Source != "mcp:helper" {
		t.Fatalf("mcp tool=%+v", got)
	}
	if got := status["start_task"]; got.Included || got.Reason != ToolReasonDelegation || got.Source != "delegation" {
		t.Fatalf("start_task=%+v", got)
	}
	if got := status["note_write"]; !got.Included || got.Source != "task" {
		t.Fatalf("note_write=%+v", got)
	}
	if got := status["work_unit_update"]; got.Included || got.Reason != ToolReasonAllowlist {
		t.Fatalf("work_unit_update=%+v", got)
	}

	included := 0
	for _, entry := range status {
		if entry.Included {
			included++
		}
	}
	if included != len(specs) {
		t.Fatalf("included=%d specs=%d", included, len(specs))
	}
}

// A granted delegation offers start_task; a deny entry removes it again.
func TestStepToolsDelegationGrantAndDenylist(t *testing.T) {
	tools := catalogFake{visible: []string{"read_file"}}
	status := map[string]StepTool{}
	for _, entry := range StepTools(tools, StepToolsInput{Delegation: DelegationPropose}) {
		status[entry.Spec.Name] = entry
	}
	if got := status["start_task"]; !got.Included {
		t.Fatalf("start_task=%+v", got)
	}
	status = map[string]StepTool{}
	for _, entry := range StepTools(tools, StepToolsInput{Delegation: DelegationPropose, Denylist: []string{"start_task"}}) {
		status[entry.Spec.Name] = entry
	}
	if got := status["start_task"]; got.Included || got.Reason != ToolReasonDenylist {
		t.Fatalf("denied start_task=%+v", got)
	}
	// Isolated without a task starter offers nothing: child ownership requires a supervisor.
	status = map[string]StepTool{}
	for _, entry := range StepTools(tools, StepToolsInput{Delegation: DelegationIsolated}) {
		status[entry.Spec.Name] = entry
	}
	if got := status["start_task"]; got.Included || got.Reason != ToolReasonDelegation {
		t.Fatalf("isolated start_task=%+v", got)
	}
}

func specNames(specs []protocol.ToolSpec) []string {
	names := make([]string, 0, len(specs))
	for _, spec := range specs {
		names = append(names, spec.Name)
	}
	return names
}
