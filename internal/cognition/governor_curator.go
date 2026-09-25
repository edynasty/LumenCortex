package cognition

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/edynasty/LumenCortex/protocol"
)

type LLMGovernorCurator struct {
	Provider protocol.Provider
	MaxTokens int
	ReasoningEffort string
}

func (c LLMGovernorCurator) Propose(ctx context.Context, graph GraphState, analysis GovernorAnalysis) (GovernorPlan, error) {
	if c.Provider == nil { return GovernorPlan{}, errors.New("graph governor curator provider is required") }
	maxTokens := c.MaxTokens
	if maxTokens < 512 { maxTokens = 6000 }
	effort := strings.TrimSpace(c.ReasoningEffort)
	if effort == "" { effort = "high" }
	payload := governorCandidateContext(graph, analysis, 64, 256)
	rawPayload, err := json.Marshal(payload)
	if err != nil { return GovernorPlan{}, err }
	response, err := c.Provider.Complete(ctx, protocol.ProviderRequest{
		Messages: []protocol.Message{
			{Role: "system", Content: governorSystemPrompt()},
			{Role: "user", Content: string(rawPayload)},
		},
		MaxTokens: maxTokens,
		ReasoningEffort: effort,
	})
	if err != nil { return GovernorPlan{}, err }
	plan, err := parseGovernorPlan(response.Message.Content)
	if err != nil { return GovernorPlan{}, err }
	if len(plan.Tiers.Hot) == 0 && len(plan.Tiers.Warm) == 0 && len(plan.Tiers.Cold) == 0 {
		plan.Tiers = cloneGovernorTiers(analysis.Tiers)
	}
	if plan.Epoch == nil && analysis.Epoch.Recommended {
		plan.Epoch = &EpochPlan{Proposed: true, Reasons: append([]string(nil), analysis.Epoch.Reasons...)}
	}
	plan.Summary = governorTrim(plan.Summary, 4000)
	return plan, nil
}

func parseGovernorPlan(content string) (GovernorPlan, error) {
	text := strings.TrimSpace(content)
	if text == "" { return GovernorPlan{}, errors.New("graph governor curator returned empty output") }
	if strings.HasPrefix(strings.ToLower(text), "```json") { text = strings.TrimSpace(text[len("```json"):]) } else if strings.HasPrefix(text, "```") { text = strings.TrimSpace(text[3:]) }
	if strings.HasSuffix(text, "```") { text = strings.TrimSpace(text[:len(text)-3]) }
	var plan GovernorPlan
	if err := json.Unmarshal([]byte(text), &plan); err == nil { return plan, nil }
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end > start {
		if err := json.Unmarshal([]byte(text[start:end+1]), &plan); err == nil { return plan, nil }
	}
	return GovernorPlan{}, errors.New("graph governor curator returned invalid JSON")
}

func governorCandidateContext(graph GraphState, analysis GovernorAnalysis, maxGroups, maxNodes int) map[string]any {
	if maxGroups < 1 { maxGroups = 64 }
	if maxNodes < 10 { maxNodes = 256 }
	candidateIDs := []string{}
	add := func(id string) {
		if id == "" || len(candidateIDs) >= maxNodes { return }
		for _, existing := range candidateIDs { if existing == id { return } }
		candidateIDs = append(candidateIDs, id)
	}
	for _, item := range analysis.Candidates.Archive { add(item.NodeID) }
	for i, group := range analysis.Candidates.Canonicalize {
		if i >= maxGroups { break }
		for _, id := range group.NodeIDs { add(id) }
	}
	for i, item := range analysis.Candidates.Branch {
		if i >= maxGroups { break }
		add(item.From); add(item.To)
	}
	for i, group := range analysis.Candidates.Promote {
		if i >= maxGroups { break }
		for _, id := range group.NodeIDs { add(id) }
	}
	for i, id := range analysis.Tiers.Hot { if i >= 32 { break }; add(id) }

	summaries := map[string]any{}
	for _, id := range candidateIDs {
		node, ok := graph.Nodes[id]
		if !ok { continue }
		tags := append([]string(nil), node.Tags...)
		if len(tags) > 12 { tags = tags[:12] }
		summaries[id] = map[string]any{
			"id": node.ID,
			"kind": node.Kind,
			"title": node.Title,
			"status": node.Status,
			"grade": node.Grade,
			"trustZone": node.TrustZone,
			"tags": tags,
			"body": governorTrim(node.Body, 800),
			"metadata": map[string]any{
				"path": governorMetadataString(node.Metadata, "path"),
				"sourceKind": governorMetadataString(node.Metadata, "sourceKind"),
				"storageTier": governorMetadataString(node.Metadata, "storageTier"),
			},
		}
	}

	archive := analysis.Candidates.Archive
	if len(archive) > maxGroups { archive = archive[:maxGroups] }
	canonicalize := analysis.Candidates.Canonicalize
	if len(canonicalize) > maxGroups { canonicalize = canonicalize[:maxGroups] }
	branch := analysis.Candidates.Branch
	if len(branch) > maxGroups { branch = branch[:maxGroups] }
	promote := analysis.Candidates.Promote
	if len(promote) > maxGroups { promote = promote[:maxGroups] }

	return map[string]any{
		"objective": "Propose long-horizon graph maintenance. Do not execute changes.",
		"metrics": analysis.Metrics,
		"currentTiers": analysis.Tiers,
		"candidates": map[string]any{
			"archive": archive, "canonicalize": canonicalize, "branch": branch, "promote": promote, "epoch": analysis.Epoch,
		},
		"nodes": summaries,
	}
}

func governorSystemPrompt() string {
	return strings.Join([]string{
		"You are the semantic Curator for LumenCortex Graph Governor.",
		"You do not mutate the graph. You only propose a GraphMutationPlan over the candidate IDs provided.",
		"Return one JSON object only. Do not use markdown fences.",
		"Preserve provenance. Prefer archive/tiering over deletion. Never invent node IDs.",
		"Canonicalization means choosing a canonical node and alias nodes without deleting provenance.",
		"Branch means preserving competing hypotheses, not choosing a winner.",
		"Promotion means proposing an abstraction over existing children.",
		"The deterministic Validator may reject any unsafe or invalid proposal.",
		"Schema: archive[], tiers{hot,warm,cold}, canonicalize[], branch[], promote[], epoch, summary.",
	}, "\n")
}
