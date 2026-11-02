package runtime

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

const (
	MaxSubagentTreeDepth = 4
	MaxSubagentTreeNodes = 100
)

var ErrSubagentLimitExceeded = errors.New("subagent concurrency limit exceeded for parent session")

type SubagentNode struct {
	SessionID       string `json:"sessionId"`
	ParentSessionID string `json:"parentSessionId,omitempty"`
	// Kind is the relation: RelationSubagent or RelationExecution.
	Kind string `json:"kind"`
	// Mode describes the child relation's execution provenance (for example, "imported").
	Mode string `json:"mode,omitempty"`
	// Agent is the catalog agent the child pinned at its first Start (ADR-0009 A3), "" before it.
	Agent  string `json:"agent,omitempty"`
	Goal   string `json:"goal"`
	Status string `json:"status"`
	Final  string `json:"final,omitempty"`
	// Withdrawn is true when a revocation or privacy fence names the child: its goal and final are not
	// returned, to the host or to the list_subagents/wait_subagent tools, and no edge cites it.
	Withdrawn bool           `json:"withdrawn,omitempty"`
	Active    bool           `json:"active"`
	CreatedAt time.Time      `json:"createdAt"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Runtime   SessionRuntime `json:"runtime"`
	Children  []SubagentNode `json:"children,omitempty"`
}

// subagentNode is the node of one child session: its row, its runtime identity, and its content only
// when the child is not withdrawn.
func (s *RunSupervisor) subagentNode(ctx context.Context, childID, parentID, kind string) (SubagentNode, error) {
	value, err := s.engine.store.Get(ctx, childID)
	if err != nil {
		return SubagentNode{}, err
	}
	identity, err := s.engine.SessionRuntime(ctx, value.ID)
	if err != nil {
		return SubagentNode{}, err
	}
	withdrawn, err := s.engine.store.SessionWithdrawn(ctx, value.ID)
	if err != nil {
		return SubagentNode{}, err
	}
	node := SubagentNode{
		SessionID: value.ID, ParentSessionID: parentID, Kind: kind, Agent: value.AgentID, Status: value.Status,
		Withdrawn: withdrawn, Active: s.Active(value.ID), CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Runtime: identity,
	}
	if !withdrawn {
		node.Goal = value.Goal
		if value.Final != nil {
			node.Final = *value.Final
		}
	}
	return node, nil
}

type SessionCheckpoint = session.Checkpoint

type SubagentWaitResult struct {
	Node     SubagentNode `json:"node"`
	TimedOut bool         `json:"timedOut"`
}

type subagentController struct {
	supervisor *RunSupervisor
	parentID   string
	// The child's provider is built by the Engine from its model reference.
	parentOpts AgentOptions
}

func (c *subagentController) Spawn(ctx context.Context, goal, agentID string) (any, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, errors.New("subagent goal is required")
	}
	if c.supervisor == nil || c.supervisor.engine == nil {
		return nil, ErrRunSupervisorClosed
	}
	engine := c.supervisor.engine
	agentID = cmp.Or(strings.TrimSpace(agentID), config.AgentExplore)
	if err := engine.delegationAgent(agentID, config.AgentInfo.SpawnEligible); err != nil {
		return nil, err
	}

	if err := c.supervisor.checkChildLimit(ctx, c.parentID); err != nil {
		return nil, err
	}

	identity, err := engine.SessionRuntime(ctx, c.parentID)
	if err != nil {
		return nil, err
	}

	metadata := map[string]any{
		"subagent": map[string]any{
			"parentSessionId": c.parentID,
			"mode":            "read-only",
		},
	}
	if identity.Kind == RuntimeWorktree {
		// The child borrows its parent's worktree (ADR-0005 §1): it may run there, under
		// the shared lease, but never remove, discard or apply it.
		_, owner, _, err := engine.worktreeRole(ctx, c.parentID, identity)
		if err != nil {
			return nil, err
		}
		identity.Owner = owner
		metadata["runtime"] = identity
	}

	childRef, err := engine.childModelRef(ctx, c.parentID, agentID)
	if err != nil {
		return nil, err
	}

	handle, err := engine.createSession(ctx, SessionOptions{
		Goal:     goal,
		Metadata: metadata,
	})
	if err != nil {
		return nil, err
	}

	relationMetadata, _ := json.Marshal(map[string]any{
		"mode":  "read-only",
		"agent": agentID,
	})
	if err := engine.store.AddRelation(ctx, session.Relation{
		ParentSessionID: c.parentID,
		ChildSessionID:  handle.ID,
		Kind:            RelationSubagent,
		Metadata:        relationMetadata,
	}); err != nil {
		return nil, err
	}
	// The checkpoint follows the child by identifier only: the goal it was spawned for is already
	// the child's own session row and its first transcript message, and a parent-side copy of it
	// would be a second body that no deletion of the child reaches.
	_, _ = engine.store.AppendCheckpoint(context.Background(), c.parentID, "subagent.spawn", map[string]any{
		"childSessionId": handle.ID,
		"status":         "created",
		"agent":          agentID,
	})

	childOpts := c.childOptions(childRef, agentID)
	if until, ok := protocol.ToolDeadline(ctx); ok {
		// A child of a run with a deadline must be done by the time its parent's tools must be (ADR-0019 §2).
		childOpts.Deadline = &until
	}
	if _, err := c.supervisor.Start(ctx, handle.ID, childOpts); err != nil {
		status := "interrupted"
		errorRaw, _ := json.Marshal(map[string]any{
			"kind":    "subagent_start_failed",
			"message": err.Error(),
		})
		_ = engine.store.Update(context.Background(), handle.ID, session.Patch{
			Status: &status,
			Error:  rawMessagePtr(errorRaw),
		})
		// The failure text is already the child's Error column; the checkpoint keeps the child, how
		// it ended and which agent ran it.
		_, _ = engine.store.AppendCheckpoint(context.Background(), c.parentID, "subagent.start_failed", map[string]any{
			"childSessionId": handle.ID,
			"status":         "interrupted",
			"agent":          agentID,
		})
		return nil, err
	}

	engine.events.publish("subagent.spawned", c.parentID, map[string]any{
		"childSessionId": handle.ID,
		"goal":           goal,
		"agent":          agentID,
	})
	return c.summary(ctx, handle.ID)
}

func (c *subagentController) List(ctx context.Context) (any, error) {
	return c.supervisor.SubagentTree(ctx, c.parentID)
}

// Wait blocks until the child's run ends or the wait ends. The wait ends at the earliest of timeout
// (default 30 s, at most 60 s) and the run's tool deadline (ADR-0019 §2); a deadline already past is a
// timed-out wait, never the default timeout. A timed-out wait leaves the child running. Cancelling ctx
// ends the wait with ctx's error, even when the deadline has passed as well.
func (c *subagentController) Wait(ctx context.Context, childSessionID string, timeout time.Duration) (any, error) {
	childSessionID = strings.TrimSpace(childSessionID)
	if childSessionID == "" {
		return nil, errors.New("child session id is required")
	}
	relation, err := c.supervisor.engine.store.ParentRelation(ctx, childSessionID)
	if err != nil {
		return nil, err
	}
	if relation.Kind != RelationSubagent || relation.ParentSessionID != c.parentID {
		return nil, errors.New("subagent does not belong to this parent session")
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > 60*time.Second {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if until, ok := protocol.ToolDeadline(ctx); ok && until.Before(deadline) {
		deadline = until
	}

	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for c.supervisor.Active(childSessionID) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-waitCtx.Done():
			// waitCtx also ends with ctx: a cancelled caller is never reported as a timeout.
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			node, summaryErr := c.summary(context.Background(), childSessionID)
			if summaryErr != nil {
				return nil, summaryErr
			}
			return SubagentWaitResult{Node: node, TimedOut: true}, nil
		case <-ticker.C:
		}
	}
	node, err := c.summary(ctx, childSessionID)
	if err != nil {
		return nil, err
	}
	return SubagentWaitResult{Node: node}, nil
}

// explorePrompt is the fixed prompt of a read-only child (the explore agent). The launch, not the
// model, fixes the authority: no writes, no bash, no spawning. Since ADR-0019 removed git_status and
// git_diff it cannot read Git state either: it reads the working tree as it is, and a parent that needs
// a diff examined puts it in the goal. The handoff format adapts oh-my-pi's scout prompt; see
// THIRD_PARTY_NOTICES.md.
const explorePrompt = `You are a focused read-only LumenCortex subagent. Investigate the assigned goal and report findings another agent can use without re-reading everything.
- Never modify files, run commands or spawn agents. Read, search and query language services only; you cannot run git, so you see files as they are now, not what changed (a diff you must examine is in the goal).
- Use search_text, find_files, read_file, list_dir and lsp_* tools. Open with a search for any behaviour you can describe.
- Issue independent searches in parallel, within the per-step call limit; this is a short investigation.
- If a search is empty, try at least one alternate strategy (different pattern, broader path, another tool) before concluding something is absent.
- Infer thoroughness from the goal and default to medium. Quick: targeted lookups, key files only. Medium: follow imports, read critical sections. Thorough: trace dependencies, tests and types.
- Never read a whole large file: read the sections that matter.
- End with a compact markdown handoff: **Summary** (findings and conclusions, brief); **Files** (each as path:start-end and what it holds); **Architecture** (how the pieces connect, brief); **Report** only when the goal asks for a report, table or enumeration: the complete deliverable, never a summary of it.`

// childOptions are a read-only child's run options. The launch fixes the authority (read-only, no
// MCP, no spawn); agentID is the agent it runs, pinned by its first Start.
func (c *subagentController) childOptions(childRef config.ModelRef, agentID string) AgentOptions {
	maxSteps := c.parentOpts.MaxSteps
	if maxSteps <= 0 || maxSteps > 12 {
		maxSteps = 12
	}
	maxCalls := c.parentOpts.MaxToolCallsPerStep
	if maxCalls <= 0 || maxCalls > 6 {
		maxCalls = 6
	}
	return AgentOptions{
		ModelRef:            childRef,
		Policy:              toolset.PolicyReadOnly,
		MaxSteps:            maxSteps,
		MaxToolCallsPerStep: maxCalls,
		ToolDenylist:        append([]string(nil), c.parentOpts.ToolDenylist...),
		SystemPrompt:        explorePrompt,
		MaxTokens:           c.parentOpts.MaxTokens,
		Temperature:         c.parentOpts.Temperature,
		parentSessionID:     c.parentID,
		childAgent:          agentID,
		disableSubagents:    true,
		disableMCP:          true,
	}
}

func (c *subagentController) summary(ctx context.Context, childID string) (SubagentNode, error) {
	return c.supervisor.subagentNode(ctx, childID, c.parentID, RelationSubagent)
}

func (s *RunSupervisor) HasActiveSubagents(ctx context.Context, parentSessionID string) (bool, error) {
	if s == nil || s.engine == nil {
		return false, ErrRunSupervisorClosed
	}
	relations, err := s.engine.store.ChildRelations(ctx, parentSessionID, 100)
	if err != nil {
		return false, err
	}
	for _, relation := range relations {
		if relation.Kind == RelationSubagent && s.Active(relation.ChildSessionID) {
			return true, nil
		}
	}
	return false, nil
}

// SubagentTree lists a session's children, read-only subagents and execution tasks (Kind), with their
// own children, bounded by MaxSubagentTreeDepth and MaxSubagentTreeNodes.
func (s *RunSupervisor) SubagentTree(ctx context.Context, parentSessionID string) ([]SubagentNode, error) {
	if s == nil || s.engine == nil {
		return nil, ErrRunSupervisorClosed
	}
	remaining := MaxSubagentTreeNodes
	return s.subagentChildren(ctx, parentSessionID, 0, &remaining)
}

func (s *RunSupervisor) subagentChildren(ctx context.Context, parentSessionID string, depth int, remaining *int) ([]SubagentNode, error) {
	if depth >= MaxSubagentTreeDepth || *remaining <= 0 {
		return []SubagentNode{}, nil
	}
	relations, err := s.engine.store.ChildRelations(ctx, parentSessionID, 100)
	if err != nil {
		return nil, err
	}
	out := make([]SubagentNode, 0, len(relations))
	for _, relation := range relations {
		if (relation.Kind != RelationSubagent && relation.Kind != RelationExecution) || *remaining <= 0 {
			continue
		}
		node, err := s.subagentNode(ctx, relation.ChildSessionID, parentSessionID, relation.Kind)
		if err != nil {
			return nil, err
		}
		var relationMeta struct {
			Mode string `json:"mode"`
		}
		_ = json.Unmarshal(relation.Metadata, &relationMeta)
		node.Mode = relationMeta.Mode
		(*remaining)--
		children, err := s.subagentChildren(ctx, node.SessionID, depth+1, remaining)
		if err != nil {
			return nil, err
		}
		node.Children = children
		out = append(out, node)
	}
	return out, nil
}

func (e *Engine) SessionCheckpoints(ctx context.Context, sessionID string, limit int) ([]SessionCheckpoint, error) {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return nil, err
	}
	return e.store.Checkpoints(ctx, sessionID, limit)
}

// checkpointSubagentStop records a finished child on its parent. The checkpoint carries the three
// fields a parent needs to follow a child — which child, how it ended, which agent ran it — and no
// copy of the child's final text or failure message: those live on the child's session row and in
// the event below, which the Desktop reads.
func (e *Engine) checkpointSubagentStop(parentID, childID string, runErr error) {
	event := map[string]any{"childSessionId": childID}
	checkpoint := map[string]any{"childSessionId": childID}
	if value, err := e.store.Get(context.Background(), childID); err == nil {
		event["status"] = value.Status
		checkpoint["status"] = value.Status
		if value.AgentID != "" {
			checkpoint["agent"] = value.AgentID
		}
		if value.Final != nil {
			event["final"] = *value.Final
		}
	}
	if runErr != nil {
		event["error"] = runErr.Error()
	}
	_, _ = e.store.AppendCheckpoint(context.Background(), parentID, "subagent.stopped", checkpoint)
	e.events.publish("subagent.stopped", parentID, event)
}

func subagentEventData(eventType, childID string, data map[string]any) map[string]any {
	out := map[string]any{
		"childSessionId": childID,
		"eventType":      eventType,
	}
	if data != nil {
		out["data"] = data
	}
	return out
}

func validateSubagentParent(parentID string) error {
	if strings.TrimSpace(parentID) == "" {
		return fmt.Errorf("subagent parent session id is required")
	}
	return nil
}
