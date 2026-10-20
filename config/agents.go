package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// An agent is a named run preset (ADR-0009): instructions, an ordered list of model candidates
// with their reasoning effort, and an authority ceiling. Core owns the catalog: four built-ins plus
// the custom agents of lumencortex.json. A session pins the agent it runs; the launch kind
// (conversation, read-only subagent, execution task) still fixes what a child may do.

// AgentUse says where an agent runs.
type AgentUse string

const (
	// AgentUseConversation agents run threads the user starts.
	AgentUseConversation AgentUse = "conversation"
	// AgentUseDelegation agents run only as children: spawn_subagent (read-only ceiling) or
	// start_task (workspace ceiling).
	AgentUseDelegation AgentUse = "delegation"
	// AgentUseBoth agents run in either role.
	AgentUseBoth AgentUse = "both"
)

// Authority values an agent may cap. They are the runtime's policy and delegation values; config
// names them so it never imports the runtime.
const (
	PolicyReadOnly     = "read-only"
	PolicyWorkspace    = "workspace"
	PolicyFull         = "full"
	DelegationNone     = "none"
	DelegationPropose  = "propose"
	DelegationIsolated = "isolated"
)

// Built-in agent ids. AgentDefault runs a session that names no agent.
const (
	AgentAsk     = "ask"
	AgentDefault = "agent"
	AgentExplore = "explore"
	AgentWorker  = "worker"
)

// Agent catalog bounds.
const (
	MaxCustomAgents         = 16
	MaxAgentNameChars       = 40
	MaxAgentDescriptionSize = 280
	MaxAgentInstructions    = 16 << 10
	MaxAgentModels          = 8
)

// ErrSubagentModelRetired reports a file that still sets subagentModel. Children now run the
// explore or worker agent, whose models list replaces it (ADR-0009).
var ErrSubagentModelRetired = errors.New(`subagentModel is retired: set "agents": {"explore": {"models": [{"model": "<ref>"}]}, "worker": {"models": [{"model": "<ref>"}]}} instead`)

// MigrateSubagentModel rewrites a file that still sets the retired subagentModel: the key is removed
// and its model becomes the one-entry models list of explore and worker, unless the file already
// gives that agent a list. It reports whether it changed the file; a file without the key, or a
// missing file, is left alone. The rewrite goes through Save with the read digest, so a concurrent
// edit is ErrConflict and nothing is written.
func MigrateSubagentModel(path string) (bool, error) {
	raw, err := readBounded(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("config %s: %w", path, err)
	}
	generic, err := decodeGeneric(bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf")))
	if err != nil {
		return false, fmt.Errorf("config %s: %w", path, err)
	}
	obj, ok := generic.(map[string]any)
	if !ok {
		return false, nil
	}
	value, ok := obj["subagentModel"]
	if !ok {
		return false, nil
	}
	ref, ok := value.(string)
	if !ok {
		return false, fmt.Errorf("config %s: %w: subagentModel is not a string", path, ErrInvalidConfig)
	}
	delete(obj, "subagentModel")
	agents, _ := obj["agents"].(map[string]any)
	if agents == nil {
		agents = map[string]any{}
	}
	for _, id := range []string{AgentExplore, AgentWorker} {
		entry, _ := agents[id].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		if _, set := entry["models"]; !set {
			entry["models"] = []any{map[string]any{"model": ref}}
		}
		agents[id] = entry
	}
	obj["agents"] = agents
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return false, err
	}
	f, err := Parse(rewritten)
	if err != nil {
		return false, fmt.Errorf("config %s: %w", path, err)
	}
	if _, err := Save(path, f, Digest(raw)); err != nil {
		return false, err
	}
	return true, nil
}

// Agent is one entry of a file's agents object. Absent fields mean "not set": a built-in keeps its
// fixed value, a custom agent its default. Built-ins accept only Instructions, Effort and Models.
type Agent struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description,omitempty"`
	Use         AgentUse `json:"use,omitempty"`
	// Policy is the ceiling of the run's tool policy: read-only, workspace or full. Absent is full
	// (no cap) for conversation agents; delegation agents must set read-only or workspace.
	Policy string `json:"policy,omitempty"`
	// Delegation is the ceiling of the start_task grant of a conversation: none, propose or
	// isolated. Absent is isolated (no cap). Delegation-only agents never hold the grant.
	Delegation   string `json:"delegation,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	// Effort is the default effort of every candidate without its own, "auto" included.
	Effort Effort `json:"effort,omitempty"`
	// Models are the agent's ordered candidates. Absent: the agent inherits a list (the default
	// list for conversations, the parent's effective list for children).
	Models []AgentModel `json:"models,omitempty"`
}

// AgentModel is one candidate of an agent's list.
type AgentModel struct {
	Model  ModelRef `json:"model"`
	Effort Effort   `json:"effort,omitempty"`
}

// AgentInfo is one agent of the merged catalog with its fixed and default values applied.
type AgentInfo struct {
	ID           string
	Name         string
	Description  string
	Use          AgentUse
	Policy       string
	Delegation   string
	Instructions string
	Effort       Effort
	// Models is the agent's own list, nil when it inherits one.
	Models  []AgentModel
	Builtin bool
	// Source is where the definition or its last override came from: builtin, global, workspace.
	Source string
	// Digest identifies the definition (every field above except Source): it changes with any edit.
	Digest string
}

// ForConversation reports whether the agent may run a thread the user starts.
func (a AgentInfo) ForConversation() bool { return a.Use != AgentUseDelegation }

// ForDelegation reports whether the agent may run as a child.
func (a AgentInfo) ForDelegation() bool { return a.Use != AgentUseConversation }

// SpawnEligible reports whether spawn_subagent may run the agent: a delegation agent whose ceiling
// is read-only, as every read-only child is.
func (a AgentInfo) SpawnEligible() bool { return a.ForDelegation() && a.Policy == PolicyReadOnly }

// TaskEligible reports whether start_task may run the agent: a delegation agent whose ceiling is
// workspace, the authority every execution task gets.
func (a AgentInfo) TaskEligible() bool { return a.ForDelegation() && a.Policy == PolicyWorkspace }

// builtinAgents are the fixed fields of the built-ins, in catalog order.
var builtinAgents = []AgentInfo{
	{ID: AgentAsk, Name: "Ask", Use: AgentUseConversation, Policy: PolicyReadOnly, Delegation: DelegationNone,
		Description: "Answers questions about the workspace without changing anything."},
	{ID: AgentDefault, Name: "Agent", Use: AgentUseConversation, Policy: PolicyFull, Delegation: DelegationIsolated,
		Description: "Works on the task with the access the thread grants."},
	{ID: AgentExplore, Name: "Explore", Use: AgentUseDelegation, Policy: PolicyReadOnly, Delegation: DelegationNone,
		Description: "Read-only research: searches and reads the workspace and reports what it found."},
	{ID: AgentWorker, Name: "Worker", Use: AgentUseDelegation, Policy: PolicyWorkspace, Delegation: DelegationNone,
		Description: "Changes files in its own isolated worktree to finish one bounded objective; the user applies the result."},
}

// IsBuiltinAgent reports whether id is reserved for a built-in agent.
func IsBuiltinAgent(id string) bool {
	return slices.ContainsFunc(builtinAgents, func(a AgentInfo) bool { return a.ID == id })
}

// validateAgents checks what the schema cannot say about one file's agents: the fixed fields of the
// built-ins, the rules between use, policy and delegation, byte bounds and duplicate candidates.
func validateAgents(agents map[string]Agent) error {
	custom := 0
	for _, id := range sortedKeys(agents) {
		a := agents[id]
		at := func(field string) string { return "agents." + id + "." + field }
		if IsBuiltinAgent(id) {
			for field, set := range map[string]bool{"name": a.Name != "", "description": a.Description != "",
				"use": a.Use != "", "policy": a.Policy != "", "delegation": a.Delegation != ""} {
				if set {
					return invalid("%s: the built-in agent %q accepts only instructions, effort and models", at(field), id)
				}
			}
		} else {
			custom++
			if a.Use != AgentUseConversation && a.Use != "" {
				if a.Policy != PolicyReadOnly && a.Policy != PolicyWorkspace {
					return invalid("%s: an agent used for delegation must cap its policy at read-only or workspace", at("policy"))
				}
				if a.Delegation != "" {
					return invalid("%s: an agent used for delegation never delegates further", at("delegation"))
				}
				if strings.TrimSpace(a.Description) == "" {
					return invalid("%s: an agent used for delegation needs a description: the model selects it by that text", at("description"))
				}
			}
			if a.Name != "" && strings.TrimSpace(a.Name) == "" {
				return invalid("%s must not be blank", at("name"))
			}
		}
		if len(a.Description) > MaxAgentDescriptionSize {
			return invalid("%s is longer than %d bytes", at("description"), MaxAgentDescriptionSize)
		}
		if len(a.Instructions) > MaxAgentInstructions {
			return invalid("%s is longer than %d bytes", at("instructions"), MaxAgentInstructions)
		}
		if !utf8.ValidString(a.Instructions) {
			return invalid("%s is not valid UTF-8", at("instructions"))
		}
		seen := map[ModelRef]bool{}
		for _, m := range a.Models {
			if seen[m.Model] {
				return invalid("%s lists %s twice", at("models"), m.Model)
			}
			seen[m.Model] = true
		}
	}
	if custom > MaxCustomAgents {
		return invalid("agents defines %d custom agents; at most %d", custom, MaxCustomAgents)
	}
	return nil
}

// mergeAgents applies the workspace's agents over the global ones (design A8, ADR-0009):
//
//   - a trusted workspace replaces or adds a custom agent by id and overrides a built-in's
//     instructions, effort and models field by field;
//   - an untrusted workspace may set only models and effort of an agent that already exists, and
//     only to models of global providers; any other entry is dropped with a warning.
//
// providers is the global catalog when the workspace is untrusted.
func (c *Config) mergeAgents(providers map[string]Provider, drop func(key, what string)) (map[string]Agent, map[string]string) {
	defs, sources := map[string]Agent{}, map[string]string{}
	for id, a := range c.global.File.Agents {
		defs[id], sources[id] = cloneAgent(a), "global"
	}
	for _, id := range sortedKeys(c.workspace.File.Agents) {
		wa := cloneAgent(c.workspace.File.Agents[id])
		base, exists := defs[id]
		exists = exists || IsBuiltinAgent(id)
		if !c.eff.Trusted {
			onlyModels := wa.Name == "" && wa.Description == "" && wa.Use == "" && wa.Policy == "" &&
				wa.Delegation == "" && wa.Instructions == ""
			refs := make([]ModelRef, len(wa.Models))
			for i, m := range wa.Models {
				refs[i] = m.Model
			}
			if !exists || !onlyModels || !allModels(providers, refs) {
				drop("agents."+id, "agent "+id)
				continue
			}
		}
		if IsBuiltinAgent(id) || !c.eff.Trusted {
			// Field by field: what the workspace sets wins.
			if wa.Instructions != "" {
				base.Instructions = wa.Instructions
			}
			if wa.Effort != "" {
				base.Effort = wa.Effort
			}
			if wa.Models != nil {
				base.Models = wa.Models
			}
			wa = base
		}
		defs[id], sources[id] = wa, "workspace"
	}
	return defs, sources
}

// agentCatalog builds the merged catalog: the built-ins with their overrides, then the custom
// agents by id. Every model reference must be in the merged catalog, an agent's first model must
// call tools, and names are unique regardless of case.
func agentCatalog(defs map[string]Agent, sources map[string]string, providers map[string]Provider) ([]AgentInfo, error) {
	out := make([]AgentInfo, 0, len(builtinAgents)+len(defs))
	for _, b := range builtinAgents {
		info := b
		info.Builtin, info.Source = true, "builtin"
		if a, ok := defs[b.ID]; ok {
			info.Instructions, info.Effort, info.Models = a.Instructions, a.Effort, a.Models
			info.Source = sources[b.ID]
		}
		out = append(out, info)
	}
	for _, id := range sortedKeys(defs) {
		if IsBuiltinAgent(id) {
			continue
		}
		a := defs[id]
		info := AgentInfo{ID: id, Name: a.Name, Description: a.Description, Use: a.Use, Policy: a.Policy,
			Delegation: a.Delegation, Instructions: a.Instructions, Effort: a.Effort, Models: a.Models, Source: sources[id]}
		if info.Name == "" {
			info.Name = id
		}
		if info.Use == "" {
			info.Use = AgentUseConversation
		}
		if info.Policy == "" {
			info.Policy = PolicyFull
		}
		switch {
		case info.Use == AgentUseDelegation:
			info.Delegation = DelegationNone
		case info.Delegation == "":
			info.Delegation = DelegationIsolated
		}
		out = append(out, info)
	}
	names := map[string]string{}
	for i := range out {
		a := &out[i]
		key := strings.ToLower(a.Name)
		if other, dup := names[key]; dup {
			return nil, invalid("agents %q and %q have the same name %q", other, a.ID, a.Name)
		}
		names[key] = a.ID
		for j, m := range a.Models {
			if !hasModel(providers, m.Model) {
				return nil, fmt.Errorf("%w: agents.%s.models[%d] %q: %w", ErrInvalidConfig, a.ID, j, m.Model, ErrUnknownModel)
			}
		}
		if len(a.Models) > 0 {
			pid, key, _ := a.Models[0].Model.Split()
			if tc := providers[pid].Models[key].ToolCalling; tc != nil && !*tc {
				return nil, invalid("agents.%s.models[0] %q does not call tools; an agent's first model must", a.ID, a.Models[0].Model)
			}
		}
		a.Digest = agentDigest(*a)
	}
	return out, nil
}

func agentDigest(a AgentInfo) string {
	raw, err := json.Marshal(struct {
		ID, Name, Description, Policy, Delegation, Instructions string
		Use                                                     AgentUse
		Effort                                                  Effort
		Models                                                  []AgentModel
	}{a.ID, a.Name, a.Description, a.Policy, a.Delegation, a.Instructions, a.Use, a.Effort, a.Models})
	if err != nil {
		panic(err) // plain strings and slices always encode
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func cloneAgent(a Agent) Agent {
	a.Models = slices.Clone(a.Models)
	return a
}

func cloneAgents(in map[string]Agent) map[string]Agent {
	if in == nil {
		return nil
	}
	out := make(map[string]Agent, len(in))
	for id, a := range in {
		out[id] = cloneAgent(a)
	}
	return out
}

func cloneAgentInfos(in []AgentInfo) []AgentInfo {
	out := slices.Clone(in)
	for i := range out {
		out[i].Models = slices.Clone(out[i].Models)
	}
	return out
}

// DelegationAgents are the catalog agents eligible says a launch kind may run, in catalog order:
// SpawnEligible for spawn_subagent, TaskEligible for start_task (ADR-0009 A3).
func (e Effective) DelegationAgents(eligible func(AgentInfo) bool) []AgentInfo {
	var out []AgentInfo
	for _, a := range e.Agents {
		if eligible(a) {
			out = append(out, a)
		}
	}
	return out
}

// Agent returns the catalog agent with id.
func (e Effective) Agent(id string) (AgentInfo, bool) {
	i := slices.IndexFunc(e.Agents, func(a AgentInfo) bool { return a.ID == id })
	if i < 0 {
		return AgentInfo{}, false
	}
	a := e.Agents[i]
	a.Models = slices.Clone(a.Models)
	return a, true
}
