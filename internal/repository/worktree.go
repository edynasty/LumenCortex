package repository

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
)

const MaxWorktreeOutputBytes = 256 << 10

type GitWorktree struct {
	Path      string `json:"path"`
	Head      string `json:"head,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Detached  bool   `json:"detached,omitempty"`
	Locked    bool   `json:"locked,omitempty"`
	Prunable  bool   `json:"prunable,omitempty"`
}

func (s *Service) GitRoot(ctx context.Context) (string, error) {
	raw, _, err := runBounded(ctx, s.workspace, 8<<10, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	root := strings.TrimSpace(string(raw))
	if root == "" {
		return "", errors.New("git repository root is empty")
	}
	return filepath.Clean(root), nil
}

func (s *Service) GitWorktrees(ctx context.Context) ([]GitWorktree, error) {
	raw, _, err := runBounded(ctx, s.workspace, MaxWorktreeOutputBytes, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(string(raw)), nil
}

func (s *Service) GitCreateWorktree(ctx context.Context, path, branch, base string) (GitWorktree, error) {
	path = filepath.Clean(strings.TrimSpace(path))
	branch = strings.TrimSpace(branch)
	base = strings.TrimSpace(base)
	if path == "" || path == "." {
		return GitWorktree{}, errors.New("worktree path is required")
	}
	if branch == "" {
		return GitWorktree{}, errors.New("worktree branch is required")
	}
	if _, _, err := runBounded(ctx, s.workspace, 16<<10, "git", "check-ref-format", "--branch", branch); err != nil {
		return GitWorktree{}, err
	}
	if base == "" {
		base = "HEAD"
	}
	if _, _, err := runBounded(ctx, s.workspace, MaxWorktreeOutputBytes, "git", "worktree", "add", "-b", branch, path, base); err != nil {
		return GitWorktree{}, err
	}
	worktrees, err := s.GitWorktrees(ctx)
	if err != nil {
		return GitWorktree{}, err
	}
	clean := filepath.Clean(path)
	for _, item := range worktrees {
		if filepath.Clean(item.Path) == clean {
			return item, nil
		}
	}
	return GitWorktree{Path: clean, Branch: branch}, nil
}

func (s *Service) GitRemoveWorktree(ctx context.Context, path string, force bool) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return errors.New("worktree path is required")
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	_, _, err := runBounded(ctx, s.workspace, MaxWorktreeOutputBytes, "git", args...)
	return err
}

func parseWorktrees(raw string) []GitWorktree {
	blocks := strings.Split(strings.TrimSpace(raw), "\n\n")
	out := make([]GitWorktree, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var item GitWorktree
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, " ")
			switch key {
			case "worktree":
				item.Path = filepath.Clean(strings.TrimSpace(value))
			case "HEAD":
				item.Head = strings.TrimSpace(value)
			case "branch":
				item.Branch = strings.TrimPrefix(strings.TrimSpace(value), "refs/heads/")
			case "detached":
				item.Detached = true
			case "locked":
				item.Locked = true
			case "prunable":
				item.Prunable = true
			}
		}
		if item.Path != "" {
			out = append(out, item)
		}
	}
	return out
}
