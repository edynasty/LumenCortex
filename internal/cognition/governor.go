package cognition

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var governorGradeWeights = map[string]float64{
	"hypothesis": 0.45,
	"static": 0.65,
	"tested": 0.82,
	"runtime": 0.92,
	"reproduced": 1.0,
}

var governorTrustWeights = map[string]float64{
	"system_verified": 1.0,
	"repo_trusted": 0.95,
	"runtime_verified": 0.95,
	"user_provided": 0.85,
	"external_untrusted": 0.55,
	"model_inferred": 0.5,
}

var governorStatusWeights = map[string]float64{
	"active": 1,
	"dormant": 0.62,
	"stale": 0.34,
	"archived": 0.08,
	"invalid": 0,
}

type GraphNode struct {
	ID string `json:"id"`
	Kind string `json:"kind"`
	Title string `json:"title,omitempty"`
	Body string `json:"body,omitempty"`
	Status string `json:"status,omitempty"`
	Grade string `json:"grade,omitempty"`
	TrustZone string `json:"trustZone,omitempty"`
	Tags []string `json:"tags,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type GraphEdge struct {
	ID string `json:"id"`
	From string `json:"from"`
	To string `json:"to"`
	Type string `json:"type"`
	Weight float64 `json:"weight,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type GraphState struct {
	Version int `json:"version,omitempty"`
	Nodes map[string]GraphNode `json:"nodes"`
	Edges map[string]GraphEdge `json:"edges"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type StorageTelemetry struct {
	Tier string `json:"tier,omitempty"`
	LastAccessAt time.Time `json:"lastAccessAt,omitempty"`
	AccessCount int `json:"accessCount,omitempty"`
}

type GovernorAnalyzeOptions struct {
	HotThreshold float64
	WarmThreshold float64
	ArchiveThreshold float64
	PromotionMinGroup int
	EpochStaleRatio float64
	EpochDuplicateGroups int
	EpochNodeCount int
	StorageByNode map[string]StorageTelemetry
	Now time.Time
}

type GovernorMetrics struct {
	NodeCount int `json:"nodeCount"`
	EdgeCount int `json:"edgeCount"`
	StaleCount int `json:"staleCount"`
	ArchivedCount int `json:"archivedCount"`
	StaleRatio float64 `json:"staleRatio"`
	ContradictionCount int `json:"contradictionCount"`
	DuplicateGroupCount int `json:"duplicateGroupCount"`
	PromotionGroupCount int `json:"promotionGroupCount"`
	TierCounts map[string]int `json:"tierCounts"`
}

type GovernorTiers struct {
	Hot []string `json:"hot"`
	Warm []string `json:"warm"`
	Cold []string `json:"cold"`
}

type ArchiveCandidate struct {
	NodeID string `json:"nodeId"`
	Value float64 `json:"value"`
	Status string `json:"status"`
	Degree int `json:"degree"`
	Reason string `json:"reason"`
}

type CanonicalizeCandidate struct {
	Kind string `json:"kind"`
	NormalizedTitle string `json:"normalizedTitle"`
	NodeIDs []string `json:"nodeIds"`
	Titles []string `json:"titles"`
}

type BranchCandidate struct {
	EdgeID string `json:"edgeId"`
	From string `json:"from"`
	To string `json:"to"`
	Relation string `json:"relation"`
	Weight float64 `json:"weight"`
}

type PromotionCandidate struct {
	Tag string `json:"tag"`
	NodeIDs []string `json:"nodeIds"`
}

type GovernorCandidates struct {
	Archive []ArchiveCandidate `json:"archive"`
	Canonicalize []CanonicalizeCandidate `json:"canonicalize"`
	Branch []BranchCandidate `json:"branch"`
	Promote []PromotionCandidate `json:"promote"`
}

type EpochRecommendation struct {
	Recommended bool `json:"recommended"`
	Reasons []string `json:"reasons"`
}

type GovernorAnalysis struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Metrics GovernorMetrics `json:"metrics"`
	Values map[string]float64 `json:"values"`
	Tiers GovernorTiers `json:"tiers"`
	Candidates GovernorCandidates `json:"candidates"`
	Epoch EpochRecommendation `json:"epoch"`
}

type GovernorAnalyzer struct{}

func (GovernorAnalyzer) Analyze(graph GraphState, options GovernorAnalyzeOptions) GovernorAnalysis {
	hot := options.HotThreshold
	if hot == 0 { hot = 0.68 }
	warm := options.WarmThreshold
	if warm == 0 { warm = 0.4 }
	archiveThreshold := options.ArchiveThreshold
	if archiveThreshold == 0 { archiveThreshold = 0.24 }
	promotionMin := options.PromotionMinGroup
	if promotionMin < 3 { promotionMin = 3 }
	staleRatioThreshold := options.EpochStaleRatio
	if staleRatioThreshold == 0 { staleRatioThreshold = 0.3 }
	duplicateThreshold := options.EpochDuplicateGroups
	if duplicateThreshold == 0 { duplicateThreshold = 5 }
	nodeThreshold := options.EpochNodeCount
	if nodeThreshold == 0 { nodeThreshold = 50000 }
	now := options.Now
	if now.IsZero() { now = time.Now().UTC() }

	nodes := sortedGovernorNodes(graph.Nodes)
	edges := sortedGovernorEdges(graph.Edges)
	degrees := governorDegreeMap(nodes, edges)
	maxDegree := 1
	for _, value := range degrees {
		if value > maxDegree { maxDegree = value }
	}

	values := map[string]float64{}
	tiers := GovernorTiers{}
	for _, node := range nodes {
		values[node.ID] = governorNodeValue(node, degrees[node.ID], maxDegree, options.StorageByNode[node.ID], now)
		value := values[node.ID]
		switch {
		case value >= hot:
			tiers.Hot = append(tiers.Hot, node.ID)
		case value >= warm:
			tiers.Warm = append(tiers.Warm, node.ID)
		default:
			tiers.Cold = append(tiers.Cold, node.ID)
		}
	}

	archive := []ArchiveCandidate{}
	for _, node := range nodes {
		value := values[node.ID]
		degree := degrees[node.ID]
		if governorArchiveCandidate(node, value, degree, archiveThreshold) {
			archive = append(archive, ArchiveCandidate{
				NodeID: node.ID, Value: value, Status: node.Status, Degree: degree,
				Reason: "low-value stale-or-dormant candidate",
			})
		}
	}
	sort.Slice(archive, func(i, j int) bool {
		if archive[i].Value == archive[j].Value { return archive[i].NodeID < archive[j].NodeID }
		return archive[i].Value < archive[j].Value
	})

	canonical := governorDuplicateTitleGroups(nodes)
	branches := []BranchCandidate{}
	for _, edge := range edges {
		if edge.Type != "contradicts" && edge.Type != "invalidates" { continue }
		weight := edge.Weight
		if weight == 0 { weight = 1 }
		branches = append(branches, BranchCandidate{
			EdgeID: edge.ID, From: edge.From, To: edge.To, Relation: edge.Type, Weight: weight,
		})
	}
	sort.Slice(branches, func(i, j int) bool {
		if branches[i].Weight == branches[j].Weight { return branches[i].EdgeID < branches[j].EdgeID }
		return branches[i].Weight > branches[j].Weight
	})

	promotions := governorPromotionGroups(nodes, promotionMin)
	staleCount := 0
	archivedCount := 0
	for _, node := range nodes {
		if node.Status == "stale" { staleCount++ }
		if node.Status == "archived" { archivedCount++ }
	}
	staleRatio := 0.0
	if len(nodes) > 0 { staleRatio = float64(staleCount) / float64(len(nodes)) }
	reasons := []string{}
	if staleRatio >= staleRatioThreshold { reasons = append(reasons, "stale-ratio") }
	if len(canonical) >= duplicateThreshold { reasons = append(reasons, "canonicalization-backlog") }
	if len(nodes) >= nodeThreshold { reasons = append(reasons, "graph-size") }

	return GovernorAnalysis{
		GeneratedAt: now,
		Metrics: GovernorMetrics{
			NodeCount: len(nodes), EdgeCount: len(edges), StaleCount: staleCount, ArchivedCount: archivedCount,
			StaleRatio: staleRatio, ContradictionCount: len(branches), DuplicateGroupCount: len(canonical),
			PromotionGroupCount: len(promotions),
			TierCounts: map[string]int{"hot": len(tiers.Hot), "warm": len(tiers.Warm), "cold": len(tiers.Cold)},
		},
		Values: values, Tiers: tiers,
		Candidates: GovernorCandidates{Archive: archive, Canonicalize: canonical, Branch: branches, Promote: promotions},
		Epoch: EpochRecommendation{Recommended: len(reasons) > 0, Reasons: reasons},
	}
}

type CanonicalizePlan struct {
	Canonical string `json:"canonical"`
	Aliases []string `json:"aliases"`
	Reason string `json:"reason,omitempty"`
}

type BranchPlan struct {
	From string `json:"from"`
	To string `json:"to"`
	Reason string `json:"reason,omitempty"`
}

type PromotePlan struct {
	Title string `json:"title,omitempty"`
	ChildIDs []string `json:"childIds"`
	Reason string `json:"reason,omitempty"`
}

type EpochPlan struct {
	Proposed bool `json:"proposed"`
	Reasons []string `json:"reasons,omitempty"`
}

type GovernorPlan struct {
	Archive []string `json:"archive,omitempty"`
	Tiers GovernorTiers `json:"tiers"`
	Canonicalize []CanonicalizePlan `json:"canonicalize,omitempty"`
	Branch []BranchPlan `json:"branch,omitempty"`
	Promote []PromotePlan `json:"promote,omitempty"`
	Epoch *EpochPlan `json:"epoch,omitempty"`
	Summary string `json:"summary,omitempty"`
}

type GovernorValidation struct {
	Valid bool `json:"valid"`
	Errors []string `json:"errors"`
	Warnings []string `json:"warnings"`
	Normalized GovernorPlan `json:"normalized"`
}

func DeterministicGovernorPlan(analysis GovernorAnalysis) GovernorPlan {
	archive := make([]string, 0, len(analysis.Candidates.Archive))
	for _, candidate := range analysis.Candidates.Archive { archive = append(archive, candidate.NodeID) }
	var epoch *EpochPlan
	if analysis.Epoch.Recommended {
		epoch = &EpochPlan{Proposed: true, Reasons: append([]string(nil), analysis.Epoch.Reasons...)}
	}
	return GovernorPlan{Archive: archive, Tiers: cloneGovernorTiers(analysis.Tiers), Epoch: epoch}
}

func ValidateGovernorPlan(plan GovernorPlan, graph GraphState) GovernorValidation {
	errorsOut := []string{}
	warnings := []string{}
	archive := uniqueGovernorStrings(plan.Archive)
	for _, id := range archive {
		node, ok := graph.Nodes[id]
		if !ok {
			errorsOut = append(errorsOut, "archive references unknown node: "+id)
			continue
		}
		if node.Kind == "evidence" && (node.Grade == "runtime" || node.Grade == "reproduced") {
			errorsOut = append(errorsOut, "high-grade evidence cannot be auto-archived: "+id)
		}
		if node.Status == "invalid" { warnings = append(warnings, "node is already invalid: "+id) }
	}

	tiers := GovernorTiers{
		Hot: uniqueGovernorStrings(plan.Tiers.Hot), Warm: uniqueGovernorStrings(plan.Tiers.Warm), Cold: uniqueGovernorStrings(plan.Tiers.Cold),
	}
	membership := map[string]string{}
	for tier, ids := range map[string][]string{"hot": tiers.Hot, "warm": tiers.Warm, "cold": tiers.Cold} {
		for _, id := range ids {
			if _, ok := graph.Nodes[id]; !ok { errorsOut = append(errorsOut, fmt.Sprintf("tier %s references unknown node: %s", tier, id)) }
			if existing, ok := membership[id]; ok && existing != tier { errorsOut = append(errorsOut, "node appears in multiple tiers: "+id) }
			membership[id] = tier
		}
	}

	for _, item := range plan.Canonicalize {
		canonical, ok := graph.Nodes[item.Canonical]
		if !ok || strings.TrimSpace(item.Canonical) == "" {
			errorsOut = append(errorsOut, "canonicalize canonical node is missing or unknown")
			continue
		}
		if governorMetadataString(canonical.Metadata, "canonicalNodeId") != "" {
			errorsOut = append(errorsOut, "canonical node is itself an alias: "+canonical.ID)
		}
		aliases := uniqueGovernorStrings(item.Aliases)
		if len(aliases) == 0 { warnings = append(warnings, "canonicalize has no aliases: "+canonical.ID) }
		for _, aliasID := range aliases {
			alias, exists := graph.Nodes[aliasID]
			if !exists { errorsOut = append(errorsOut, "canonicalize alias references unknown node: "+aliasID); continue }
			if aliasID == canonical.ID { errorsOut = append(errorsOut, "canonicalize alias equals canonical node: "+aliasID) }
			if alias.Kind != canonical.Kind {
				errorsOut = append(errorsOut, fmt.Sprintf("canonicalize kind mismatch: %s(%s) vs %s(%s)", canonical.ID, canonical.Kind, aliasID, alias.Kind))
			}
			existing := governorMetadataString(alias.Metadata, "canonicalNodeId")
			if existing != "" && existing != canonical.ID {
				errorsOut = append(errorsOut, fmt.Sprintf("alias already canonicalized elsewhere: %s -> %s", aliasID, existing))
			}
			if governorCanonicalGradeChecked(canonical.Kind) && governorGradeWeights[alias.Grade] > governorGradeWeights[canonical.Grade] {
				errorsOut = append(errorsOut, fmt.Sprintf("canonical node has lower evidence grade than alias: %s < %s", canonical.ID, aliasID))
			}
		}
	}

	for _, item := range plan.Branch {
		if _, ok := graph.Nodes[item.From]; !ok || strings.TrimSpace(item.From) == "" { errorsOut = append(errorsOut, "branch from node is missing or unknown") }
		if _, ok := graph.Nodes[item.To]; !ok || strings.TrimSpace(item.To) == "" { errorsOut = append(errorsOut, "branch to node is missing or unknown") }
		if item.From != "" && item.To != "" && item.From == item.To { errorsOut = append(errorsOut, "branch endpoints must be different nodes") }
	}

	for _, item := range plan.Promote {
		children := uniqueGovernorStrings(item.ChildIDs)
		if len(children) < 3 { warnings = append(warnings, "promotion has fewer than three children") }
		for _, id := range children {
			if _, ok := graph.Nodes[id]; !ok { errorsOut = append(errorsOut, "promotion references unknown node: "+id) }
		}
	}

	normalized := GovernorPlan{
		Archive: archive, Tiers: tiers,
		Canonicalize: append([]CanonicalizePlan(nil), plan.Canonicalize...),
		Branch: append([]BranchPlan(nil), plan.Branch...),
		Promote: append([]PromotePlan(nil), plan.Promote...),
		Epoch: plan.Epoch, Summary: governorTrim(plan.Summary, 4000),
	}
	return GovernorValidation{Valid: len(errorsOut) == 0, Errors: errorsOut, Warnings: warnings, Normalized: normalized}
}

func governorNodeValue(node GraphNode, degree int, maxDegree int, storage StorageTelemetry, now time.Time) float64 {
	grade := governorGradeWeights[node.Grade]
	if grade == 0 { grade = 0.45 }
	trust := governorTrustWeights[node.TrustZone]
	if trust == 0 { trust = 0.5 }
	lifecycle, ok := governorStatusWeights[node.Status]
	if !ok { lifecycle = 0.5 }
	centrality := math.Log2(float64(degree)+1) / math.Log2(float64(maxDegree)+1)
	activation := governorMetadataNumber(node.Metadata, "activationFrequency")
	if activation == 0 { activation = governorMetadataNumber(node.Metadata, "retrievalContribution") }
	activation = clamp01(activation)
	accessFrequency := 0.0
	if storage.AccessCount > 0 { accessFrequency = clamp01(math.Log2(float64(storage.AccessCount)+1) / 8) }
	accessRecency := 0.0
	if !storage.LastAccessAt.IsZero() {
		delta := now.Sub(storage.LastAccessAt)
		if delta < 0 { delta = 0 }
		accessRecency = math.Exp(-float64(delta) / float64(30*24*time.Hour))
	}
	return clamp01(0.25*grade + 0.18*trust + 0.22*lifecycle + 0.16*centrality + 0.09*activation + 0.06*accessFrequency + 0.04*accessRecency)
}

func governorArchiveCandidate(node GraphNode, value float64, degree int, threshold float64) bool {
	if node.Status != "stale" && node.Status != "dormant" { return false }
	if node.Kind == "task" || node.Kind == "abstraction" { return false }
	if node.Kind == "evidence" && (node.Grade == "runtime" || node.Grade == "reproduced") { return false }
	if degree > 1 { return false }
	return value < threshold
}

func governorDuplicateTitleGroups(nodes []GraphNode) []CanonicalizeCandidate {
	groups := map[string][]GraphNode{}
	for _, node := range nodes {
		if node.Status == "invalid" { continue }
		title := governorNormalizeTitle(node.Title)
		if title == "" { continue }
		key := node.Kind + ":" + title
		groups[key] = append(groups[key], node)
	}
	keys := []string{}
	for key, items := range groups { if len(items) > 1 { keys = append(keys, key) } }
	sort.Strings(keys)
	out := make([]CanonicalizeCandidate, 0, len(keys))
	for _, key := range keys {
		items := groups[key]
		sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
		ids := []string{}
		titles := []string{}
		for _, node := range items { ids = append(ids, node.ID); titles = append(titles, node.Title) }
		out = append(out, CanonicalizeCandidate{Kind: items[0].Kind, NormalizedTitle: governorNormalizeTitle(items[0].Title), NodeIDs: ids, Titles: titles})
	}
	return out
}

func governorPromotionGroups(nodes []GraphNode, minGroup int) []PromotionCandidate {
	groups := map[string][]string{}
	for _, node := range nodes {
		if node.Status != "active" || node.Kind == "abstraction" || node.Kind == "task" { continue }
		for _, tag := range node.Tags {
			normalized := strings.ToLower(strings.TrimSpace(tag))
			if normalized == "" || normalized == "agent-session" || normalized == "tool-observation" { continue }
			groups[normalized] = append(groups[normalized], node.ID)
		}
	}
	out := []PromotionCandidate{}
	for tag, ids := range groups {
		if len(ids) < minGroup { continue }
		sort.Strings(ids)
		out = append(out, PromotionCandidate{Tag: tag, NodeIDs: uniqueGovernorStrings(ids)})
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].NodeIDs) == len(out[j].NodeIDs) { return out[i].Tag < out[j].Tag }
		return len(out[i].NodeIDs) > len(out[j].NodeIDs)
	})
	return out
}

func governorDegreeMap(nodes []GraphNode, edges []GraphEdge) map[string]int {
	out := map[string]int{}
	for _, node := range nodes { out[node.ID] = 0 }
	for _, edge := range edges { out[edge.From]++; out[edge.To]++ }
	return out
}

func governorNormalizeTitle(value string) string {
	fields := []string{}
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 { return }
		fields = append(fields, current.String())
		current.Reset()
	}
	for _, r := range strings.ToLower(value) {
		if unicode.IsLetter(r) || unicode.IsNumber(r) { current.WriteRune(r) } else { flush() }
	}
	flush()
	return strings.Join(fields, " ")
}

func sortedGovernorNodes(values map[string]GraphNode) []GraphNode {
	out := make([]GraphNode, 0, len(values))
	for id, node := range values {
		if node.ID == "" { node.ID = id }
		if node.Status == "" { node.Status = "active" }
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func sortedGovernorEdges(values map[string]GraphEdge) []GraphEdge {
	out := make([]GraphEdge, 0, len(values))
	for id, edge := range values { if edge.ID == "" { edge.ID = id }; out = append(out, edge) }
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func governorMetadataNumber(metadata map[string]any, key string) float64 {
	if metadata == nil { return 0 }
	value, ok := metadata[key]
	if !ok { return 0 }
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case string:
		number, _ := strconv.ParseFloat(typed, 64); return number
	default:
		return 0
	}
}

func governorMetadataString(metadata map[string]any, key string) string {
	if metadata == nil { return "" }
	value, ok := metadata[key]
	if !ok || value == nil { return "" }
	return strings.TrimSpace(fmt.Sprint(value))
}

func governorCanonicalGradeChecked(kind string) bool {
	switch kind {
	case "evidence", "belief", "negative", "abstraction":
		return true
	default:
		return false
	}
}

func uniqueGovernorStrings(values []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] { continue }
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func cloneGovernorTiers(value GovernorTiers) GovernorTiers {
	return GovernorTiers{Hot: append([]string(nil), value.Hot...), Warm: append([]string(nil), value.Warm...), Cold: append([]string(nil), value.Cold...)}
}

func governorTrim(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max { return value }
	return value[:max]
}
