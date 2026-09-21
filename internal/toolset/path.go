package toolset

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (s *Set) resolve(input string, allowMissing bool) (string, string, error) {
	if strings.TrimSpace(input) == "" {
		input = "."
	}
	candidate := input
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(s.workspace, candidate)
	}
	candidate = filepath.Clean(candidate)
	rel, err := filepath.Rel(s.workspace, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes workspace: %s", input)
	}
	check := candidate
	if allowMissing {
		for {
			_, err := os.Lstat(check)
			if err == nil {
				break
			}
			if !os.IsNotExist(err) {
				return "", "", err
			}
			parent := filepath.Dir(check)
			if parent == check {
				return "", "", fmt.Errorf("cannot resolve parent for %s", input)
			}
			check = parent
		}
	}
	real, err := filepath.EvalSymlinks(check)
	if err != nil {
		return "", "", err
	}
	realRel, err := filepath.Rel(s.realWorkspace, real)
	if err != nil || realRel == ".." || strings.HasPrefix(realRel, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("path escapes workspace through symlink: %s", input)
	}
	return candidate, filepath.ToSlash(rel), nil
}
