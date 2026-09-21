package repository

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultSearchResults = 200
	MaxSearchResults     = 1000
	MaxSearchBytes       = 512 << 10
	MaxDiffBytes         = 1 << 20
	MaxStatusBytes       = 512 << 10
	MaxFallbackFileBytes = 1 << 20
	MaxWalkFiles         = 20000
)

type Service struct {
	workspace string
}

type SearchMatch struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column,omitempty"`
	Text   string `json:"text"`
}

type SearchResult struct {
	Matches   []SearchMatch `json:"matches"`
	Truncated bool          `json:"truncated"`
	Engine    string        `json:"engine"`
}

type FileResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
}

type GitFileStatus struct {
	Path     string `json:"path"`
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
}

type GitStatus struct {
	Files []GitFileStatus `json:"files"`
}

type GitDiff struct {
	Path      string `json:"path,omitempty"`
	Content   string `json:"content"`
	Bytes     int    `json:"bytes"`
	Truncated bool   `json:"truncated"`
	Staged    bool   `json:"staged"`
}

type GitActionResult struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

func New(workspace string) (*Service, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, errors.New("workspace is required")
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	if info, err := os.Stat(abs); err != nil || !info.IsDir() {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("workspace is not a directory")
	}
	return &Service{workspace: abs}, nil
}

func (s *Service) SearchText(ctx context.Context, query, relative string, limit int) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchResult{}, errors.New("search query is required")
	}
	limit = normalizeLimit(limit)
	target, err := s.resolve(relative)
	if err != nil {
		return SearchResult{}, err
	}
	if rg, err := exec.LookPath("rg"); err == nil {
		args := []string{"--line-number", "--column", "--no-heading", "--color", "never", "--fixed-strings", "--", query, target}
		raw, truncated, runErr := runBounded(ctx, s.workspace, MaxSearchBytes, rg, args...)
		if runErr != nil {
			var exitErr *exec.ExitError
			if !(errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1) {
				return SearchResult{}, runErr
			}
		}
		matches := parseRG(raw, s.workspace, limit)
		return SearchResult{Matches: matches, Truncated: truncated || len(matches) >= limit, Engine: "rg"}, nil
	}
	return s.searchFallback(ctx, query, target, limit)
}

func (s *Service) FindFiles(ctx context.Context, pattern, relative string, limit int) (FileResult, error) {
	limit = normalizeLimit(limit)
	root, err := s.resolve(relative)
	if err != nil {
		return FileResult{}, err
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = "*"
	}
	out := make([]string, 0, limit)
	visited := 0
	truncated := false
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "dist" || entry.Name() == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		visited++
		if visited > MaxWalkFiles {
			truncated = true
			return io.EOF
		}
		rel, err := filepath.Rel(s.workspace, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		matched, matchErr := filepath.Match(pattern, entry.Name())
		if matchErr != nil {
			return matchErr
		}
		if !matched && strings.Contains(pattern, "/") {
			matched, matchErr = filepath.Match(pattern, rel)
			if matchErr != nil {
				return matchErr
			}
		}
		if matched {
			out = append(out, rel)
			if len(out) >= limit {
				truncated = true
				return io.EOF
			}
		}
		return nil
	})
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return FileResult{Paths: out, Truncated: truncated}, err
}

func (s *Service) GitStatus(ctx context.Context) (GitStatus, error) {
	raw, _, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return GitStatus{}, err
	}
	parts := bytes.Split(raw, []byte{0})
	files := make([]GitFileStatus, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		item := parts[i]
		if len(item) < 4 {
			continue
		}
		index, worktree := string(item[0]), string(item[1])
		path := strings.TrimSpace(string(item[3:]))
		if (index == "R" || index == "C") && i+1 < len(parts) {
			i++
			path = strings.TrimSpace(string(parts[i]))
		}
		if path != "" {
			files = append(files, GitFileStatus{Path: filepath.ToSlash(path), Index: index, Worktree: worktree})
		}
	}
	return GitStatus{Files: files}, nil
}

func (s *Service) GitDiff(ctx context.Context, relative string, staged bool) (GitDiff, error) {
	args := []string{"diff", "--no-ext-diff", "--no-color", "--unified=3"}
	if staged {
		args = append(args, "--cached")
	}
	relative = strings.TrimSpace(relative)
	if relative != "" {
		resolved, err := s.resolve(relative)
		if err != nil {
			return GitDiff{}, err
		}
		rel, _ := filepath.Rel(s.workspace, resolved)
		relative = filepath.ToSlash(rel)
		args = append(args, "--", relative)
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxDiffBytes, "git", args...)
	if err != nil {
		return GitDiff{}, err
	}
	return GitDiff{
		Path: relative, Content: string(raw), Bytes: len(raw), Truncated: truncated, Staged: staged,
	}, nil
}

func (s *Service) GitStage(ctx context.Context, relative string) (GitActionResult, error) {
	path, err := s.gitPath(relative)
	if err != nil {
		return GitActionResult{}, err
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "add", "--", path)
	return GitActionResult{Output: string(raw), Truncated: truncated}, err
}

func (s *Service) GitUnstage(ctx context.Context, relative string) (GitActionResult, error) {
	path, err := s.gitPath(relative)
	if err != nil {
		return GitActionResult{}, err
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "restore", "--staged", "--", path)
	return GitActionResult{Output: string(raw), Truncated: truncated}, err
}

func (s *Service) GitRevert(ctx context.Context, relative string) (GitActionResult, error) {
	path, err := s.gitPath(relative)
	if err != nil {
		return GitActionResult{}, err
	}
	status, err := s.GitStatus(ctx)
	if err != nil {
		return GitActionResult{}, err
	}
	untracked := false
	for _, item := range status.Files {
		if item.Path == path && item.Index == "?" && item.Worktree == "?" {
			untracked = true
			break
		}
	}
	if untracked {
		absolute, err := s.resolve(path)
		if err != nil {
			return GitActionResult{}, err
		}
		if err := os.Remove(absolute); err != nil {
			return GitActionResult{}, err
		}
		return GitActionResult{Output: "removed untracked file " + path}, nil
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "restore", "--worktree", "--", path)
	return GitActionResult{Output: string(raw), Truncated: truncated}, err
}

func (s *Service) GitCommit(ctx context.Context, message string) (GitActionResult, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return GitActionResult{}, errors.New("commit message is required")
	}
	raw, truncated, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "commit", "-m", message)
	return GitActionResult{Output: string(raw), Truncated: truncated}, err
}

func (s *Service) GitPush(ctx context.Context) (GitActionResult, error) {
	raw, truncated, err := runBounded(ctx, s.workspace, MaxStatusBytes, "git", "push")
	return GitActionResult{Output: string(raw), Truncated: truncated}, err
}

func (s *Service) gitPath(relative string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" {
		return "", errors.New("git path is required")
	}
	absolute, err := s.resolve(relative)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(s.workspace, absolute)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func (s *Service) resolve(relative string) (string, error) {
	relative = strings.TrimSpace(relative)
	if relative == "" || relative == "." {
		return s.workspace, nil
	}
	path := relative
	if !filepath.IsAbs(path) {
		path = filepath.Join(s.workspace, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(s.workspace, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path escapes workspace: %s", relative)
	}
	return path, nil
}

func normalizeLimit(limit int) int {
	if limit <= 0 {
		return DefaultSearchResults
	}
	if limit > MaxSearchResults {
		return MaxSearchResults
	}
	return limit
}

func parseRG(raw []byte, workspace string, limit int) []SearchMatch {
	lines := strings.Split(string(raw), "\n")
	out := make([]SearchMatch, 0, min(limit, len(lines)))
	for _, line := range lines {
		if line == "" || len(out) >= limit {
			break
		}
		parts := strings.SplitN(line, ":", 4)
		if len(parts) != 4 {
			continue
		}
		lineNo, err1 := strconv.Atoi(parts[1])
		column, err2 := strconv.Atoi(parts[2])
		if err1 != nil || err2 != nil {
			continue
		}
		path := parts[0]
		if filepath.IsAbs(path) {
			if rel, err := filepath.Rel(workspace, path); err == nil {
				path = rel
			}
		}
		out = append(out, SearchMatch{Path: filepath.ToSlash(path), Line: lineNo, Column: column, Text: parts[3]})
	}
	return out
}

func (s *Service) searchFallback(ctx context.Context, query, root string, limit int) (SearchResult, error) {
	out := make([]SearchMatch, 0, limit)
	truncated := false
	visited := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "dist" || entry.Name() == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		visited++
		if visited > MaxWalkFiles {
			truncated = true
			return io.EOF
		}
		info, err := entry.Info()
		if err != nil || info.Size() > MaxFallbackFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			column := strings.Index(line, query)
			if column < 0 {
				continue
			}
			rel, _ := filepath.Rel(s.workspace, path)
			out = append(out, SearchMatch{Path: filepath.ToSlash(rel), Line: i + 1, Column: column + 1, Text: line})
			if len(out) >= limit {
				truncated = true
				return io.EOF
			}
		}
		return nil
	})
	if errors.Is(err, io.EOF) {
		err = nil
	}
	return SearchResult{Matches: out, Truncated: truncated, Engine: "go"}, err
}

type cappedWriter struct {
	buf bytes.Buffer
	max int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	original := len(p)
	remaining := w.max - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = w.buf.Write(p)
	}
	return original, nil
}

func runBounded(ctx context.Context, dir string, maxBytes int, name string, args ...string) ([]byte, bool, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	stderr := &cappedWriter{max: 64 << 10}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, int64(maxBytes)+1))
	truncated := len(data) > maxBytes
	if truncated {
		data = data[:maxBytes]
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if readErr != nil {
		return nil, truncated, readErr
	}
	if truncated {
		waitErr = nil
	}
	if ctx.Err() != nil {
		return nil, truncated, ctx.Err()
	}
	if waitErr != nil {
		message := strings.TrimSpace(stderr.buf.String())
		if message != "" {
			return data, truncated, fmt.Errorf("%w: %s", waitErr, message)
		}
		return data, truncated, waitErr
	}
	return data, truncated, nil
}
