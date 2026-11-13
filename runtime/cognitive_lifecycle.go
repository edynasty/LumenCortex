package runtime

import (
	"context"
	"errors"
	"slices"

	"github.com/edynasty/LumenCortex/internal/knowledge"
	"github.com/edynasty/LumenCortex/internal/session"
)

// SetCognitiveLifecycle is a host operation, never a model tool. Restore does not
// establish truth: current source/dependency/time predicates must still hold and
// evidence must be reassessed separately. Invalid identities cannot be revived.
func (e *Engine) SetCognitiveLifecycle(ctx context.Context, owner, id string, expected int64, action, reason string) (CognitiveNode, error) {
	if !knowledge.SafeExperienceText(reason, 512) {
		return CognitiveNode{}, errors.New("lifecycle reason is required")
	}
	lock, err := e.acquireRun(ctx, owner)
	if err != nil {
		return CognitiveNode{}, err
	}
	defer lock.Release()
	n, err := e.readCognitiveNode(ctx, owner, id, 0)
	if err != nil {
		return CognitiveNode{}, err
	}
	if n.Version != expected {
		return CognitiveNode{}, session.ErrCognitiveRevision
	}
	switch action {
	case "archive":
		n.Status = "archived"
	case "restore":
		n.Status = "active"
	case "revoke":
		n.Status = "invalid"
	default:
		return CognitiveNode{}, errors.New("expected archive, restore or revoke")
	}
	n.Version++
	n.Times.RecordedAt = e.now()
	n.RevisionReason = reason
	if action == "revoke" && n.Times.InvalidatedAt.IsZero() {
		n.Times.InvalidatedAt = n.Times.RecordedAt
	}
	reader, err := e.cognitiveReader(ctx, owner)
	if err != nil {
		return CognitiveNode{}, err
	}
	if err := e.store.CommitCognitiveLifecycle(ctx, []knowledge.Node{n}, map[string]int64{id: expected}, reader); err != nil {
		return CognitiveNode{}, err
	}
	return experienceForReader(n, owner), nil
}

// GovernorApply accepts a host-selected plan and exact expected revisions. No
// agent can grant this authority. Heterogeneous-owner batches are refused, not
// partially applied. Branch/promote/epoch proposals remain analysis-only because
// they require substantive new claims, not just a memory lifecycle operation.
func (e *Engine) GovernorApply(ctx context.Context, owner string, plan GovernorPlan, expected map[string]int64, reason string) ([]CognitiveNode, error) {
	if !knowledge.SafeExperienceText(reason, 512) {
		return nil, errors.New("governor reason is required")
	}
	if len(plan.Branch) > 0 || len(plan.Promote) > 0 || (plan.Epoch != nil && plan.Epoch.Proposed) {
		return nil, errors.New("branch, promote and epoch are proposals, not authorized lifecycle changes")
	}
	lock, err := e.acquireRun(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	validation, err := e.GovernorValidate(ctx, plan)
	if err != nil {
		return nil, err
	}
	if !validation.Valid {
		return nil, errors.New("invalid governor plan")
	}
	plan = validation.Normalized
	nodes := map[string]knowledge.Node{}
	load := func(id string) (knowledge.Node, error) {
		if n, ok := nodes[id]; ok {
			return n, nil
		}
		n, err := e.readCognitiveNode(ctx, owner, id, 0)
		if err != nil {
			return n, err
		}
		if expected[id] != n.Version {
			return n, session.ErrCognitiveRevision
		}
		if n.Memory == nil {
			n.Memory = &knowledge.MemoryPolicy{}
		}
		return n, nil
	}
	for _, tier := range []struct {
		name string
		ids  []string
	}{{"hot", plan.Tiers.Hot}, {"warm", plan.Tiers.Warm}, {"cold", plan.Tiers.Cold}} {
		for _, id := range tier.ids {
			n, err := load(id)
			if err != nil {
				return nil, err
			}
			n.Memory.Tier = tier.name
			nodes[id] = n
		}
	}
	for _, id := range plan.Archive {
		n, err := load(id)
		if err != nil {
			return nil, err
		}
		n.Status = "archived"
		nodes[id] = n
	}
	for _, group := range plan.Canonicalize {
		canonical, err := e.readCognitiveNode(ctx, owner, group.Canonical, 0)
		if err != nil {
			return nil, err
		}
		if expected[group.Canonical] != canonical.Version {
			return nil, session.ErrCognitiveRevision
		}
		for _, id := range group.Aliases {
			n, err := load(id)
			if err != nil {
				return nil, err
			}
			n.Status = "archived"
			n.Memory.Canonical = &knowledge.NodeRef{ID: canonical.ID, Version: canonical.Version}
			nodes[id] = n
		}
	}
	if len(nodes) == 0 {
		return []CognitiveNode{}, nil
	}
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	revisions := make([]knowledge.Node, 0, len(nodes))
	versions := make(map[string]int64, len(nodes))
	for _, id := range ids {
		n := nodes[id]
		versions[id] = n.Version
		n.Version++
		n.Times.RecordedAt = e.now()
		n.RevisionReason = reason
		revisions = append(revisions, n)
	}
	reader, err := e.cognitiveReader(ctx, owner)
	if err != nil {
		return nil, err
	}
	if err := e.store.CommitCognitiveLifecycle(ctx, revisions, versions, reader); err != nil {
		return nil, err
	}
	for i := range revisions {
		revisions[i] = experienceForReader(revisions[i], owner)
	}
	return revisions, nil
}

// MaintainCognitiveMemory bounds the active set without deleting episodes or
// evidence. It archives only already stale/dormant, unprotected, unreferenced
// owned nodes. If safety/unknown/active premises fill the budget it reports
// pressure instead of deleting important knowledge to hit a numerical target.
type CognitiveMemoryMaintenance struct {
	Active    int      `json:"active"`
	Protected int      `json:"protected"`
	Archived  []string `json:"archived"`
	Pressure  bool     `json:"pressure"`
}

func (e *Engine) MaintainCognitiveMemory(ctx context.Context, owner string, capacity int) (CognitiveMemoryMaintenance, error) {
	result := CognitiveMemoryMaintenance{}
	if capacity < 1 {
		return result, errors.New("positive active-memory capacity required")
	}
	lock, err := e.acquireRun(ctx, owner)
	if err != nil {
		return result, err
	}
	defer lock.Release()
	reader, err := e.cognitiveReader(ctx, owner)
	if err != nil {
		return result, err
	}
	nodes, edges, err := e.store.CognitiveGraph(ctx, reader)
	if err != nil {
		return result, err
	}
	referenced := map[string]bool{}
	for _, n := range nodes {
		for _, dep := range n.Dependencies {
			referenced[dep.ID] = true
		}
	}
	for _, edge := range edges {
		referenced[edge.From] = true
		referenced[edge.To] = true
	}
	var eligible []knowledge.Node
	for _, n := range nodes {
		if n.SessionID != owner && (n.Experience == nil || n.Experience.Origin.Source.SessionID != owner) {
			continue
		}
		if n.Status == "archived" || n.Status == "invalid" {
			continue
		}
		result.Active++
		if n.ProtectedMemory() || referenced[n.ID] {
			result.Protected++
			continue
		}
		if n.Status == "stale" || n.Status == "dormant" {
			eligible = append(eligible, n)
		}
	}
	slices.SortStableFunc(eligible, func(a, b knowledge.Node) int {
		ai, bi := 0.0, 0.0
		if a.Memory != nil {
			ai = a.Memory.Importance
		}
		if b.Memory != nil {
			bi = b.Memory.Importance
		}
		if ai < bi {
			return -1
		}
		if ai > bi {
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		return 1
	})
	count := min(max(0, result.Active-capacity), len(eligible))
	count = min(count, 512)
	versions := map[string]int64{}
	for i := 0; i < count; i++ {
		n := &eligible[i]
		versions[n.ID] = n.Version
		n.Version++
		n.Status = "archived"
		n.RevisionReason = "bounded_memory_maintenance"
		n.Times.RecordedAt = e.now()
		result.Archived = append(result.Archived, n.ID)
	}
	if count > 0 {
		if err := e.store.CommitCognitiveLifecycle(ctx, eligible[:count], versions, reader); err != nil {
			return CognitiveMemoryMaintenance{}, err
		}
	}
	result.Active -= count
	result.Pressure = result.Active > capacity
	return result, nil
}

// CognitiveMemoryInfo separates attention, applicability and observed outcomes.
// Only the requesting reader's accesses and feedback are returned.
type CognitiveMemoryInfo struct {
	ID        string                          `json:"id"`
	Version   int64                           `json:"version"`
	Class     string                          `json:"class"`
	Policy    *CognitiveMemoryPolicy          `json:"policy,omitempty"`
	Protected bool                            `json:"protected"`
	Access    session.CognitiveAccessCount    `json:"access"`
	Outcomes  *knowledge.ExperienceStatistics `json:"outcomes,omitempty"`
	Utility   string                          `json:"utility"`
}

func (e *Engine) CognitiveMemory(ctx context.Context, readerID, id string) (CognitiveMemoryInfo, error) {
	n, err := e.ReadCognitiveNode(ctx, readerID, id, 0)
	if err != nil {
		return CognitiveMemoryInfo{}, err
	}
	count, err := e.store.CognitiveAccess(ctx, readerID, id)
	if err != nil {
		return CognitiveMemoryInfo{}, err
	}
	info := CognitiveMemoryInfo{ID: n.ID, Version: n.Version, Class: n.MemoryClass(), Policy: n.Memory, Protected: n.ProtectedMemory(), Access: count, Utility: "causal_benefit_unmeasured"}
	if n.Experience != nil {
		stats := n.Experience.Statistics()
		info.Outcomes = &stats
	}
	return info, nil
}
