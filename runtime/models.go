package runtime

import (
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/evaluator"
	"github.com/edynasty/LumenCortex/protocol"
	"github.com/edynasty/LumenCortex/provider"
)

// ProviderFactory builds the provider client of one resolved catalog model. The Engine calls it
// before a run starts, once per model the run may use. Options.ProviderFactory replaces the
// default, which is provider.FromModel: it selects the adapter by the provider's package
// (openai-compatible, anthropic-messages).
type ProviderFactory func(config.ResolvedModel) (protocol.Provider, error)

var (
	// ErrUnknownModel reports a model reference that is empty or not in the provider catalog. The
	// host offers its model picker; nothing was written.
	ErrUnknownModel = config.ErrUnknownModel
	// ErrModelWithoutToolCalling reports a session model whose configuration sets toolCalling: false.
	ErrModelWithoutToolCalling = errors.New("model does not support tool calling")
	// ErrInvalidEffort reports an effort outside none, low, medium, high, max, auto.
	ErrInvalidEffort = errors.New("effort must be one of none, low, medium, high, max, auto")
	// ErrUnknownAgent reports an agent id that is not in the catalog: requested by the host, or
	// pinned by a session whose agent was since removed. The host offers its agent picker; nothing
	// was written. A removed agent never falls back to another one, which could widen authority.
	ErrUnknownAgent = agent.ErrAgentNotInCatalog
	// ErrAgentNotForConversation reports a requested agent whose use is delegation only.
	ErrAgentNotForConversation = errors.New("agent runs only as a delegated child")
)

func defaultProviderFactory(m config.ResolvedModel) (protocol.Provider, error) {
	return provider.FromModel(m, provider.Options{})
}

// startRequest is what planModels reads from the Start besides configuration.
type startRequest struct {
	model, storedModel config.ModelRef
	effort             config.Effort
	// agent is the host's choice, storedAgent the session's pin and defaultAgent the launch kind's
	// default (agent for a thread, explore or worker for a child).
	agent, storedAgent, defaultAgent string
	// child is a delegated run: the host's agent is ignored and use is not checked. parentAgent is
	// the parent session's agent, whose list a child agent without its own inherits.
	child       bool
	parentAgent string
	grant       agent.AgentGrant
}

// modelPlan is everything a Start decides from configuration before it writes anything: the agent,
// models, and the checks the run decides (one load, so the gate and the evidence rule see the same
// checks).
type modelPlan struct {
	agentID       string
	agentRule     string
	agentDecision agent.Decision
	// grant is the host's grant after the agent's ceilings.
	grant        agent.AgentGrant
	instructions string
	session      config.ModelRef
	resolved     config.ResolvedModel
	rule         string
	decision     agent.Decision
	// failover is the agent's own list, else the configured failover; it follows the session model.
	failover []config.ModelRef
	models   map[config.ModelRef]agent.Model
	// effort is the default of every candidate effortFor does not name: the agent's effort, else
	// effort.default. effortFor holds the list entries with their own effort.
	effort    config.Effort
	effortFor map[config.ModelRef]config.Effort
	// escalation is the configured effort escalation, nil for none (design §4.5).
	escalation *config.Escalation
	// auto is the auto effort resolver's configuration with defaults applied. It is always set;
	// the judge client is built only when a Start actually resolves auto.
	auto *autoPlan
	// checks are the effective configuration's checks (design §4.7); untrusted workspace checks are
	// already dropped by config.
	checks []evaluator.Spec
}

// autoPlan is the resolved effort.auto settings of one Start. judge is zero when no judge model is
// configured or it is not in the catalog; the heuristic then decides.
type autoPlan struct {
	max     config.Effort
	timeout time.Duration
	judge   config.ResolvedModel
}

// planModels resolves the session's agent, its model and candidate list, and builds the required
// clients (design §4.4, D7, ADR-0009). Every failure leaves the database untouched. The configuration
// is read from the Engine workspace, never from a session worktree.
//
// A list reference outside the catalog fails the Start, and so does one whose API key variable is
// unset: a configuration that would only break during the first outage is refused up front. A list
// model without tool calling is kept for the decision log but gets no client.
func (e *Engine) planModels(req startRequest) (*modelPlan, error) {
	if !agent.ValidEffortRequest(req.effort) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidEffort, req.effort)
	}
	cfg, err := config.Load(e.workspace)
	if err != nil {
		return nil, err
	}
	eff := cfg.Effective()
	if !agent.ValidEffortRequest(eff.EffortDefault) {
		return nil, fmt.Errorf("%w: configured default %q", ErrInvalidEffort, eff.EffortDefault)
	}
	requested := req.agent
	if req.child {
		requested = ""
	}
	agentID, agentRule, grant, agentDecision, err := agent.AgentDecision(requested, req.storedAgent, req.defaultAgent, req.grant,
		func(id string) (agent.AgentCeilings, bool) {
			a, ok := eff.Agent(id)
			return agent.AgentCeilings{Policy: a.Policy, Delegation: a.Delegation, Digest: a.Digest}, ok
		})
	if err != nil {
		return nil, err
	}
	def, _ := eff.Agent(agentID)
	if agentRule == agent.RuleAgentUser && !def.ForConversation() {
		return nil, fmt.Errorf("%w: %s", ErrAgentNotForConversation, agentID)
	}
	// An agent without its own list inherits: a child its parent agent's list, else (and for a
	// thread) the configured failover after the pinned model, exactly as before agents had lists.
	// The configured model only pins a new session.
	list := def.Models
	var agentFirst config.ModelRef
	if len(list) > 0 {
		agentFirst = list[0].Model
	} else if parent, ok := eff.Agent(req.parentAgent); req.child && ok && len(parent.Models) > 0 {
		list = parent.Models
	} else {
		for _, ref := range eff.Failover {
			list = append(list, config.AgentModel{Model: ref})
		}
	}
	ref, rule, decision := agent.ModelDecision(req.model, req.storedModel, agentFirst, eff.Model,
		agent.AgentSwitched(req.storedAgent, agentID),
		config.Digest([]byte(cfg.Global().Digest+"\n"+cfg.Workspace().Digest)))
	if ref == "" {
		return nil, fmt.Errorf("%w: no model was requested, pinned or configured", ErrUnknownModel)
	}
	resolved, err := cfg.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if !resolved.ToolCalling {
		return nil, fmt.Errorf("%w: %s", ErrModelWithoutToolCalling, ref)
	}
	auto := &autoPlan{max: config.DefaultAutoMax, timeout: time.Duration(config.DefaultAutoTimeout) * time.Second}
	if eff.Auto != nil {
		auto.max, auto.timeout = eff.Auto.Max, eff.Auto.Timeout
		if eff.Auto.Model != "" {
			judge, err := cfg.Resolve(eff.Auto.Model)
			if err != nil {
				return nil, fmt.Errorf("effort.auto model %s: %w", eff.Auto.Model, err)
			}
			auto.judge = judge
		}
	}
	plan := &modelPlan{
		agentID: agentID, agentRule: agentRule, agentDecision: agentDecision, grant: grant, instructions: def.Instructions,
		session: ref, resolved: resolved, rule: rule, decision: decision,
		models: map[config.ModelRef]agent.Model{}, effort: eff.EffortDefault, effortFor: map[config.ModelRef]config.Effort{},
		escalation: eff.Escalation, auto: auto, checks: evaluator.Specs(eff.Checks),
	}
	if def.Effort != "" {
		plan.effort = def.Effort
	}
	for _, m := range list {
		plan.failover = append(plan.failover, m.Model)
		if m.Effort != "" && m.Effort != plan.effort {
			plan.effortFor[m.Model] = m.Effort
		}
	}
	for _, candidate := range append([]config.ModelRef{ref}, plan.failover...) {
		if _, done := plan.models[candidate]; done {
			continue
		}
		m := resolved
		if candidate != ref {
			if m, err = cfg.Resolve(candidate); err != nil {
				return nil, fmt.Errorf("agent %s model %s: %w", agentID, candidate, err)
			}
		}
		built := agent.Model{
			Ref: candidate, ProviderID: m.ProviderID, ModelID: m.ModelID,
			Context: m.Context, DefaultContext: !m.ContextSet, Output: m.Output, OutputSet: m.OutputSet,
			ToolCalling: m.ToolCalling, Efforts: m.Efforts,
		}
		if m.ToolCalling {
			if built.Client, err = e.providerFactory(m); err != nil {
				return nil, fmt.Errorf("model %s: %w", candidate, err)
			}
		}
		plan.models[candidate] = built
	}
	return plan, nil
}

// needsAuto reports whether any candidate of the run would request "auto": the override, or with no
// override the agent's default or a list entry.
func (p *modelPlan) needsAuto(override config.Effort) bool {
	if override != "" {
		return override == config.EffortAuto
	}
	if p.effort == config.EffortAuto {
		return true
	}
	for _, e := range p.effortFor {
		if e == config.EffortAuto {
			return true
		}
	}
	return false
}
