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
// and exact bytes written by this commit. Refusals classify external modifications separately from
// rollback I/O failures and always return a descriptive error (ADR-0024 R4-02-A, R4-02-B, R4-02-C).
func removeCreatedAtRollback(path string, id fileIdentity, expectedContent []byte) (PublishState, error) {
	if !id.valid {
		return PublishRollbackFailed, errors.New("created file identity unavailable; refused blind unlink")
	}
	named, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PublishExternalModification, errors.New("created file removed externally before rollback")
		}
		return PublishRollbackFailed, err
	}
	if !id.matchesOwnedCleanup(named) || named.Size() != int64(len(expectedContent)) {
		return PublishExternalModification, errors.New("external modification detected on created file; preserved disk contents")
	}
	rf, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PublishExternalModification, errors.New("created file removed externally before rollback")
		}
		return PublishRollbackFailed, err
	}
	defer rf.Close()

	opened, err := rf.Stat()
	if err != nil {
		return PublishRollbackFailed, err
	}
	if !id.matchesOwnedCleanup(opened) || !os.SameFile(named, opened) || opened.Size() != int64(len(expectedContent)) {
		return PublishExternalModification, errors.New("external modification detected on created file; preserved disk contents")
	}
	cur, err := readLimited(rf, MaxFileBytes)
	if err != nil {
		if CodeOf(err) == CodeTooLarge {
			return PublishExternalModification, errors.New("external modification detected on created file; preserved disk contents")
		}
		return PublishRollbackFailed, err
	}
	if !bytes.Equal(cur, expectedContent) {
		return PublishExternalModification, errors.New("external modification detected on created file; preserved disk contents")
	}
	namedAfter, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PublishExternalModification, errors.New("created file removed externally before rollback")
		}
		return PublishRollbackFailed, err
	}
	if !os.SameFile(opened, namedAfter) || !id.matchesOwnedCleanup(namedAfter) || namedAfter.Size() != int64(len(expectedContent)) {
		return PublishExternalModification, errors.New("external modification detected on created file; preserved disk contents")
	}
	if err := os.Remove(path); err != nil {
		return PublishRollbackFailed, err
	}
	return PublishRolledBack, nil
}

// verifyUnmodifiedForRollback verifies on the open descriptor f that path still resolves without
// symlink replacement to the exact inode, modification timestamp, byte length, and byte content
// written by this commit before conditional rollback overwrites it (ADR-0024 R4-02-C).
func verifyUnmodifiedForRollback(path string, f *os.File, id fileIdentity, expectedContent []byte) (PublishState, error) {
	if !id.valid || f == nil {
		return PublishRollbackFailed, errors.New("written file identity unavailable; refused blind overwrite during rollback")
	}
	named, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PublishExternalModification, errors.New("target removed externally before rollback")
		}
		return PublishRollbackFailed, err
	}
	if !id.matchesOwnedCleanup(named) || named.Size() != int64(len(expectedContent)) {
		return PublishExternalModification, errors.New("external modification detected before rollback; preserved disk contents")
	}
	opened, err := f.Stat()
	if err != nil {
		return PublishRollbackFailed, err
	}
	if !id.matchesOwnedCleanup(opened) || !os.SameFile(named, opened) || opened.Size() != int64(len(expectedContent)) {
		return PublishExternalModification, errors.New("external modification detected before rollback; preserved disk contents")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return PublishRollbackFailed, err
	}
	cur, err := readLimited(f, MaxFileBytes)
	if err != nil {
		if CodeOf(err) == CodeTooLarge {
			return PublishExternalModification, errors.New("external modification detected before rollback; preserved disk contents")
		}
		return PublishRollbackFailed, err
	}
	if !bytes.Equal(cur, expectedContent) {
		return PublishExternalModification, errors.New("external modification detected before rollback; preserved disk contents")
	}
	namedAfter, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PublishExternalModification, errors.New("target removed externally before rollback")
		}
		return PublishRollbackFailed, err
	}
	if !os.SameFile(opened, namedAfter) || !id.matchesOwnedCleanup(namedAfter) || namedAfter.Size() != int64(len(expectedContent)) {
		return PublishExternalModification, errors.New("external modification detected before rollback; preserved disk contents")
	}
	return "", nil
}

// inspectExternalModificationReadOnly performs a read-only check after an in-place Close failure to
// detect whether an external writer already modified, replaced, or removed path without ever
// modifying disk contents (ADR-0024 R4-02-C).
func inspectExternalModificationReadOnly(path string, id fileIdentity, expectedContent []byte) bool {
	named, err := os.Lstat(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	if !named.Mode().IsRegular() {
		return true
	}
	if id.valid && !id.matchesInfo(named) {
		return true
	}
	rf, err := os.Open(path)
	if err != nil {
		return errors.Is(err, os.ErrNotExist)
	}
	defer rf.Close()
	opened, err := rf.Stat()
	if err != nil {
		return false
	}
	if !os.SameFile(named, opened) || opened.Size() != int64(len(expectedContent)) {
		return true
	}
	cur, err := readLimited(rf, MaxFileBytes)
	if err != nil {
		return CodeOf(err) == CodeTooLarge
	}
	return !bytes.Equal(cur, expectedContent)
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
