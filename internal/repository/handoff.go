package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const MaxHandoffBytes = 512 << 10

func (s *Service) GitHead(ctx context.Context) (string, error) {
	raw, _, err := runBounded(ctx, s.workspace, 8<<10, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (s *Service) GitCurrentBranch(ctx context.Context) (string, error) {
	raw, _, err := runBounded(ctx, s.workspace, 8<<10, "git", "branch", "--show-current")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

func (s *Service) GitCommitsBetween(ctx context.Context, base, head string) ([]string, error) {
	base = strings.TrimSpace(base)
	head = strings.TrimSpace(head)
	if base == "" || head == "" {
		return nil, errors.New("base and head commits are required")
	}
	if _, _, err := runBounded(ctx, s.workspace, 8<<10, "git", "rev-parse", "--verify", base+"^{commit}"); err != nil {
		return nil, err
	}
	if _, _, err := runBounded(ctx, s.workspace, 8<<10, "git", "rev-parse", "--verify", head+"^{commit}"); err != nil {
		return nil, err
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxHandoffBytes, "git", "rev-list", "--reverse", base+".."+head)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("handoff commit list exceeded runtime limit")
	}
	return nonEmptyLines(string(raw)), nil
}

// GitUnappliedCommits lists the commits of source whose patch is not in target
// (git cherry). A non-empty base limits the comparison to commits after base.
func (s *Service) GitUnappliedCommits(ctx context.Context, target, source, base string) ([]string, error) {
	target = strings.TrimSpace(target)
	source = strings.TrimSpace(source)
	base = strings.TrimSpace(base)
	if target == "" || source == "" {
		return nil, errors.New("target and source commits are required")
	}
	args := []string{"cherry", target, source}
	if base != "" {
		args = append(args, base)
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxHandoffBytes, "git", args...)
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("handoff cherry comparison exceeded runtime limit")
	}
	out := make([]string, 0)
	for _, line := range nonEmptyLines(string(raw)) {
		if strings.HasPrefix(line, "+ ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "+ ")))
		}
	}
	return out, nil
}

func (s *Service) GitChangedFilesBetween(ctx context.Context, base, head string) ([]string, error) {
	base = strings.TrimSpace(base)
	head = strings.TrimSpace(head)
	if base == "" || head == "" {
		return nil, errors.New("base and head commits are required")
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxHandoffBytes, "git", "diff", "--name-only", "--no-renames", base+".."+head, "--")
	if err != nil {
		return nil, err
	}
	if truncated {
		return nil, errors.New("handoff changed-file list exceeded runtime limit")
	}
	return nonEmptyLines(string(raw)), nil
}

// GitCherryPick applies commits to the checkout. A failed pick is aborted, which is
// safe only for a sequence this call started: an operation already in progress
// (another process's cherry-pick, revert, merge or rebase) is refused untouched.
func (s *Service) GitCherryPick(ctx context.Context, commits []string) (GitActionResult, error) {
	if len(commits) == 0 {
		return GitActionResult{}, errors.New("at least one commit is required")
	}
	if op, err := s.operationInProgress(ctx); err != nil {
		return GitActionResult{}, err
	} else if op != "" {
		return GitActionResult{}, fmt.Errorf("a git %s is in progress in the checkout", op)
	}
	args := append([]string{"cherry-pick"}, commits...)
	raw, truncated, err := runBounded(ctx, s.workspace, MaxHandoffBytes, "git", args...)
	if err != nil {
		_, _, _ = runBounded(context.Background(), s.workspace, 64<<10, "git", "cherry-pick", "--abort")
		return GitActionResult{Output: string(raw), Truncated: truncated}, err
	}
	return GitActionResult{Output: string(raw), Truncated: truncated}, nil
}

// operationInProgress names the git operation in progress in the checkout, or "".
func (s *Service) operationInProgress(ctx context.Context) (string, error) {
	markers := []struct{ path, op string }{
		{"sequencer", "cherry-pick or revert"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
		{"MERGE_HEAD", "merge"},
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase"},
	}
	args := []string{"rev-parse"}
	for _, marker := range markers {
		args = append(args, "--git-path", marker.path)
	}
	raw, _, err := runBounded(ctx, s.workspace, 16<<10, "git", args...)
	if err != nil {
		return "", err
	}
	paths := nonEmptyLines(string(raw))
	if len(paths) != len(markers) {
		return "", fmt.Errorf("git rev-parse --git-path returned %d paths, want %d", len(paths), len(markers))
	}
	for i, path := range paths {
		if !filepath.IsAbs(path) {
			path = filepath.Join(s.workspace, path)
		}
		if _, err := os.Lstat(path); err == nil {
			return markers[i].op, nil
		}
	}
	return "", nil
}

func nonEmptyLines(raw string) []string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}
