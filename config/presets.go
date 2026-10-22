package config

import (
	"embed"
	"slices"
	"strings"
)

// AgentPresets are the specialist definitions an editor may copy into the catalog (ADR-0009 A4).
// They are not catalog entries: Core never runs one until the user saves it into lumencortex.json,
// and the saved copy is ordinary configuration the user owns. The instructions bodies are compiled
// into the binary; Origin and OMPRole name the oh-my-pi agent and model role each derives from (see
// THIRD_PARTY_NOTICES.md at the repository root).
//
//go:embed presets/*.md
var presetFiles embed.FS

// AgentPreset is one compiled-in specialist. Agent carries the suggested entry: Name, Description,
// Use, Policy, Instructions and Effort, with Models nil (inherit) until ResolveAgentPresets fills
// it from the OMP model role. ID is the suggested catalog id.
type AgentPreset struct {
	ID      string
	Agent   Agent
	Origin  string
	OMPRole string
}

// presetDef is the compiled-in metadata of one preset; the file holds only the instructions body.
type presetDef struct {
	id, file, name, description, origin, role string
	use                                       AgentUse
	policy                                    string
	effort                                    Effort
}

// presetDefs are the presets in catalog order. Descriptions stay well under
// MaxAgentDescriptionSize because Core validates the saved copy again.
var presetDefs = []presetDef{
	{
		id: "reviewer", file: "presets/reviewer.md", name: "Reviewer",
		description: "Read-only review of the current change: verdict, confidence and findings with priority and file:line.",
		origin:      "reviewer", role: "slow", use: AgentUseBoth, policy: PolicyReadOnly, effort: EffortHigh,
	},
	{
		id: "security-reviewer", file: "presets/security-reviewer.md", name: "Security Reviewer",
		description: "Read-only security review: traces attacker sources to sinks, with rule ids, CWE, severity, evidence and remediation.",
		origin:      "security-reviewer", use: AgentUseBoth, policy: PolicyReadOnly, effort: EffortHigh,
	},
	{
		id: "sonic", file: "presets/sonic.md", name: "Sonic",
		description: "Strictly mechanical edits or data collection with explicit instructions; it makes no scope decisions.",
		origin:      "sonic", role: "smol", use: AgentUseDelegation, policy: PolicyWorkspace, effort: EffortLow,
	},
	{
		id: "thread-historian", file: "presets/thread-historian.md", name: "Thread Historian",
		description: "Reads one historical thread and answers from its evidence, citing the thread and message context.",
		origin:      "seance", use: AgentUseDelegation, policy: PolicyReadOnly, effort: EffortMedium,
	},
}

// AgentPresets returns fresh copies of the compiled-in specialists. The instructions body comes
// from the embedded file; every other field is metadata here. A missing embedded file is a build
// defect, so it panics rather than silently dropping a preset.
func AgentPresets() []AgentPreset {
	out := make([]AgentPreset, 0, len(presetDefs))
	for _, d := range presetDefs {
		body, err := presetFiles.ReadFile(d.file)
		if err != nil {
			panic("config: embedded preset " + d.file + ": " + err.Error())
		}
		out = append(out, AgentPreset{
			ID: d.id,
			Agent: Agent{
				Name:         d.name,
				Description:  d.description,
				Use:          d.use,
				Policy:       d.policy,
				Instructions: strings.TrimRight(string(body), "\n"),
				Effort:       d.effort,
			},
			Origin:  d.origin,
			OMPRole: d.role,
		})
	}
	return out
}

// ResolveAgentPresets returns the presets with their model lists filled from the OMP model roles.
// A preset without an OMP role, or whose role converts to nothing, keeps Models nil and inherits
// the default list when the user saves it. Conversion problems are warnings, never errors: the
// presets stay usable without an OMP installation.
func ResolveAgentPresets(eff *Effective, ompConfigPath string) ([]AgentPreset, []string) {
	presets := AgentPresets()
	if ompConfigPath == "" {
		return presets, nil
	}
	roles, warnings, err := OMPRoleModels(ompConfigPath, eff)
	if err != nil {
		return presets, append(warnings, "oh-my-pi config "+ompConfigPath+": "+err.Error())
	}
	for i := range presets {
		if presets[i].OMPRole == "" {
			continue
		}
		if models := roles[presets[i].OMPRole]; len(models) > 0 {
			presets[i].Agent.Models = slices.Clone(models)
		}
	}
	return presets, warnings
}
