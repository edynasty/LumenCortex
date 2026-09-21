package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

func (s *Service) EnsureLocalExclude(ctx context.Context, target string) error {
	root, err := s.GitRoot(ctx)
	if err != nil {
		return err
	}
	target, err = filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return nil
	}

	raw, _, err := runBounded(ctx, s.workspace, 8<<10, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		raw, _, err = runBounded(ctx, s.workspace, 8<<10, "git", "rev-parse", "--git-common-dir")
		if err != nil {
			return err
		}
	}
	commonDir := strings.TrimSpace(string(raw))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(root, commonDir)
	}
	excludePath := filepath.Join(filepath.Clean(commonDir), "info", "exclude")
	if err := os.MkdirAll(filepath.Dir(excludePath), 0o755); err != nil {
		return err
	}

	pattern := "/" + strings.TrimPrefix(rel, "/")
	if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
		pattern += "/"
	}

	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == pattern {
			return nil
		}
	}

	content := string(existing)
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	content += pattern + "\n"
	return os.WriteFile(excludePath, []byte(content), 0o644)
}
