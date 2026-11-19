//go:build !windows

package editing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

// fileIdentity captures the OS identity and write timestamp of a file exclusively created by the
// current Commit call so cleanup and rollback never unlink a foreign or externally modified file.
type fileIdentity struct {
	valid     bool
	info      os.FileInfo
	hasDevIno bool
	dev       uint64
	ino       uint64
	modTime   time.Time
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
	id := fileIdentity{valid: true, info: info, modTime: info.ModTime()}
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

func (id fileIdentity) matchesOwnedCleanup(other os.FileInfo) bool {
	if !id.matchesInfo(other) {
		return false
	}
	return other.ModTime().Equal(id.modTime)
}

func isLinkFallbackErr(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.ENOSYS)
}

// publishTempNoReplace atomically publishes tmpPath (held open by tmpFile) to dstPath without
// replacing any existing entry at dstPath, always removing the private tmpPath entry and never
// unlinking dstPath on error (ADR-0024 §8, R2-01).
func publishTempNoReplace(tmpPath string, tmpFile *os.File, dstPath string) (fileIdentity, error) {
	defer func() { _ = os.Remove(tmpPath) }()

	err := os.Link(tmpPath, dstPath)
	if err != nil && isLinkFallbackErr(err) {
		err = renameNoReplacePlatform(tmpPath, dstPath)
	}
	if err != nil {
		_ = tmpFile.Close()
		return fileIdentity{}, err
	}
	// Remove tmpPath before capturing identity on tmpFile so st_nlink is settled at 1.
	_ = os.Remove(tmpPath)
	id, idErr := captureFileIdentity(tmpFile)
	closeErr := tmpFile.Close()
	if idErr != nil {
		return fileIdentity{}, errors.Join(idErr, closeErr)
	}
	return id, closeErr
}

// removeCreatedWhileOpen unlinks path ONLY when f still matches id, f's current byte content and
// size equal expectedContent (proving no foreign writer wrote into this inode), and path still
// resolves without symlink indirection to the exact unmodified regular file.
func removeCreatedWhileOpen(path string, f *os.File, id fileIdentity, expectedContent []byte) (bool, error) {
	removed := false
	if id.valid && f != nil {
		if curInfo, statErr := f.Stat(); statErr == nil && curInfo.Size() == int64(len(expectedContent)) && id.matchesOwnedCleanup(curInfo) {
			if _, seekErr := f.Seek(0, io.SeekStart); seekErr == nil {
				cur, readErr := io.ReadAll(io.LimitReader(f, int64(len(expectedContent))+1))
				if readErr == nil && bytes.Equal(cur, expectedContent) {
					if named, lerr := os.Lstat(path); lerr == nil && id.matchesOwnedCleanup(named) && named.Size() == int64(len(expectedContent)) {
						if rmErr := os.Remove(path); rmErr == nil {
							removed = true
						}
					}
				}
			}
		}
	}
	var closeErr error
	if f != nil {
		closeErr = f.Close()
	}
	return removed, closeErr
}

// removeCreatedAtRollback unlinks path during multi-file rollback only if path still resolves without
// symlink indirection to a regular file matching the creation fileIdentity, exact modification timestamp,
// and exact bytes written by this commit.
func removeCreatedAtRollback(path string, id fileIdentity, expectedContent []byte) bool {
	if !id.valid {
		return false
	}
	named, err := os.Lstat(path)
	if err != nil || !id.matchesOwnedCleanup(named) || named.Size() != int64(len(expectedContent)) {
		return false
	}
	rf, err := os.Open(path)
	if err != nil {
		return false
	}
	defer rf.Close()

	opened, err := rf.Stat()
	if err != nil || !id.matchesOwnedCleanup(opened) || !os.SameFile(named, opened) || opened.Size() != int64(len(expectedContent)) {
		return false
	}
	cur, err := readLimited(rf, MaxFileBytes)
	if err != nil || !bytes.Equal(cur, expectedContent) {
		return false
	}
	namedAfter, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, namedAfter) || !id.matchesOwnedCleanup(namedAfter) || namedAfter.Size() != int64(len(expectedContent)) {
		return false
	}
	return os.Remove(path) == nil
}

// CleanupExclusivelyCreatedFile closes f after unlinking path only if path still points to the exact
// empty regular file descriptor f that the caller just created with O_CREATE|O_EXCL.
func CleanupExclusivelyCreatedFile(path string, f *os.File) error {
	if f == nil {
		return nil
	}
	id, err := captureFileIdentity(f)
	if err != nil {
		return errors.Join(err, f.Close())
	}
	_, closeErr := removeCreatedWhileOpen(path, f, id, nil)
	return closeErr
}
