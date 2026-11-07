package toolset

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"github.com/edynasty/LumenCortex/internal/safefs"
	"github.com/edynasty/LumenCortex/protocol"
)

// ReadArtifact uses the same path and protected-state checks as read_file, without
// interpreting binary bytes as UTF-8. The host imposes the transfer limit before reading.
func (s *Set) ReadArtifact(ctx context.Context, path string, limit int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.policyAllows(protocol.ToolSpec{Permission: "read"}) {
		return nil, errors.New("artifact read denied")
	}
	if limit <= 0 || limit > 4<<20 {
		limit = 4 << 20
	}
	file, rel, err := s.resolve(path, false)
	if err != nil {
		return nil, err
	}
	if testHookBeforeOpen != nil {
		testHookBeforeOpen(file)
	}
	root, err := safefs.Open(s.realWorkspace)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, info, err := root.OpenRegular(rel, limit)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := s.denyIfProtected(info); err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("artifact source must be a regular file")
	}
	if info.Size() > limit {
		return nil, errors.New("artifact source exceeds transfer limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("artifact grew beyond transfer limit")
	}
	return data, nil
}

// WriteArtifact shares write_file's authority and symlink/protected-state boundary.
func (s *Set) WriteArtifact(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.policyAllows(protocol.ToolSpec{Permission: "write"}) {
		return errors.New("artifact write denied")
	}
	if len(data) > maxEditBytes {
		return errors.New("artifact exceeds 4 MiB workspace transfer limit")
	}
	file, rel, err := s.resolve(path, true)
	if err != nil {
		return err
	}
	if testHookBeforeOpen != nil {
		testHookBeforeOpen(file)
	}
	root, err := safefs.Open(s.realWorkspace)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := root.MkdirAll(filepath.ToSlash(filepath.Dir(rel))); err != nil {
		return err
	}
	return root.WriteFile(rel, data, 0644)
}
