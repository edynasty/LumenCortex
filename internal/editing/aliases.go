package editing

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

// duplicateTarget finds a change whose target is a file an earlier change already names through a
// spelling canonicalPath keeps distinct (ADR-0024 R7R2-PartialCommit-01):
//   - an existing file reached by a hard link or a letter-case variant, compared by identity
//     (os.SameFile), so a case-sensitive volume keeps "A.txt" and "a.txt" apart;
//   - a new path below the same existing directory whose remaining spelling differs only in letter
//     case, when that directory folds case.
//
// It returns the later and earlier change indexes, or -1, -1. It only stats and looks up names.
func (e *Engine) duplicateTarget(prep []*prepared) (int, int) {
	type target struct {
		file   os.FileInfo // an existing target
		anchor os.FileInfo // the deepest existing directory above a new target
		dir    string
		rest   string // the new target's path below dir
	}
	targets := make([]target, len(prep))
	for i, p := range prep {
		info, err := os.Stat(p.key)
		if err == nil {
			targets[i].file = info
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			continue // prepare reports the error for this path
		}
		dir, rest := splitExisting(p.key)
		if anchor, err := os.Stat(dir); err == nil {
			targets[i] = target{anchor: anchor, dir: dir, rest: rest}
		}
	}
	folds := e.foldsCase
	if folds == nil {
		folds = probeFoldsCase
	}
	for j := range targets {
		for i := 0; i < j; i++ {
			a, b := targets[i], targets[j]
			switch {
			case a.file != nil && b.file != nil:
				if os.SameFile(a.file, b.file) {
					return j, i
				}
			case a.anchor != nil && b.anchor != nil && os.SameFile(a.anchor, b.anchor):
				if a.rest == b.rest || (strings.EqualFold(a.rest, b.rest) && folds(a.dir)) {
					return j, i
				}
			}
		}
	}
	return -1, -1
}

// splitExisting splits an absolute path into its deepest existing ancestor and the part below it.
func splitExisting(path string) (string, string) {
	existing, rest := path, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			return existing, rest
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return existing, rest
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
}

// probeFoldsCase reports whether name lookups in dir ignore letter case. It looks up a case variant
// of an entry of dir, then of dir itself in its parent, and compares identities; it never writes.
// Case sensitivity is a property of a volume (and on Windows and Linux casefold, of a directory),
// so the answer comes from the directory itself. When no spelling can be tested it assumes the
// platform default: darwin and windows volumes fold case by default.
func probeFoldsCase(dir string) bool {
	if f, err := os.Open(dir); err == nil {
		names, _ := f.Readdirnames(32)
		_ = f.Close()
		for _, name := range names {
			if folds, ok := caseVariantSame(dir, name); ok {
				return folds
			}
		}
	}
	if folds, ok := caseVariantSame(filepath.Dir(dir), filepath.Base(dir)); ok {
		return folds
	}
	return runtime.GOOS == "darwin" || runtime.GOOS == "windows"
}

// caseVariantSame looks up name and its case-swapped spelling in dir. ok is false when name has no
// letters to swap or the lookup is inconclusive.
func caseVariantSame(dir, name string) (folds, ok bool) {
	variant := strings.Map(func(r rune) rune {
		if unicode.IsUpper(r) {
			return unicode.ToLower(r)
		}
		return unicode.ToUpper(r)
	}, name)
	if variant == name {
		return false, false
	}
	orig, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		return false, false
	}
	other, err := os.Lstat(filepath.Join(dir, variant))
	if errors.Is(err, fs.ErrNotExist) {
		return false, true
	}
	if err != nil {
		return false, false
	}
	return os.SameFile(orig, other), true
}
