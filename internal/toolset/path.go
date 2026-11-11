package toolset

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrProtectedPath reports that a file tool was pointed at LumenCortex's own state (ADR-0016).
var ErrProtectedPath = errors.New("path is LumenCortex state and is not reachable by file tools")

// Protected names the state that read_file, list_dir, write_file, replace_in_file and apply_patch
// must never reach, by any path that leads to it: absolute or relative, through .. or a symlink, as
// a hard link, or by a path swapped in between the check and the open.
//
// This is a guard on the file tools only. The shell runs with the user's own permissions and can open
// the same files; no OS-level isolation is provided here.
type Protected struct {
	// Trees are directories whose whole content is protected: the external state of the workspace.
	Trees []string
	// Legacy is a directory in which only the state files an earlier build keeps are protected
	// (legacyStateGlobs); its other files, such as mcp.json and skills/, are project configuration.
	Legacy string
}

// legacyStateGlobs are the names, matched case-insensitively against the first component under
// Protected.Legacy, that hold or guard state: the database and its sidecars, the locks, the run
// locks, the backups, the checkpoint marker, and the copy a migration leaves.
var legacyStateGlobs = []string{"lumencortex.db*", "db.lock", "bind.lock", "runs", "backups", "checkpoint.pending*", "legacy-state-*", "eval", "eval-workspaces"}

// maxProtectedEntries bounds the walk that collects the files whose identity is compared. State
// directories hold a few dozen files; more than this is refused rather than guessed at.
const maxProtectedEntries = 4096

func (p Protected) empty() bool { return len(p.Trees) == 0 && p.Legacy == "" }

// testHookBeforeOpen runs between a path being validated and the file being opened, so a test can
// swap the path as an attacker would.
var testHookBeforeOpen func(path string)

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
	// The part of the path that does not exist yet is appended to the resolved part, so the guard
	// sees where a new file would really land.
	missing, err := filepath.Rel(check, candidate)
	if err != nil {
		return "", "", err
	}
	if err := s.guard(real, filepath.Join(real, missing)); err != nil {
		return "", "", fmt.Errorf("%w: %s", err, input)
	}
	return candidate, filepath.ToSlash(rel), nil
}

// guard refuses a path that leads to protected state. anchor is the deepest existing part of the
// path with symlinks resolved; full is anchor plus the components that do not exist yet. Names are
// compared first, without regard to case; identity (device and inode, or the Windows file index) is
// compared after, for the anchor and every directory above it in the workspace, which also catches a
// hard link, a case or Unicode spelling the file system folds, and a path through a protected
// directory whose contents are not listed (runs).
func (s *Set) guard(anchor, full string) error {
	if s.protected.empty() {
		return nil
	}
	if s.protectedByName(full) {
		return ErrProtectedPath
	}
	entries, err := s.protected.entries()
	if err != nil {
		return err
	}
	for dir := anchor; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			return err
		}
		if sameAsAny(info, entries) {
			return ErrProtectedPath
		}
		if !within(s.realWorkspace, dir) || dir == s.realWorkspace || filepath.Dir(dir) == dir {
			return nil
		}
	}
}

// protectedByName reports whether the resolved path lies in a protected tree or is a state file
// directly under the legacy directory. Paths are compared without regard to case: on a file system
// that folds case another spelling names the same file, and a spelling that does not exist yet has
// no identity to compare. On one that does not, a case variant is refused needlessly, never allowed.
func (s *Set) protectedByName(full string) bool {
	full = strings.ToLower(full)
	for _, tree := range s.protected.Trees {
		if within(strings.ToLower(realOf(tree)), full) {
			return true
		}
	}
	if s.protected.Legacy == "" {
		return false
	}
	rel, ok := relWithin(strings.ToLower(realOf(s.protected.Legacy)), full)
	if !ok || rel == "." {
		return false
	}
	first := strings.SplitN(rel, string(filepath.Separator), 2)[0]
	for _, glob := range legacyStateGlobs {
		if matched, _ := filepath.Match(glob, first); matched {
			return true
		}
	}
	return false
}

// denyIfProtected returns ErrProtectedPath when info is a file or directory of the protected state.
func (s *Set) denyIfProtected(info os.FileInfo) error {
	if s.protected.empty() {
		return nil
	}
	entries, err := s.protected.entries()
	if err != nil {
		return err
	}
	if sameAsAny(info, entries) {
		return ErrProtectedPath
	}
	return nil
}

func sameAsAny(info os.FileInfo, entries []os.FileInfo) bool {
	for _, entry := range entries {
		if os.SameFile(info, entry) {
			return true
		}
	}
	return false
}

// lockDirName is the directory of run and lease lock files. It gains a file per session and is never
// pruned, so its files are not listed: they are empty, and a path into the directory is refused by the
// directory's own identity (guard) and name.
const lockDirName = "runs"

// entries lists every protected file and directory that exists now, except the files inside a runs
// directory. Entries that vanish during the walk are skipped: they cannot be reached either.
func (p Protected) entries() ([]os.FileInfo, error) {
	var out []os.FileInfo
	walk := func(root string) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if len(out) >= maxProtectedEntries {
				return fmt.Errorf("protected state holds more than %d entries, so file tools cannot verify it", maxProtectedEntries)
			}
			if info, err := d.Info(); err == nil {
				out = append(out, info)
			}
			if d.IsDir() && strings.EqualFold(d.Name(), lockDirName) && (path == root || filepath.Dir(path) == root) {
				return filepath.SkipDir
			}
			return nil
		})
	}
	for _, tree := range p.Trees {
		if err := walk(realOf(tree)); err != nil {
			return nil, err
		}
	}
	if p.Legacy != "" {
		legacy := realOf(p.Legacy)
		children, err := os.ReadDir(legacy)
		if err == nil {
			for _, child := range children {
				if matchesLegacy(child.Name()) {
					if err := walk(filepath.Join(legacy, child.Name())); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return out, nil
}

func matchesLegacy(name string) bool {
	name = strings.ToLower(name)
	for _, glob := range legacyStateGlobs {
		if matched, _ := filepath.Match(glob, name); matched {
			return true
		}
	}
	return false
}

// realOf resolves symlinks in the part of path that exists and keeps the rest as written.
func realOf(path string) string {
	path = filepath.Clean(path)
	existing, rest := path, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return path
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return path
	}
	return filepath.Join(real, rest)
}

// relWithin returns target relative to root and whether target is root or inside it.
func relWithin(root, target string) (string, bool) {
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func within(root, target string) bool {
	_, ok := relWithin(root, target)
	return ok
}

// openRead opens a file for reading and refuses it, after opening, when it is protected state: the
// check is on the open descriptor, so a path swapped after resolve, a hard link and an alias all
// end here.
func (s *Set) openRead(path string) (*os.File, os.FileInfo, error) {
	if testHookBeforeOpen != nil {
		testHookBeforeOpen(path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err == nil {
		err = s.denyIfProtected(info)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

// readChecked reads a whole file through openRead.
func (s *Set) readChecked(path string) ([]byte, error) {
	f, _, err := s.openRead(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}
