package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// MaxBlobFileBytes bounds a file FileBlobs hashes, so a cite of a large build output or database is not
// re-read by every recall that returns it.
const MaxBlobFileBytes = 4 << 20

// FileBlobs returns, for each workspace-relative path, the git blob id its current content would have
// in this workspace (`git hash-object`, which applies the repository's clean filters), or "" when the
// path is not a regular file of at most MaxBlobFileBytes whose real location, symlinks resolved, is
// inside the workspace, or when it could not be hashed. It is how note cites record and later compare
// a file's state (ADR-0010 D1, D4): "" refuses a cite and marks a recorded one stale. It never stores
// content. One git process hashes every file; when it fails (a file removed or unreadable in between)
// each file is hashed alone, so one bad file never fails the others.
func (s *Service) FileBlobs(ctx context.Context, paths []string) (map[string]string, error) {
	out := make(map[string]string, len(paths))
	realWorkspace, err := filepath.EvalSymlinks(s.workspace)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, p := range paths {
		out[p] = ""
		if s.hashable(realWorkspace, p) {
			files = append(files, p)
		}
	}
	if len(files) == 0 {
		return out, nil
	}
	if ids, ok := s.hashObjects(ctx, files); ok {
		for i, p := range files {
			out[p] = ids[i]
		}
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, p := range files {
		if ids, ok := s.hashObjects(ctx, []string{p}); ok {
			out[p] = ids[0]
		}
	}
	return out, ctx.Err()
}

func (s *Service) hashable(realWorkspace, p string) bool {
	abs, err := s.resolve(p)
	if err != nil || abs == s.workspace {
		return false
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Size() > MaxBlobFileBytes {
		return false
	}
	// Lstat sees only the last element; a symlinked directory on the way may lead outside.
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(realWorkspace, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func (s *Service) hashObjects(ctx context.Context, files []string) ([]string, bool) {
	args := append([]string{"hash-object", "--"}, files...)
	raw, _, err := runBounded(ctx, s.workspace, 128*len(files)+256, "git", args...)
	if err != nil {
		return nil, false
	}
	ids := strings.Fields(string(raw))
	return ids, len(ids) == len(files)
}
