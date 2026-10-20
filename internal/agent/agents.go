package agent

import (
	"fmt"
	"slices"

	"github.com/edynasty/LumenCortex/config"
)

// This file is the agent rule of a Start (ADR-0009): which catalog agent the session runs, and the
// authority the run gets once that agent's ceilings are applied. Like the model rule it is pure and
// logged as one decision that replays from its recorded inputs.

// DecisionAgent is the decision kind of the Start's agent resolution.
const DecisionAgent = "agent"

// Agent rule ids.
const (
	RuleAgentUser    = "agent.user"    // AgentOptions.Agent differs from sessions.agent_id; written to it
	RuleAgentSession = "agent.session" // sessions.agent_id
	RuleAgentDefault = "agent.default" // the launch kind's default agent; written to sessions.agent_id
)

// ResolveAgent picks the agent a Start runs and names the rule:
//
//	requested differs from stored  → agent.user    (re-pins the session)
//	otherwise stored is set        → agent.session
//	otherwise                      → agent.default (def: agent for a thread, explore or worker for a child)
func ResolveAgent(requested, stored, def string) (string, string) {
	switch {
	case requested != "" && requested != stored:
		return requested, RuleAgentUser
	case stored != "":
		return stored, RuleAgentSession
	default:
		return def, RuleAgentDefault
	}
}

// WritesAgent reports whether an agent rule persists its result to sessions.agent_id.
func WritesAgent(rule string) bool { return rule == RuleAgentUser || rule == RuleAgentDefault }

// AgentSwitched reports whether the agent rule moved a session from one agent to another. A session
// that had no agent (created before migration 0005) is not switched: its model pin stays.
func AgentSwitched(stored, resolved string) bool { return stored != "" && resolved != stored }

var (
	policyOrder     = []string{config.PolicyReadOnly, config.PolicyWorkspace, config.PolicyFull}
	delegationOrder = []string{DelegationNone, DelegationPropose, DelegationIsolated}
)

// CapPolicy is the lower of a granted tool policy and an agent's ceiling. An empty ceiling caps
// nothing. Unknown values are left to toolset.New, which refuses them.
func CapPolicy(grant, ceiling string) string {
	g, c := slices.Index(policyOrder, grant), slices.Index(policyOrder, ceiling)
	if g < 0 || c < 0 || g <= c {
		return grant
	}
	return ceiling
}

// CapDelegation is the lower of a granted delegation and an agent's ceiling ("none" and "" are the
// same grant). An empty ceiling caps nothing.
func CapDelegation(grant, ceiling string) string {
	if ceiling == "none" {
		ceiling = DelegationNone
	} else if ceiling == "" {
		return grant
	}
	g, c := slices.Index(delegationOrder, grant), slices.Index(delegationOrder, ceiling)
	if g < 0 || c < 0 || g <= c {
		return grant
	}
	return ceiling
}

type agentInputs struct {
	Requested string `json:"requested"`
	Stored    string `json:"stored"`
	Default   string `json:"default"`
	// Digest identifies the resolved agent's definition (config.AgentInfo.Digest).
	Digest string `json:"digest"`
	// Policy and Delegation are what the host granted; the ceilings are the resolved agent's.
	Policy            string `json:"policy"`
	Delegation        string `json:"delegation"`
	PolicyCeiling     string `json:"policyCeiling"`
	DelegationCeiling string `json:"delegationCeiling"`
}

type agentOutput struct {
	Agent      string `json:"agent"`
	Write      bool   `json:"write"`
	Policy     string `json:"policy"`
	Delegation string `json:"delegation"`
}

// AgentCeilings are the resolved agent's authority ceilings and definition digest.
type AgentCeilings struct {
	Policy, Delegation, Digest string
}

// AgentGrant is what the host granted a Start.
type AgentGrant struct {
	Policy, Delegation string
}

// AgentDecision records a Start's agent resolution and the authority its ceilings leave. lookup
// returns the ceilings of the resolved id, false when the catalog has no such agent; the caller then
// refuses the Start (ErrUnknownAgent) and nothing is logged.
func AgentDecision(requested, stored, def string, grant AgentGrant, lookup func(string) (AgentCeilings, bool)) (string, string, AgentGrant, Decision, error) {
	id, rule := ResolveAgent(requested, stored, def)
	ceil, ok := lookup(id)
	if !ok {
		return "", "", AgentGrant{}, Decision{}, fmt.Errorf("agent %q is not in the catalog", id)
	}
	out := AgentGrant{Policy: CapPolicy(grant.Policy, ceil.Policy), Delegation: CapDelegation(grant.Delegation, ceil.Delegation)}
	return id, rule, out, newDecision(DecisionAgent, rule,
		agentInputs{Requested: requested, Stored: stored, Default: def, Digest: ceil.Digest,
			Policy: grant.Policy, Delegation: grant.Delegation, PolicyCeiling: ceil.Policy, DelegationCeiling: ceil.Delegation},
		agentOutput{Agent: id, Write: WritesAgent(rule), Policy: out.Policy, Delegation: out.Delegation}), nil
}

func replayAgent(d Decision) error {
	var in agentInputs
	var out agentOutput
	if err := decodeDecision(d, &in, &out); err != nil {
		return err
	}
	id, rule := ResolveAgent(in.Requested, in.Stored, in.Default)
	policy, delegation := CapPolicy(in.Policy, in.PolicyCeiling), CapDelegation(in.Delegation, in.DelegationCeiling)
	if rule != d.Rule || id != out.Agent || WritesAgent(rule) != out.Write || policy != out.Policy || delegation != out.Delegation {
		return fmt.Errorf("agent decision: recomputed %s via %s (write=%v, %s/%q), logged %s via %s (write=%v, %s/%q)",
			id, rule, WritesAgent(rule), policy, delegation, out.Agent, d.Rule, out.Write, out.Policy, out.Delegation)
	}
	return nil
}
