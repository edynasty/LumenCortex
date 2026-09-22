package repository

import (
	"context"
	"errors"
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

func (s *Service) GitCherryPick(ctx context.Context, commits []string) (GitActionResult, error) {
	if len(commits) == 0 {
		return GitActionResult{}, errors.New("at least one commit is required")
	}
	args := append([]string{"cherry-pick"}, commits...)
	raw, truncated, err := runBounded(ctx, s.workspace, MaxHandoffBytes, "git", args...)
	if err != nil {
		_, _, _ = runBounded(context.Background(), s.workspace, 64<<10, "git", "cherry-pick", "--abort")
		return GitActionResult{Output: string(raw), Truncated: truncated}, err
	}
	return GitActionResult{Output: string(raw), Truncated: truncated}, nil
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
