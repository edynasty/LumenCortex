package runtime

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/instructions"
	"github.com/edynasty/LumenCortex/internal/lsp"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/shell"
	"github.com/edynasty/LumenCortex/internal/skills"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/internal/workflow"
	"github.com/edynasty/LumenCortex/protocol"
)

// The kinds of layer a run's prompt is made of, in the order the run sends them (ADR-0013).
const (
	// LayerBuiltin is the base system prompt: the run's purpose and authority, or a host prompt.
	LayerBuiltin = "builtin"
	// LayerGlobalInstructions is the user's <GlobalDir>/AGENTS.md.
	LayerGlobalInstructions = instructions.KindGlobal
	// LayerProjectInstructions is the workspace's AGENTS.md, or its CLAUDE.md.
	LayerProjectInstructions = instructions.KindProject
	// LayerSkills is the enabled skills prompt.
	LayerSkills = "skills"
	// LayerAgentInstructions is the catalog agent's own instructions (ADR-0009).
	LayerAgentInstructions = "agent-instructions"
	// LayerTaskState is the Task State block, which travels as the first user message rather than in
	// the system prefix.
	LayerTaskState = "task-state"
)

// The scopes a layer belongs to: built into the binary, the user's configuration, the workspace, the
// agent's definition, or the session's own state.
const (
	scopeCore    = "core"
	scopeGlobal  = "global"
	scopeProject = "project"
	scopeAgent   = "agent"
	scopeSession = "session"
)

// PromptLayer is one piece of a run's prompt with its provenance. Text is exactly what the layer
// contributes, label included, so a host that concatenates the included layers of one kind of
// message rebuilds the request the run sends. Text is empty when the layer is not included.
type PromptLayer struct {
	// Kind is the layer's role (LayerBuiltin, LayerGlobalInstructions, ...).
	Kind string `json:"kind"`
	// Source names where the text came from: a file path, a directory list, or the agent's id and
	// definition source. Empty when nothing was found.
	Source string `json:"source,omitempty"`
	// Scope is who owns the layer: core, global, project, agent or session.
	Scope string `json:"scope,omitempty"`
	// Included reports whether the layer enters the prompt.
	Included bool `json:"included"`
	// Reason says why a layer is not included: missing, an untrusted workspace, a refusal.
	Reason string `json:"reason,omitempty"`
	// Bytes is the layer's contribution to the prompt: len(Text), 0 when not included.
	Bytes int `json:"bytes"`
	// Text is the layer as the model receives it.
	Text string `json:"text,omitempty"`
	// Truncated reports that the source is larger than the layer's limit and only its beginning is
	// included.
	Truncated bool `json:"truncated,omitempty"`
	// Warning is a visible consequence of Truncated or of a refused source.
	Warning string `json:"warning,omitempty"`
}

// PromptTool is one tool a run's step could offer.
type PromptTool struct {
	Name string `json:"name"`
	// Permission is the tool's permission class: read, write, exec, delegate or state (a tool that
	// writes only the run's own durable state, admitted under every policy).
	Permission string `json:"permission"`
	// Source is where the tool came from: builtin, task, lsp, mcp:<serverID> or delegation.
	Source string `json:"source"`
	// Included reports whether the run offers the tool.
	Included bool `json:"included"`
	// Reason is the filter that withholds the tool: policy, allowlist, denylist or delegation.
	Reason string `json:"reason,omitempty"`
}

// EffectivePromptRequest selects the run to describe: a session (optional), its agent, and the
// authority the host would grant it. It carries the AgentOptions fields that shape the prompt and
// the tool list; a field it does not carry leaves the run's default in place.
type EffectivePromptRequest struct {
	// SessionID is the session the run would serve. With one, the prompt describes the session's
	// pinned agent, its Task State block, its worktree and its thread tools; without one it
	// describes a first run of Agent in the Engine workspace.
	SessionID string `json:"sessionId,omitempty"`
	// Agent is the catalog agent the host would run, "" for the session's pin, else "agent".
	Agent string `json:"agent,omitempty"`
	// Policy and Delegation are the host's grant; the agent's ceilings narrow them, as at a Start.
	Policy     string `json:"policy,omitempty"`
	Delegation string `json:"delegation,omitempty"`
	// ToolAllowlist and ToolDenylist are the host's tool filters; an active workflow narrows the
	// allowlist as it would for the run.
	ToolAllowlist []string `json:"toolAllowlist,omitempty"`
	ToolDenylist  []string `json:"toolDenylist,omitempty"`
	// Interactive marks a conversation turn, whose base prompt differs from a task run's.
	Interactive bool `json:"interactive,omitempty"`
	// Subagents reports that the host starts the run through RunSupervisor.Start, which offers the
	// subagent tools and, with delegation isolated, start_task. A child run has neither.
	Subagents bool `json:"subagents,omitempty"`
}

// EffectivePromptResult is the prompt a run of the request would send, layer by layer and tool by
// tool. It is what the host shows; it is never a second implementation of the prompt.
type EffectivePromptResult struct {
	// Workspace is the Engine workspace the project inputs are read from.
	Workspace string `json:"workspace"`
	// Trusted reports whether the workspace is trusted, which decides whether its instruction file,
	// its skills and its MCP servers are used (ADR-0013).
	Trusted    bool          `json:"trusted"`
	Agent      string        `json:"agent"`
	Policy     string        `json:"policy"`
	Delegation string        `json:"delegation"`
	Layers     []PromptLayer `json:"layers"`
	Tools      []PromptTool  `json:"tools"`
	// Warnings lists what the host should surface: a truncated instruction file, a withheld project
	// layer, an unreadable skill registry.
	Warnings []string `json:"warnings,omitempty"`
}

// promptInput is everything the system prefix is assembled from. It holds no session, takes no lock
// and writes nothing: the same value produces the messages a run sends and the layers a host is
// shown.
type promptInput struct {
	// workspace is the Engine workspace, never a session worktree.
	workspace string
	globalDir string
	// trusted is the trust of the Engine workspace, read from the configuration of this run.
	trusted bool
	// base is the run's base system prompt: its purpose and authority, or a host prompt.
	base              string
	agentID           string
	agentSource       string
	agentInstructions string
	skills            *skills.Registry
}

// assembledPrompt is a run's system prefix with what the runtime records about it.
type assembledPrompt struct {
	Layers    []PromptLayer
	Skills    skills.PromptResult
	SkillsErr error
}

// assemblePrompt builds the run's system prefix in the order the loop sends it: the base prompt, the
// global instructions, the project instructions, the skills prompt and the agent's instructions. It
// is the one place that reads those inputs (ADR-0013): prepareRun takes the loop's messages from it
// and EffectivePrompt reports the same layers, so the prompt a run sends and the prompt a host is
// shown cannot diverge.
func assemblePrompt(in promptInput) assembledPrompt {
	out := assembledPrompt{Layers: make([]PromptLayer, 0, 5)}
	out.Layers = append(out.Layers, PromptLayer{
		Kind: LayerBuiltin, Source: "core", Scope: scopeCore,
		Included: strings.TrimSpace(in.base) != "", Bytes: len(in.base), Text: in.base,
	})
	for _, file := range instructions.Load(in.globalDir, in.workspace, in.trusted) {
		out.Layers = append(out.Layers, instructionLayer(file))
	}
	layer, result, err := skillsLayer(in.skills, in.trusted)
	out.Skills, out.SkillsErr = result, err
	out.Layers = append(out.Layers, layer)
	out.Layers = append(out.Layers, agentLayer(in))
	return out
}

// instructionLayer is one instruction file as a prompt layer, labelled with its source so the model
// can attribute what it reads to the file it came from.
func instructionLayer(file instructions.File) PromptLayer {
	layer := PromptLayer{
		Kind: file.Kind, Source: file.Source, Scope: file.Scope,
		Included: file.Included, Reason: file.Reason,
		Truncated: file.Truncated, Warning: file.Warning,
	}
	if !file.Included {
		return layer
	}
	header := "## Global instructions"
	if file.Kind == instructions.KindProject {
		header = "## Project instructions"
	}
	text := header + " (from " + file.Source + ")\n\n" + file.Text
	if file.Truncated {
		text += "\n\n[truncated at 64 KiB: the rest of this file is not included]"
	}
	layer.Text, layer.Bytes = text, len(text)
	return layer
}

// skillsLayer is the skills prompt as a layer. The project scope is read only in a trusted
// workspace; the withheld project skills are named so the host can explain the absence.
func skillsLayer(registry *skills.Registry, trusted bool) (PromptLayer, skills.PromptResult, error) {
	layer := PromptLayer{Kind: LayerSkills, Scope: scopeGlobal}
	if registry == nil {
		layer.Reason = "no skill registry"
		return layer, skills.PromptResult{}, nil
	}
	result, err := registry.Prompt(skills.PromptOptions{IncludeProject: trusted})
	if err != nil {
		layer.Reason = "unreadable"
		layer.Warning = "the skills could not be read: " + err.Error()
		return layer, skills.PromptResult{}, err
	}
	sources := make([]string, 0, 6)
	for _, root := range registry.Roots(skills.ScopeGlobal) {
		sources = append(sources, root.Path)
	}
	if trusted {
		for _, root := range registry.Roots(skills.ScopeProject) {
			sources = append(sources, root.Path)
		}
		layer.Scope = scopeProject
	}
	layer.Source = strings.Join(sources, ", ")
	if len(result.Withheld) > 0 {
		layer.Warning = "project skills are not loaded because the workspace is not trusted: " + strings.Join(skillIDs(result.Withheld), ", ")
	}
	if result.Text == "" {
		layer.Reason = "no enabled skills"
		return layer, result, nil
	}
	layer.Text = "## Skills (from " + layer.Source + ")\n\n" + result.Text
	layer.Bytes, layer.Included = len(layer.Text), true
	return layer, result, nil
}

// agentLayer is the catalog agent's instructions. They stay unlabelled: ADR-0009 makes them plain
// text appended last, never a replacement for the base prompt.
func agentLayer(in promptInput) PromptLayer {
	layer := PromptLayer{Kind: LayerAgentInstructions, Scope: scopeAgent, Source: "agent:" + in.agentID}
	if in.agentSource != "" {
		layer.Source += " (" + in.agentSource + ")"
	}
	if strings.TrimSpace(in.agentInstructions) == "" {
		layer.Reason = "none"
		return layer
	}
	layer.Text, layer.Bytes, layer.Included = in.agentInstructions, len(in.agentInstructions), true
	return layer
}

// systemLayers is the loop's view of an assembled prompt: every included layer between the base
// prompt and the agent's instructions, in order.
func systemLayers(layers []PromptLayer) []agent.SystemLayer {
	out := make([]agent.SystemLayer, 0, len(layers))
	for _, layer := range layers {
		switch layer.Kind {
		case LayerBuiltin, LayerAgentInstructions, LayerTaskState:
			// The base prompt travels as Options.SystemPrompt, the agent's instructions as
			// Options.Instructions, and the Task State block is a user message.
			continue
		}
		if !layer.Included {
			continue
		}
		out = append(out, agent.SystemLayer{Kind: layer.Kind, Source: layer.Source, Text: layer.Text})
	}
	return out
}

// promptWarnings collects what the host should surface about an assembled prompt.
func promptWarnings(layers []PromptLayer) []string {
	var out []string
	for _, layer := range layers {
		if layer.Warning != "" {
			out = append(out, layer.Warning)
		}
	}
	return out
}

func skillIDs(items []skills.Skill) []string {
	ids := make([]string, 0, len(items))
	for _, skill := range items {
		ids = append(ids, skill.ID)
	}
	return ids
}

// EffectivePrompt reports the prompt a run of req would send: the layers of its system prefix, the
// Task State block, and every tool its step could offer with the filter that withholds each one. It
// describes the run, it does not start one: it takes no run lock, reconciles nothing, resolves no
// automatic effort, builds no provider client and writes no row. It reads configuration, the
// instruction files, the skill registry and the session's Task State.
func (e *Engine) EffectivePrompt(ctx context.Context, req EffectivePromptRequest) (EffectivePromptResult, error) {
	cfg, err := config.Load(e.workspace)
	if err != nil {
		return EffectivePromptResult{}, err
	}
	eff := cfg.Effective()
	delegation, err := normalizeDelegation(req.Delegation)
	if err != nil {
		return EffectivePromptResult{}, err
	}
	policy := req.Policy
	if policy == "" {
		policy = toolset.PolicyReadOnly
	}
	sessionID := strings.TrimSpace(req.SessionID)
	var session session.Session
	child, childKind, childPolicy, childAgent := false, "", "", ""
	if sessionID != "" {
		if session, err = e.store.Get(ctx, sessionID); err != nil {
			return EffectivePromptResult{}, err
		}
		// A child's authority is fixed by its relation, whatever the host asks for.
		relation, relationErr := e.store.ParentRelation(ctx, sessionID)
		switch {
		case relationErr == nil:
			child, childKind = true, relation.Kind
			delegation = agent.DelegationNone
			if relation.Kind == RelationExecution {
				childPolicy, childAgent = toolset.PolicyWorkspace, config.AgentWorker
			} else {
				childPolicy, childAgent = toolset.PolicyReadOnly, config.AgentExplore
			}
			policy = childPolicy
		case !errors.Is(relationErr, sql.ErrNoRows):
			return EffectivePromptResult{}, relationErr
		}
	}
	defaultAgent := config.AgentDefault
	requested := strings.TrimSpace(req.Agent)
	if child {
		// A child's agent is fixed by its launch kind; the host's choice is ignored, as at its Start.
		defaultAgent, requested = cmp.Or(childAgent, config.AgentExplore), ""
	}
	agentID, _, grant, _, err := agent.AgentDecision(requested, session.AgentID, defaultAgent,
		agent.AgentGrant{Policy: policy, Delegation: delegation},
		func(id string) (agent.AgentCeilings, bool) {
			a, ok := eff.Agent(id)
			return agent.AgentCeilings{Policy: a.Policy, Delegation: a.Delegation, Digest: a.Digest}, ok
		})
	if err != nil {
		return EffectivePromptResult{}, err
	}
	def, _ := eff.Agent(agentID)
	policy, delegation = grant.Policy, grant.Delegation

	interactive := req.Interactive && !child
	base := agent.SystemPrompt(interactive, policyAuthority(policy))
	if child {
		// A child's launch fixes its prompt, not the host and not the default (ADR-0009 A4).
		if childKind == RelationExecution {
			base = workerPrompt + "\n" + policyAuthority(toolset.PolicyWorkspace)
		} else {
			base = explorePrompt
		}
	}
	prompt := assemblePrompt(promptInput{
		workspace: e.workspace, globalDir: eff.GlobalDir, trusted: eff.Trusted,
		base:              base,
		agentID:           agentID,
		agentSource:       def.Source,
		agentInstructions: def.Instructions,
		skills:            e.skillRegistry,
	})

	result := EffectivePromptResult{
		Workspace: e.workspace, Trusted: eff.Trusted, Agent: agentID, Policy: policy, Delegation: delegation,
		Layers: prompt.Layers, Warnings: promptWarnings(prompt.Layers),
	}

	workspace := e.workspace
	var wf *workflow.Runtime
	if sessionID != "" {
		// The session's own workspace, as its run would have it, and the Task State block its first
		// request carries.
		if workspace, err = e.agentWorkspace(ctx, sessionID); err != nil {
			return EffectivePromptResult{}, err
		}
		state, err := e.loadTaskState(ctx, sessionID)
		if err != nil {
			return EffectivePromptResult{}, err
		}
		if wf, err = state.RestoreWorkflow(); err != nil {
			return EffectivePromptResult{}, err
		}
		block := taskstate.Render(session.Goal, state, wf)
		result.Layers = append(result.Layers, PromptLayer{
			Kind: LayerTaskState, Source: "session:" + sessionID, Scope: scopeSession,
			Included: true, Bytes: len(block), Text: block,
		})
	}

	allowlist := protocol.CanonicalToolNames(req.ToolAllowlist)
	if wf != nil {
		allowlist = wf.EffectiveAllowlist(allowlist)
	}
	tools, err := e.promptTools(workspace, policy, child, sessionID, req.Subagents, eff.DelegationAgents(config.AgentInfo.SpawnEligible), newSkillReader(prompt.Skills.Included))
	if err != nil {
		return EffectivePromptResult{}, err
	}
	stepTools := agent.StepTools(tools, agent.StepToolsInput{
		Allowlist: allowlist, Denylist: protocol.CanonicalToolNames(req.ToolDenylist),
		Delegation: delegation, Tasks: req.Subagents && !child,
		TaskAgents: eff.DelegationAgents(config.AgentInfo.TaskEligible),
	})
	result.Tools = make([]PromptTool, 0, len(stepTools))
	for _, tool := range stepTools {
		result.Tools = append(result.Tools, PromptTool{
			Name: tool.Spec.Name, Permission: tool.Spec.Permission, Source: tool.Source,
			Included: tool.Included, Reason: tool.Reason,
		})
	}
	return result, nil
}

// promptTools builds the toolset a run of the session would have, without starting anything: no MCP
// server is launched and no provider client is built. The toolset only registers what is already
// running, which is exactly what a run would see.
func (e *Engine) promptTools(workspace, policy string, child bool, sessionID string, subagents bool, spawnAgents []config.AgentInfo, skillTools toolset.SkillReader) (*toolset.Set, error) {
	runner := e.shell
	if workspace != e.workspace {
		runner = shell.New(workspace)
	}
	var lspManager *lsp.Manager
	e.lspMu.Lock()
	if candidate := e.lspManagers[workspace]; candidate != nil && candidate.Status().Running {
		lspManager = candidate
	}
	e.lspMu.Unlock()
	registry := e.mcpRegistry
	var controller toolset.SubagentController
	var threads toolset.ThreadReader
	// The working notebook is bound per run to the run's session (ADR-0020). A report without a
	// session describes a run that will have one, so it lists the notebook tools too; nothing here
	// executes them.
	notebooks := toolset.NotebookStore(newNotebookAccess(e, sessionID, "report"))
	if child {
		// A child runs without MCP, without spawn tools and without threads of its own.
		registry = nil
	} else {
		if subagents {
			controller = reportSubagents{}
		}
		if sessionID != "" {
			threads = threadReader{engine: e, sessionID: sessionID, workspace: workspace}
		}
	}
	return toolset.New(toolset.Options{
		Workspace:      workspace,
		Policy:         policy,
		Shell:          runner,
		LSP:            lspManager,
		MCP:            registry,
		Subagents:      controller,
		SubagentAgents: spawnAgents,
		Threads:        threads,
		Skills:         skillTools,
		Notebook:       notebooks,
	})
}

// reportSubagents stands in for a run's subagent controller in EffectivePrompt: the toolset only
// needs one to register spawn_subagent, and nothing here can run, because EffectivePrompt never
// starts a run.
type reportSubagents struct{}

func (reportSubagents) Spawn(context.Context, string, string) (any, error) {
	return nil, errors.New("effective prompt does not spawn subagents")
}

func (reportSubagents) List(context.Context) (any, error) {
	return nil, errors.New("effective prompt does not spawn subagents")
}

func (reportSubagents) Wait(context.Context, string, time.Duration) (any, error) {
	return nil, errors.New("effective prompt does not spawn subagents")
}
