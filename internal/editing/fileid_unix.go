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

// isNoReplaceRenameUnsupportedErr reports a no-replace rename the filesystem or kernel does not
// implement: ENOTSUP/EOPNOTSUPP (macOS renameatx_np RENAME_EXCL on exFAT), EINVAL (Linux renameat2
// RENAME_NOREPLACE on a filesystem without it) and ENOSYS (no such call).
func isNoReplaceRenameUnsupportedErr(err error) bool {
	return errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOSYS)
}

// linkForPublish and renameNoReplaceForPublish are the kernel primitives publishTempNoReplace uses;
// tests replace them to simulate a filesystem that supports neither.
var (
	linkForPublish            = os.Link
	renameNoReplaceForPublish = renameNoReplacePlatform
)

func isCleanPrePublishErr(err error) bool {
	return errors.Is(err, os.ErrExist) ||
		errors.Is(err, errNoReplacePublishUnsupported) ||
		errors.Is(err, syscall.EACCES) ||
		errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EROFS) ||
		errors.Is(err, syscall.ENOENT) ||
		errors.Is(err, syscall.ENOTDIR) ||
		errors.Is(err, syscall.EXDEV) ||
		errors.Is(err, syscall.ENAMETOOLONG) ||
		errors.Is(err, syscall.EMLINK) ||
		errors.Is(err, syscall.ELOOP) ||
		errors.Is(err, syscall.EISDIR) ||
		errors.Is(err, syscall.ENOSPC) ||
		errors.Is(err, syscall.EDQUOT) ||
		errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOSYS) ||
		errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOTSUP)
}

func capturePostPublishIdentity(dstPath string, f *os.File, hooks PublishFaultHooks) (fileIdentity, error, bool) {
	if f == nil {
		return fileIdentity{}, errors.New("nil file descriptor"), false
	}
	var (
		info os.FileInfo
		err  error
	)
	if hooks.StatAfterPublish != nil {
		info, err = hooks.StatAfterPublish(dstPath, f)
	} else {
		info, err = f.Stat()
	}
	if err != nil {
		if hooks.CaptureIdentity != nil {
			if cerr := hooks.CaptureIdentity(dstPath, f); cerr != nil {
				return fileIdentity{}, errors.Join(err, cerr), true
			}
		}
		return fileIdentity{}, err, false
	}
	if !info.Mode().IsRegular() {
		return fileIdentity{}, fmt.Errorf("exclusively created target is not a regular file (mode=%v)", info.Mode()), false
	}
	if hooks.CaptureIdentity != nil {
		if cerr := hooks.CaptureIdentity(dstPath, f); cerr != nil {
			return fileIdentity{}, cerr, true
		}
	}
	id := fileIdentity{valid: true, info: info, modTime: info.ModTime()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		id.hasDevIno = true
		id.dev = uint64(st.Dev)
		id.ino = uint64(st.Ino)
	}
	return id, nil, false
}

// publishTempNoReplace atomically publishes tmpPath (held open by tmpFile) to dstPath without
// replacing any existing entry at dstPath, always removing the private tmpPath entry and never
// unlinking dstPath on single-file post-publication error (ADR-0024 §8, R2-01, R4-02). Once link or
// no-replace rename succeeds, publication is irrevocably recorded even if a later Stat, Close, or
// identity capture fails.
func publishTempNoReplace(tmpPath string, tmpFile *os.File, dstPath string, hooks PublishFaultHooks) (publishOutcome, error) {
	removeTmp := func() bool {
		rmErr := os.Remove(tmpPath)
		return rmErr != nil && !errors.Is(rmErr, os.ErrNotExist)
	}

	preID, _ := captureFileIdentity(tmpFile)

	usedRename := false
	var err error
	if hooks.PublishUncertainErr != nil {
		err = hooks.PublishUncertainErr(dstPath)
	} else {
		if hooks.ForceLinkFallback != nil {
			err = hooks.ForceLinkFallback(dstPath)
		} else {
			err = linkForPublish(tmpPath, dstPath)
		}
		if err != nil && isLinkFallbackErr(err) {
			linkErr := err
			usedRename = true
			err = renameNoReplaceForPublish(tmpPath, dstPath)
			if err != nil && isNoReplaceRenameUnsupportedErr(err) {
				err = fmt.Errorf("%w (link: %v; no-replace rename: %v)", errNoReplacePublishUnsupported, linkErr, err)
			}
		}
	}
	if err != nil {
		_ = tmpFile.Close()
		leftover := removeTmp()
		if preID.valid {
			if named, lerr := os.Lstat(dstPath); lerr == nil && preID.matchesInfo(named) {
				return publishOutcome{
					state:        PublishSucceededWithPostError,
					published:    true,
					verified:     false,
					tempLeftover: leftover,
					id:           preID,
				}, err
			}
		}
		if isCleanPrePublishErr(err) && !leftover {
			return publishOutcome{state: PublishNone}, err
		}
		return publishOutcome{state: PublishUncertain, tempLeftover: leftover}, err
	}

	// Destination publication has irrevocably succeeded: remove tmpPath before capturing identity on
	// tmpFile so st_nlink is settled at 1.
	leftover := removeTmp()
	var hookErr error
	if usedRename {
		if hooks.AfterRenameSuccess != nil {
			hookErr = hooks.AfterRenameSuccess(dstPath)
		}
	} else {
		if hooks.AfterLinkSuccess != nil {
			hookErr = hooks.AfterLinkSuccess(dstPath)
		}
	}
	postID, idErr, idFaultInjected := capturePostPublishIdentity(dstPath, tmpFile, hooks)
	var closeErr error
	if tmpFile != nil {
		closeErr = tmpFile.Close()
	}
	if hooks.CloseAfterPublish != nil {
		closeErr = errors.Join(closeErr, hooks.CloseAfterPublish(dstPath))
	}
	finalID := postID
	if idErr != nil {
		if idFaultInjected {
			finalID = fileIdentity{}
		} else if preID.valid {
			finalID = preID
		}
	}
	postErr := errors.Join(hookErr, idErr, closeErr)
	if postErr != nil {
		return publishOutcome{
			state:        PublishSucceededWithPostError,
			published:    true,
			verified:     false,
			tempLeftover: leftover,
			id:           finalID,
		}, postErr
	}
	return publishOutcome{
		state:        PublishSucceededAndVerified,
		published:    true,
		verified:     true,
		tempLeftover: leftover,
		id:           finalID,
	}, nil
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
