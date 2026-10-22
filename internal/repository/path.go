package repository

import (
	"os"
	"path/filepath"
	"strings"
)

// ConfinedFile reports the real location and the Lstat info of abs when abs is a regular file whose
// real location, symlinks resolved, is inside root. ok is false for a missing path, a non-regular
// file (a directory, a device, a pipe), and a file that resolves outside root: a symlink in the
// workspace that points elsewhere names a file that is not part of the workspace, whatever its name
// says. root and abs must be absolute.
//
// It is the rule every workspace read of a single known file follows, so a caller that reads a file
// it found by name (a cite, an instruction file) cannot be led out of the workspace by a link.
func ConfinedFile(root, abs string) (real string, info os.FileInfo, ok bool) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", nil, false
	}
	return confinedFile(realRoot, abs)
}

// confinedFile is ConfinedFile with root already resolved, for a caller that resolves the workspace
// once and checks many paths.
func confinedFile(realRoot, abs string) (real string, info os.FileInfo, ok bool) {
	// Lstat sees only the last element; a symlinked directory on the way may lead outside.
	var err error
	if info, err = os.Lstat(abs); err != nil || !info.Mode().IsRegular() {
		return "", nil, false
	}
	if real, err = filepath.EvalSymlinks(abs); err != nil {
		return "", nil, false
	}
	if !insideReal(realRoot, real) {
		return "", nil, false
	}
	return real, info, true
}

// insideReal reports whether real, a resolved path, is realRoot or inside it. Both must be resolved:
// a prefix comparison on unresolved paths would accept a symlinked workspace that points elsewhere.
func insideReal(realRoot, real string) bool {
	rel, err := filepath.Rel(realRoot, real)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
