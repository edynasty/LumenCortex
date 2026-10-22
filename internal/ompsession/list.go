package ompsession

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ErrOutsideRoot reports a path that is not a top-level session file of the sessions root.
var ErrOutsideRoot = errors.New("not a top-level session file of the OMP sessions directory")

// Summary describes one top-level session file from its header, without reading its entries.
type Summary struct {
	Path      string
	ID        string
	Title     string
	Cwd       string
	CreatedAt time.Time
	ModTime   time.Time
	Size      int64
	// Children counts the subagent transcripts directly beside the session.
	Children int
}

// List returns the top-level sessions under root (<root>/<project>/<file>.jsonl), newest first.
// Files without a session header are skipped.
func List(root string) ([]Summary, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []Summary
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(root, project.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			name := f.Name()
			if !f.Type().IsRegular() || !strings.HasSuffix(name, ".jsonl") || strings.HasPrefix(name, ".") {
				continue
			}
			path := filepath.Join(dir, name)
			info, err := f.Info()
			if err != nil {
				continue
			}
			header, err := ReadHeader(path)
			if err != nil {
				continue
			}
			out = append(out, Summary{
				Path: path, ID: header.ID, Title: header.Title, Cwd: header.Cwd,
				CreatedAt: parseTime(header.Timestamp, info.ModTime()), ModTime: info.ModTime().UTC(),
				Size: info.Size(), Children: directChildren(strings.TrimSuffix(path, ".jsonl")),
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

func directChildren(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Type().IsRegular() && isChildTranscript(e.Name()) {
			n++
		}
	}
	return n
}

// ResolvePath returns the real path of a top-level session file of root, refusing anything else:
// a path outside root after symlinks are resolved, a subagent transcript, or a non-regular file.
func ResolvePath(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 2 || parts[0] == ".." || parts[0] == "." || !strings.HasSuffix(parts[1], ".jsonl") || strings.HasPrefix(parts[1], ".") {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	info, err := os.Stat(real)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	return real, nil
}
