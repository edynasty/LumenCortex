package config

import (
	"fmt"
	"maps"
	"slices"
	"time"
)

// MergeProviderSettings applies a workspace provider's settings over the global ones (nil when the
// provider exists only in the workspace). Only a trusted workspace may do so: an untrusted one gets
// ErrUntrustedOverride, and the loader drops the entry with a warning.
//
// Fields merge one by one; a headers set, even an empty one, replaces the global set. Changing baseURL or
// endpoint, which would send the global credential elsewhere, needs the workspace's own apiKey, a
// reference different from the global one, or ErrCredentialRedirect results. A redirected provider
// never inherits the global headers, which may carry credentials too.
func MergeProviderSettings(global *ProviderSettings, ws ProviderSettings, trusted bool) (ProviderSettings, error) {
	if !trusted {
		return ProviderSettings{}, ErrUntrustedOverride
	}
	if global == nil {
		return cloneSettings(ws), nil
	}
	redirect := (ws.BaseURL != "" && ws.BaseURL != global.BaseURL) ||
		(ws.Endpoint != "" && ws.Endpoint != global.Endpoint)
	if redirect && (ws.APIKey == "" || ws.APIKey == global.APIKey) {
		return ProviderSettings{}, ErrCredentialRedirect
	}
	out := cloneSettings(*global)
	if redirect {
		out.Headers = nil
	}
	if ws.BaseURL != "" {
		out.BaseURL = ws.BaseURL
	}
	if ws.Endpoint != "" {
		out.Endpoint = ws.Endpoint
	}
	if ws.APIKey != "" {
		out.APIKey = ws.APIKey
	}
	if ws.Headers != nil {
		out.Headers = maps.Clone(ws.Headers)
	}
	if ws.TimeoutSeconds != 0 {
		out.TimeoutSeconds = ws.TimeoutSeconds
	}
	if ws.IdleTimeoutSeconds != 0 {
		out.IdleTimeoutSeconds = ws.IdleTimeoutSeconds
	}
	return out, nil
}

func (c *Config) merge() error {
	g, w := c.global.File, c.workspace.File
	trusted := c.eff.Trusted

	providers := cloneProviders(g.Providers)
	if providers == nil {
		providers = map[string]Provider{}
	}
	checks := cloneChecks(g.Checks)
	if checks == nil {
		checks = map[string]Check{}
	}
	for id, check := range checks {
		check.Source = CheckSourceGlobal
		checks[id] = check
	}
	model, subagent := g.Model, g.SubagentModel
	failover := slices.Clone(g.Failover)
	effort := cloneEffortPolicy(g.Effort)

	drop := func(key, what string) {
		c.eff.Warnings = append(c.eff.Warnings, c.untrustedWarning(key, what))
	}

	if len(w.TrustedWorkspaces) > 0 {
		c.eff.Warnings = append(c.eff.Warnings, Warning{
			Code: WarnUntrustedOverride, Path: c.workspace.Path, Key: "trustedWorkspaces", Err: ErrUntrustedOverride,
			Message: fmt.Sprintf("ignored trustedWorkspaces in %s: trust is read from the global file %s only",
				c.workspace.Path, c.global.Path),
		})
	}

	for _, id := range sortedKeys(w.Providers) {
		if !trusted {
			drop("providers."+id, "provider "+id)
			continue
		}
		wp := w.Providers[id]
		gp, ok := providers[id]
		var base *Provider
		if ok {
			base = &gp
		}
		merged, err := mergeProvider(base, wp)
		if err != nil {
			return fmt.Errorf("%w: workspace provider %q in %s: %w", ErrInvalidConfig, id, c.workspace.Path, err)
		}
		providers[id] = merged
	}
	for _, id := range sortedKeys(w.Checks) {
		if !trusted {
			drop("checks."+id, "check "+id)
			continue
		}
		check := cloneCheck(w.Checks[id])
		check.Source = CheckSourceWorkspace
		checks[id] = check
	}

	// An untrusted workspace may only choose among the models of global providers. Providers is
	// still the global catalog here, because the loop above dropped every workspace provider.
	if w.Model != "" {
		if trusted || hasModel(providers, w.Model) {
			model = w.Model
		} else {
			drop("model", "model "+string(w.Model))
		}
	}
	if w.SubagentModel != "" {
		if trusted || hasModel(providers, w.SubagentModel) {
			subagent = w.SubagentModel
		} else {
			drop("subagentModel", "subagentModel "+string(w.SubagentModel))
		}
	}
	if w.Failover != nil {
		if trusted || allModels(providers, w.Failover) {
			failover = slices.Clone(w.Failover)
		} else {
			drop("failover", "failover list")
		}
	}
	if w.Effort != nil {
		effort = cloneEffortPolicy(w.Effort)
		// An untrusted workspace may choose only among the models of global providers, like
		// subagentModel and failover. An auto judge it names elsewhere is ignored (the heuristic
		// classifier still applies).
		if effort.Auto != nil && effort.Auto.Model != "" && !trusted && !hasModel(providers, effort.Auto.Model) {
			drop("effort.auto.model", "effort.auto.model "+string(effort.Auto.Model))
			effort.Auto.Model = ""
		}
	}

	refs := append([]ModelRef{model, subagent}, failover...)
	if effort != nil && effort.Auto != nil && effort.Auto.Model != "" {
		refs = append(refs, effort.Auto.Model)
	}
	for _, ref := range refs {
		if ref != "" && !hasModel(providers, ref) {
			return fmt.Errorf("%w: model reference %q: %w", ErrInvalidConfig, ref, ErrUnknownModel)
		}
	}

	c.providers = providers
	c.eff.Model, c.eff.SubagentModel, c.eff.Failover = model, subagent, failover
	if effort != nil {
		c.eff.EffortDefault = effort.Default
		c.eff.Escalation = effort.Escalation
		c.eff.Auto = resolvedAutoPolicy(effort.Auto)
	}
	c.eff.Checks = checks
	return nil
}

func (c *Config) untrustedWarning(key, what string) Warning {
	return Warning{
		Code: WarnUntrustedOverride, Path: c.workspace.Path, Key: key,
		TrustEntry: c.eff.Workspace, TrustFile: c.global.Path, Err: ErrUntrustedOverride,
		Message: fmt.Sprintf("ignored %s in %s: workspace %s is not trusted, so the global configuration applies. "+
			"To use it, add %q to trustedWorkspaces in %s",
			what, c.workspace.Path, c.eff.Workspace, c.eff.Workspace, c.global.Path),
	}
}

func hasModel(providers map[string]Provider, ref ModelRef) bool {
	pid, key, ok := ref.Split()
	if !ok {
		return false
	}
	_, found := providers[pid].Models[key]
	return found
}

func allModels(providers map[string]Provider, refs []ModelRef) bool {
	for _, r := range refs {
		if !hasModel(providers, r) {
			return false
		}
	}
	return true
}

// mergeProvider applies a trusted workspace provider entry over the global one with the same id
// (nil when the id is workspace-only), field by field.
func mergeProvider(global *Provider, ws Provider) (Provider, error) {
	out := Provider{}
	var gs *ProviderSettings
	if global != nil {
		out = cloneProvider(*global)
		gs = global.Settings
	}
	if ws.Name != "" {
		out.Name = ws.Name
	}
	if ws.Package != "" {
		out.Package = ws.Package
	}
	if gs != nil || ws.Settings != nil {
		var wsSettings ProviderSettings
		if ws.Settings != nil {
			wsSettings = *ws.Settings
		}
		merged, err := MergeProviderSettings(gs, wsSettings, true)
		if err != nil {
			return Provider{}, err
		}
		out.Settings = &merged
	}
	for _, key := range sortedKeys(ws.Models) {
		if out.Models == nil {
			out.Models = map[string]Model{}
		}
		out.Models[key] = mergeModel(out.Models[key], ws.Models[key])
	}
	return out, nil
}

func mergeModel(base, ws Model) Model {
	out := cloneModel(base)
	if ws.Name != "" {
		out.Name = ws.Name
	}
	if ws.ModelID != "" {
		out.ModelID = ws.ModelID
	}
	if ws.Limit != nil {
		l := Limit{}
		if out.Limit != nil {
			l = *out.Limit
		}
		if ws.Limit.Context != 0 {
			l.Context = ws.Limit.Context
		}
		if ws.Limit.Output != 0 {
			l.Output = ws.Limit.Output
		}
		out.Limit = &l
	}
	if ws.Reasoning != nil {
		r := Reasoning{}
		if out.Reasoning != nil {
			r = *out.Reasoning
		}
		if ws.Reasoning.Format != "" {
			r.Format = ws.Reasoning.Format
		}
		if len(ws.Reasoning.Efforts) > 0 {
			r.Efforts = slices.Clone(ws.Reasoning.Efforts)
		}
		if ws.Reasoning.Echo != "" {
			r.Echo = ws.Reasoning.Echo
		}
		out.Reasoning = &r
	}
	if ws.ToolCalling != nil {
		v := *ws.ToolCalling
		out.ToolCalling = &v
	}
	if ws.Modalities != nil {
		out.Modalities = &Modalities{Input: slices.Clone(ws.Modalities.Input)}
	}
	return out
}

func cloneDocument(d Document) Document {
	d.File = cloneFile(d.File)
	return d
}

func cloneFile(f File) File {
	out := f
	out.Failover = slices.Clone(f.Failover)
	out.Effort = cloneEffortPolicy(f.Effort)
	out.Providers = cloneProviders(f.Providers)
	out.Checks = cloneChecks(f.Checks)
	out.TrustedWorkspaces = slices.Clone(f.TrustedWorkspaces)
	return out
}

// resolvedAutoPolicy applies the documented defaults to effort.auto: a high ceiling, an 8 second
// judge timeout, clamped to the schema's 1..60 range. A ceiling that is not a concrete level falls
// back to high; the schema rejects it before it gets here.
func resolvedAutoPolicy(a *AutoEffort) *AutoEffortPolicy {
	if a == nil {
		return nil
	}
	ceiling := a.Max
	if !ceiling.IsLevel() {
		ceiling = DefaultAutoMax
	}
	seconds := a.TimeoutSeconds
	if seconds <= 0 {
		seconds = DefaultAutoTimeout
	}
	seconds = min(max(seconds, 1), 60)
	return &AutoEffortPolicy{Model: a.Model, Max: ceiling, Timeout: time.Duration(seconds) * time.Second}
}

func cloneEffortPolicy(p *EffortPolicy) *EffortPolicy {
	if p == nil {
		return nil
	}
	out := *p
	if p.Escalation != nil {
		e := *p.Escalation
		out.Escalation = &e
	}
	if p.Auto != nil {
		a := *p.Auto
		out.Auto = &a
	}
	return &out
}

func cloneProviders(in map[string]Provider) map[string]Provider {
	if in == nil {
		return nil
	}
	out := make(map[string]Provider, len(in))
	for id, p := range in {
		out[id] = cloneProvider(p)
	}
	return out
}

func cloneProvider(p Provider) Provider {
	out := p
	if p.Settings != nil {
		s := cloneSettings(*p.Settings)
		out.Settings = &s
	}
	if p.Models != nil {
		out.Models = make(map[string]Model, len(p.Models))
		for k, m := range p.Models {
			out.Models[k] = cloneModel(m)
		}
	}
	return out
}

func cloneSettings(s ProviderSettings) ProviderSettings {
	s.Headers = maps.Clone(s.Headers)
	return s
}

func cloneModel(m Model) Model {
	out := m
	if m.Limit != nil {
		l := *m.Limit
		out.Limit = &l
	}
	if m.Reasoning != nil {
		r := *m.Reasoning
		r.Efforts = slices.Clone(m.Reasoning.Efforts)
		out.Reasoning = &r
	}
	if m.ToolCalling != nil {
		v := *m.ToolCalling
		out.ToolCalling = &v
	}
	if m.Modalities != nil {
		out.Modalities = &Modalities{Input: slices.Clone(m.Modalities.Input)}
	}
	return out
}

func cloneChecks(in map[string]Check) map[string]Check {
	if in == nil {
		return nil
	}
	out := make(map[string]Check, len(in))
	for id, c := range in {
		out[id] = cloneCheck(c)
	}
	return out
}

func cloneCheck(c Check) Check {
	c.Files = slices.Clone(c.Files)
	if c.Required != nil {
		v := *c.Required
		c.Required = &v
	}
	return c
}
