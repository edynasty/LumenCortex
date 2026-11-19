//go:build !windows

package editing

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// fileIdentity captures the OS identity of a file exclusively created by the current Commit call
// (O_CREATE | O_EXCL) so cleanup and rollback never unlink a foreign file that replaced the path.
type fileIdentity struct {
	valid     bool
	info      os.FileInfo
	hasDevIno bool
	dev       uint64
	ino       uint64
}

func captureFileIdentity(f *os.File) (fileIdentity, error) {
	if f == nil {
		return fileIdentity{}, errors.New("nil file descriptor")
	}
	info, err := f.Stat()
	if err != nil {
		return fileIdentity{}, err
	}
	if !info.Mode().IsRegular() {
		return fileIdentity{}, fmt.Errorf("exclusively created target is not a regular file (mode=%v)", info.Mode())
	}
	id := fileIdentity{valid: true, info: info}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		id.hasDevIno = true
		id.dev = uint64(st.Dev)
		id.ino = uint64(st.Ino)
	}
	return id, nil
}

func (id fileIdentity) matchesInfo(other os.FileInfo) bool {
	if !id.valid || id.info == nil || other == nil || !other.Mode().IsRegular() {
		return false
	}
	if !os.SameFile(id.info, other) {
		return false
	}
	if id.hasDevIno {
		st, ok := other.Sys().(*syscall.Stat_t)
		if !ok || st == nil {
			return false
		}
		if uint64(st.Dev) != id.dev || uint64(st.Ino) != id.ino {
			return false
		}
	}
	return true
}

// removeCreatedWhileOpen unlinks path only if it still names the exact regular inode held open by f.
// Keeping f open across Lstat and Remove prevents kernel inode reuse before the unlink completes.
func removeCreatedWhileOpen(path string, f *os.File, id fileIdentity) (bool, error) {
	removed := false
	if id.valid {
		if named, err := os.Lstat(path); err == nil && id.matchesInfo(named) {
			if rmErr := os.Remove(path); rmErr == nil {
				removed = true
			}
		}
	}
	closeErr := f.Close()
	return removed, closeErr
}

// removeCreatedAtRollback unlinks path during multi-file rollback only if path still resolves without
// symlink indirection to a regular file matching both the creation fileIdentity and the exact bytes
// written by this commit.
func removeCreatedAtRollback(path string, id fileIdentity, expectedContent []byte) bool {
	if !id.valid {
		return false
	}
	named, err := os.Lstat(path)
	if err != nil || !id.matchesInfo(named) {
		return false
	}
	rf, err := os.Open(path)
	if err != nil {
		return false
	}
	defer rf.Close()

	opened, err := rf.Stat()
	if err != nil || !id.matchesInfo(opened) || !os.SameFile(named, opened) {
		return false
	}
	cur, err := readLimited(rf, MaxFileBytes)
	if err != nil || !bytes.Equal(cur, expectedContent) {
		return false
	}
	// Re-check Lstat while rf holds the inode open so a concurrent rename between Open and Remove is caught.
	namedAfter, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, namedAfter) || !id.matchesInfo(namedAfter) {
		return false
	}
	return os.Remove(path) == nil
}

// CleanupExclusivelyCreatedFile closes f after unlinking path only if path still points to the exact
// regular file descriptor f that the caller just created with O_CREATE|O_EXCL.
func CleanupExclusivelyCreatedFile(path string, f *os.File) error {
	if f == nil {
		return nil
	}
	id, err := captureFileIdentity(f)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	_, closeErr := removeCreatedWhileOpen(path, f, id)
	return closeErr
}
