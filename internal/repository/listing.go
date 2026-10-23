package repository

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// MaxListEntries bounds one directory listing. A directory with more entries is cut at that
// count and the listing reports Truncated, so a caller can render a partial tree and say so.
const MaxListEntries = 2000

// DirEntry kinds. A symlink is reported as a symlink whatever it points at: the listing never
// follows an entry's link, so browsing cannot be led out of the workspace one level at a time.
const (
	DirEntryDir     = "dir"
	DirEntryFile    = "file"
	DirEntrySymlink = "symlink"
)

type DirEntry struct {
	Name string `json:"name"`
	// Path is the entry's workspace-relative path with forward slashes, the form every other
	// repository call accepts.
	Path string `json:"path"`
	// Kind is dir, file or symlink.
	Kind string `json:"kind"`
	Size int64  `json:"size,omitempty"`
}

type DirListing struct {
	// Path is the listed directory's workspace-relative path; "" is the workspace root.
	Path      string     `json:"path"`
	Entries   []DirEntry `json:"entries"`
	Truncated bool       `json:"truncated"`
}

// ListDir lists one directory of the workspace: directories first, then files and symlinks by
// case-insensitive name. The `.git` directory is omitted; every other entry, dotfiles included,
// is reported. The directory itself must resolve inside the workspace (a symlinked directory on
// the way out is refused), and entries are never followed, so one call cannot escape the root.
func (s *Service) ListDir(ctx context.Context, relative string) (DirListing, error) {
	if err := ctx.Err(); err != nil {
		return DirListing{}, err
	}
	abs, err := s.resolve(relative)
	if err != nil {
		return DirListing{}, err
	}
	realRoot, err := filepath.EvalSymlinks(s.workspace)
	if err != nil {
		return DirListing{}, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return DirListing{}, err
	}
	if !insideReal(realRoot, real) {
		return DirListing{}, fmt.Errorf("path escapes workspace through symlink: %s", relative)
	}
	info, err := os.Stat(real)
	if err != nil {
		return DirListing{}, err
	}
	if !info.IsDir() {
		return DirListing{}, fmt.Errorf("%s is not a directory", relative)
	}
	lexical, err := filepath.Rel(s.workspace, abs)
	if err != nil || filepath.IsAbs(lexical) {
		return DirListing{}, fmt.Errorf("path escapes workspace: %s", relative)
	}
	if lexical == "." {
		lexical = ""
	}
	lexical = filepath.ToSlash(lexical)

	entries, err := os.ReadDir(real)
	if err != nil {
		return DirListing{}, err
	}
	out := DirListing{Path: lexical, Entries: make([]DirEntry, 0, min(len(entries), MaxListEntries))}
	for index, entry := range entries {
		if index%256 == 0 {
			if err := ctx.Err(); err != nil {
				return DirListing{}, err
			}
		}
		if entry.Name() == ".git" && entry.IsDir() {
			continue
		}
		if len(out.Entries) == MaxListEntries {
			out.Truncated = true
			break
		}
		kind := DirEntryFile
		switch {
		case entry.IsDir():
			kind = DirEntryDir
		case entry.Type()&os.ModeSymlink != 0:
			kind = DirEntrySymlink
		}
		item := DirEntry{Name: entry.Name(), Kind: kind, Path: entry.Name()}
		if lexical != "" {
			item.Path = lexical + "/" + entry.Name()
		}
		if info, err := entry.Info(); err == nil {
			item.Size = info.Size()
		}
		out.Entries = append(out.Entries, item)
	}
	sort.SliceStable(out.Entries, func(i, j int) bool {
		left, right := out.Entries[i], out.Entries[j]
		if (left.Kind == DirEntryDir) != (right.Kind == DirEntryDir) {
			return left.Kind == DirEntryDir
		}
		lowerLeft, lowerRight := strings.ToLower(left.Name), strings.ToLower(right.Name)
		if lowerLeft != lowerRight {
			return lowerLeft < lowerRight
		}
		return left.Name < right.Name
	})
	return out, nil
}
