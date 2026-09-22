package runtime

import (
	"context"
	"errors"
	"sort"

	"github.com/edynasty/LumenCortex/internal/repository"
)

type WorktreeHandoffPlan struct {
	SessionID        string         `json:"sessionId"`
	Runtime          SessionRuntime `json:"runtime"`
	TargetBranch     string         `json:"targetBranch,omitempty"`
	TargetHead       string         `json:"targetHead,omitempty"`
	SourceHead       string         `json:"sourceHead,omitempty"`
	Commits          []string       `json:"commits"`
	SourceFiles      []string       `json:"sourceFiles"`
	TargetFiles      []string       `json:"targetFiles"`
	OverlappingFiles []string       `json:"overlappingFiles"`
	SourceDirty      bool           `json:"sourceDirty"`
	TargetDirty      bool           `json:"targetDirty"`
	CanApply         bool           `json:"canApply"`
	BlockedReason    string         `json:"blockedReason,omitempty"`
}

type WorktreeApplyResult struct {
	Plan       WorktreeHandoffPlan `json:"plan"`
	Action     GitActionResult     `json:"action"`
	TargetHead string              `json:"targetHead"`
}

func (e *Engine) WorktreeHandoffPlan(ctx context.Context, sessionID string) (WorktreeHandoffPlan, error) {
	identity, err := e.SessionRuntime(ctx, sessionID)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	if identity.Kind != RuntimeWorktree {
		return WorktreeHandoffPlan{}, errors.New("session does not use a worktree runtime")
	}

	sourceRepo, err := repository.New(identity.Path)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	sourceStatus, err := sourceRepo.GitStatus(ctx)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	targetStatus, err := e.repo.GitStatus(ctx)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	sourceHead, err := sourceRepo.GitHead(ctx)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	targetHead, err := e.repo.GitHead(ctx)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	targetBranch, err := e.repo.GitCurrentBranch(ctx)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}

	base := identity.Head
	if base == "" {
		return WorktreeHandoffPlan{}, errors.New("worktree base commit is missing")
	}
	commits, err := sourceRepo.GitCommitsBetween(ctx, base, sourceHead)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	sourceFiles, err := sourceRepo.GitChangedFilesBetween(ctx, base, sourceHead)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	targetFiles, err := e.repo.GitChangedFilesBetween(ctx, base, targetHead)
	if err != nil {
		return WorktreeHandoffPlan{}, err
	}
	overlap := intersectPaths(sourceFiles, targetFiles)

	plan := WorktreeHandoffPlan{
		SessionID:        sessionID,
		Runtime:          identity,
		TargetBranch:     targetBranch,
		TargetHead:       targetHead,
		SourceHead:       sourceHead,
		Commits:          commits,
		SourceFiles:      sourceFiles,
		TargetFiles:      targetFiles,
		OverlappingFiles: overlap,
		SourceDirty:      len(sourceStatus.Files) > 0,
		TargetDirty:      len(targetStatus.Files) > 0,
	}
	switch {
	case plan.SourceDirty:
		plan.BlockedReason = "worktree has uncommitted changes"
	case plan.TargetDirty:
		plan.BlockedReason = "target workspace has uncommitted changes"
	case len(plan.Commits) == 0:
		plan.BlockedReason = "worktree has no commits to apply"
	case len(plan.OverlappingFiles) > 0:
		plan.BlockedReason = "worktree and target changed overlapping files"
	default:
		plan.CanApply = true
	}
	return plan, nil
}

func (e *Engine) ApplySessionWorktree(ctx context.Context, sessionID string) (WorktreeApplyResult, error) {
	plan, err := e.WorktreeHandoffPlan(ctx, sessionID)
	if err != nil {
		return WorktreeApplyResult{}, err
	}
	if !plan.CanApply {
		return WorktreeApplyResult{Plan: plan}, errors.New(plan.BlockedReason)
	}

	action, err := e.repo.GitCherryPick(ctx, plan.Commits)
	if err != nil {
		return WorktreeApplyResult{Plan: plan, Action: action}, err
	}
	head, err := e.repo.GitHead(ctx)
	if err != nil {
		return WorktreeApplyResult{Plan: plan, Action: action}, err
	}
	e.events.publish("worktree.applied", sessionID, map[string]any{
		"branch": plan.Runtime.Branch,
		"commits": len(plan.Commits),
		"targetHead": head,
	})
	return WorktreeApplyResult{Plan: plan, Action: action, TargetHead: head}, nil
}

func intersectPaths(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, path := range right {
		rightSet[path] = struct{}{}
	}
	out := make([]string, 0)
	for _, path := range left {
		if _, ok := rightSet[path]; ok {
			out = append(out, path)
		}
	}
	sort.Strings(out)
	return out
}
