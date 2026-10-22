package agent

import (
	"slices"
	"sort"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// StepToolsInput is everything a step's tool list depends on besides the toolset itself.
type StepToolsInput struct {
	// Allowlist is the step's allowlist (the host's, narrowed by an active workflow), nil for none.
	Allowlist []string
	// Denylist is the host's denylist: names removed whatever else offers them.
	Denylist []string
	// Delegation is the run's grant, and Tasks whether a task starter backs it: delegation isolated
	// without one offers no start_task.
	Delegation string
	Tasks      bool
	// TaskAgents are the catalog agents start_task may run (ADR-0009 A3).
	TaskAgents []config.AgentInfo
}

// StepSpecs is the tool list of one step: the toolset's own tools (policy and allowlist applied),
// the task tools, the delegation tool, minus the denylist. Loop.Run sends exactly this list, and the
// prompt report is derived from it, so the two cannot disagree.
func StepSpecs(tools Tools, in StepToolsInput) []protocol.ToolSpec {
	specs := tools.Specs(in.Allowlist)
	specs = append(specs, filterTaskToolSpecs(in.Allowlist)...)
	specs = append(specs, delegationSpecs(grantFor(in.Delegation, in.Tasks), in.Allowlist, in.TaskAgents)...)
	return filterDeniedTools(specs, in.Denylist)
}

// StepTool is one tool a step could offer: its spec, its origin, whether the step offers it, and the
// filter that withheld it.
type StepTool struct {
	Spec     protocol.ToolSpec
	Source   string
	Included bool
	// Reason is the filter that withheld the tool, "" for an included one.
	Reason string
}

// The filters that withhold a tool from a step. They are stable strings: the effective prompt
// reports them to the host.
const (
	// ToolReasonPolicy: the run's policy does not allow the tool's permission.
	ToolReasonPolicy = "policy"
	// ToolReasonAllowlist: the step's allowlist does not name the tool.
	ToolReasonAllowlist = "allowlist"
	// ToolReasonDenylist: the host's denylist names the tool.
	ToolReasonDenylist = "denylist"
	// ToolReasonDelegation: start_task without a delegation grant.
	ToolReasonDelegation = "delegation"
)

// ToolCatalog is a Tools that can also list every tool it registered, before the policy and allowlist
// filters. StepTools uses it to name a tool the policy withholds; a Tools without it reports only
// what Specs(nil) returns.
type ToolCatalog interface {
	Tools
	Catalog() []toolset.CatalogTool
}

// StepTools is StepSpecs plus every other tool a step could offer, each marked with whether the step
// offers it and which filter withheld it. The included entries are exactly StepSpecs' list.
func StepTools(tools Tools, in StepToolsInput) []StepTool {
	included := map[string]bool{}
	for _, spec := range StepSpecs(tools, in) {
		included[spec.Name] = true
	}
	// Specs(nil) applies the policy only: a registered tool missing from it was withheld by policy.
	policyVisible := map[string]bool{}
	for _, spec := range tools.Specs(nil) {
		policyVisible[spec.Name] = true
	}
	out := make([]StepTool, 0, len(policyVisible)+8)
	seen := map[string]bool{}
	// registered is the toolset's own set: only those tools have a policy decision.
	registered := map[string]bool{}
	add := func(spec protocol.ToolSpec, source string) {
		if seen[spec.Name] {
			return
		}
		seen[spec.Name] = true
		entry := StepTool{Spec: spec, Source: source, Included: included[spec.Name]}
		if !entry.Included {
			entry.Reason = withheldReason(in, spec.Name, registered[spec.Name], policyVisible[spec.Name])
		}
		out = append(out, entry)
	}
	if catalog, ok := tools.(ToolCatalog); ok {
		for _, entry := range catalog.Catalog() {
			registered[entry.Spec.Name] = true
			add(entry.Spec, entry.Source)
		}
	} else {
		for _, spec := range tools.Specs(nil) {
			registered[spec.Name] = true
			add(spec, "builtin")
		}
	}
	for _, spec := range taskToolSpecs() {
		add(spec, "task")
	}
	add(delegationSpec(grantFor(in.Delegation, in.Tasks), in.TaskAgents), "delegation")
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Name < out[j].Spec.Name })
	return out
}

// withheldReason names the filter that removed name from the step's list: the policy first for a
// toolset tool, then the allowlist, then the denylist, and the missing grant for start_task.
func withheldReason(in StepToolsInput, name string, registered, policyVisible bool) string {
	switch {
	case name == toolStartTask && grantFor(in.Delegation, in.Tasks) == DelegationNone:
		return ToolReasonDelegation
	case registered && !policyVisible:
		return ToolReasonPolicy
	case in.Allowlist != nil && !slices.Contains(in.Allowlist, name):
		return ToolReasonAllowlist
	default:
		return ToolReasonDenylist
	}
}
