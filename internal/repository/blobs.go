package repository

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// FileBlobs returns, for each workspace-relative path, the git blob id its current content would have
// in this workspace (`git hash-object`, which applies the repository's clean filters), or "" when the
// path is not a regular file inside the workspace. One git process hashes every file. It is how note
// cites record and later compare a file's state (ADR-0010 D1, D4); it never stores content.
func (s *Service) FileBlobs(ctx context.Context, paths []string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	var files []string
	for _, p := range paths {
		out[p] = ""
		abs, err := s.resolve(p)
		if err != nil || abs == s.workspace {
			continue
		}
		info, err := os.Lstat(abs)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		files = append(files, p)
	}
	if len(files) == 0 {
		return out, nil
	}
	args := append([]string{"hash-object", "--"}, files...)
	raw, _, err := runBounded(ctx, s.workspace, 128*len(files)+256, "git", args...)
	if err != nil {
		return nil, fmt.Errorf("hash cited files: %w", err)
	}
	ids := strings.Fields(string(raw))
	if len(ids) != len(files) {
		return nil, fmt.Errorf("git hash-object returned %d ids for %d files", len(ids), len(files))
	}
	for i, p := range files {
		out[p] = ids[i]
	}
	return out, nil
}
