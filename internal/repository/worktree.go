package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
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

// WorktreeStatus is the uncommitted state of a working tree:
//   - Files: the dirty paths (tracked and untracked, not ignored);
//   - Ignored: the ignored entries, a fully ignored directory as one "dir/" entry,
//     which `git worktree remove` deletes although git reports the tree clean;
//   - Digest: covers both git listings plus the size, mode and mtime of every listed
//     path, so it changes when a listed file is edited, not only when the list does.
type WorktreeStatus struct {
	Files   []string `json:"files"`
	Ignored []string `json:"ignored"`
	// IgnoredTruncated: git listed more ignored entries than MaxStatusBytes holds;
	// Ignored is a prefix.
	IgnoredTruncated bool   `json:"ignoredTruncated,omitempty"`
	Digest           string `json:"digest"`
}

// GitWorktreeStatus reads the status of the service's working tree. A dirty listing
// larger than MaxStatusBytes is an error: a partial list cannot prove what would be
// lost. An over-long ignored listing is kept as a prefix; any ignored entry already
// stops the safe default.
func (s *Service) GitWorktreeStatus(ctx context.Context) (WorktreeStatus, error) {
	raw, truncated, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return WorktreeStatus{}, err
	}
	if truncated {
		return WorktreeStatus{}, errors.New("worktree status exceeded runtime limit")
	}
	// Traditional mode with normal untracked listing collapses an ignored directory
	// into one entry, so node_modules stays one line.
	ignoredRaw, ignoredTruncated, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "status", "--porcelain=v1", "-z", "--ignored=traditional", "--untracked-files=normal")
	if err != nil {
		return WorktreeStatus{}, err
	}
	if ignoredTruncated {
		// Drop the entry the limit cut in half.
		if i := bytes.LastIndexByte(ignoredRaw, 0); i >= 0 {
			ignoredRaw = ignoredRaw[:i+1]
		} else {
			ignoredRaw = nil
		}
	}
	out := WorktreeStatus{Files: []string{}, Ignored: []string{}, IgnoredTruncated: ignoredTruncated}
	for _, file := range parseStatus(raw) {
		out.Files = append(out.Files, file.Path)
	}
	for _, file := range parseStatus(ignoredRaw) {
		if file.Index == "!" {
			out.Ignored = append(out.Ignored, file.Path)
		}
	}
	hash := sha256.New()
	hash.Write(raw)
	hash.Write([]byte{0, 0})
	hash.Write(ignoredRaw)
	for _, path := range append(append([]string(nil), out.Files...), out.Ignored...) {
		fmt.Fprintf(hash, "\x00%s\x00", path)
		if info, err := os.Lstat(filepath.Join(s.workspace, filepath.FromSlash(strings.TrimSuffix(path, "/")))); err == nil {
			fmt.Fprintf(hash, "%d:%o:%d", info.Size(), info.Mode(), info.ModTime().UnixNano())
		} else {
			hash.Write([]byte("absent"))
		}
	}
	out.Digest = "sha256:" + hex.EncodeToString(hash.Sum(nil))
	return out, nil
}

// GitBranchHead returns the commit of refs/heads/<branch>, or "" when the branch does not exist.
func (s *Service) GitBranchHead(ctx context.Context, branch string) (string, error) {
	if err := s.checkBranchName(ctx, branch); err != nil {
		return "", err
	}
	raw, _, err := runBounded(ctx, s.workspace, 4<<10, "git", "for-each-ref", "--format=%(objectname)", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(raw)), nil
}

// GitDeleteBranch deletes a local branch whatever its merge state; callers decide
// beforehand that nothing on it would be lost.
func (s *Service) GitDeleteBranch(ctx context.Context, branch string) error {
	if err := s.checkBranchName(ctx, branch); err != nil {
		return err
	}
	_, _, err := runBounded(ctx, s.workspace, MaxWorktreeOutputBytes, "git", "branch", "-D", branch)
	return err
}

// GitPruneWorktrees drops the registrations of worktrees whose directories are gone.
func (s *Service) GitPruneWorktrees(ctx context.Context) error {
	_, _, err := runBounded(ctx, s.workspace, MaxWorktreeOutputBytes, "git", "worktree", "prune")
	return err
}

func (s *Service) checkBranchName(ctx context.Context, branch string) error {
	if strings.TrimSpace(branch) == "" || strings.HasPrefix(branch, "-") {
		return errors.New("invalid branch name")
	}
	_, _, err := runBounded(ctx, s.workspace, 16<<10, "git", "check-ref-format", "--branch", branch)
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
