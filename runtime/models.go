package runtime

import (
	"errors"
	"fmt"

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
	// ErrInvalidEffort reports an effort outside none, low, medium, high, max.
	ErrInvalidEffort = errors.New("effort must be one of none, low, medium, high, max")
)

func defaultProviderFactory(m config.ResolvedModel) (protocol.Provider, error) {
	return provider.FromModel(m, provider.Options{})
}

// modelPlan is everything a Start decides from configuration before it writes anything: models, and
// the checks the run decides (one load, so the gate and the evidence rule see the same checks).
type modelPlan struct {
	session  config.ModelRef
	resolved config.ResolvedModel
	rule     string
	decision agent.Decision
	failover []config.ModelRef
	models   map[config.ModelRef]agent.Model
	effort   config.Effort
	// escalation is the configured effort escalation, nil for none (design §4.5).
	escalation *config.Escalation
	// checks are the effective configuration's checks (design §4.7); untrusted workspace checks are
	// already dropped by config.
	checks []evaluator.Spec
}

// planModels resolves the session model and the failover list and builds the required clients
// (design §4.4, D7). Every failure leaves the database untouched. The configuration is read from
// the Engine workspace, never from a session worktree.
//
// A failover reference outside the catalog fails the Start, and so does one whose API key variable is
// unset: a configuration that would only break during the first outage is refused up front. A
// failover model without tool calling is kept for the decision log but gets no client.
func (e *Engine) planModels(requested, stored config.ModelRef, effort config.Effort) (*modelPlan, error) {
	if !agent.ValidEffort(effort) {
		return nil, fmt.Errorf("%w: %q", ErrInvalidEffort, effort)
	}
	cfg, err := config.Load(e.workspace)
	if err != nil {
		return nil, err
	}
	eff := cfg.Effective()
	ref, rule, decision := agent.ModelDecision(requested, stored, eff.Model,
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
	plan := &modelPlan{
		session: ref, resolved: resolved, rule: rule, decision: decision,
		failover: eff.Failover, models: map[config.ModelRef]agent.Model{}, effort: eff.EffortDefault,
		escalation: eff.Escalation, checks: evaluator.Specs(eff.Checks),
	}
	for _, candidate := range append([]config.ModelRef{ref}, eff.Failover...) {
		if _, done := plan.models[candidate]; done {
			continue
		}
		m := resolved
		if candidate != ref {
			if m, err = cfg.Resolve(candidate); err != nil {
				return nil, fmt.Errorf("failover model %s: %w", candidate, err)
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
