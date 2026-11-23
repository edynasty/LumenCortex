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

const closeHandlesBeforeFallbackRename = false

// fileIdentity records an observation of a regular descriptor. It is not a content lock.
type fileIdentity struct {
	valid     bool
	info      os.FileInfo
	hasDevIno bool
	dev       uint64
	ino       uint64
	nlink     uint64
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
	id := fileIdentity{valid: true, info: info, nlink: 1, modTime: info.ModTime()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		id.hasDevIno = true
		id.dev = uint64(st.Dev)
		id.ino = uint64(st.Ino)
		id.nlink = uint64(st.Nlink)
	}
	return id, nil
}

func (id fileIdentity) linkCount() uint64 {
	if !id.valid || id.nlink == 0 {
		return 1
	}
	return id.nlink
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

// isSwapRenameUnsupportedErr reports an atomic exchange rename the filesystem or kernel does not
// implement (for example FAT32/exFAT or non-darwin/linux Unix).
func isSwapRenameUnsupportedErr(err error) bool {
	return errors.Is(err, syscall.EOPNOTSUPP) ||
		errors.Is(err, syscall.ENOTSUP) ||
		errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOSYS)
}

// linkForPublish, renameNoReplaceForPublish, renameNoReplaceForQuarantine and renameSwapForPublish
// are the kernel primitives publication uses; tests may replace them individually to simulate
// filesystem support boundaries.
var (
	linkForPublish               = os.Link
	renameNoReplaceForPublish    = renameNoReplacePlatform
	renameNoReplaceForQuarantine = renameNoReplacePlatform
	renameSwapForPublish         = renameSwapPlatform
)

func quarantineMoveForDelete(srcPath, dstPath string, _ *os.File) (bool, error) {
	return false, renameNoReplaceForQuarantine(srcPath, dstPath)
}

func quarantineRestoreForDelete(quarantinePath, dstPath string) error {
	return renameNoReplaceForQuarantine(quarantinePath, dstPath)
}

func isQuarantineUnsupportedErr(err error) bool {
	return isNoReplaceRenameUnsupportedErr(err)
}

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
	id := fileIdentity{valid: true, info: info, nlink: 1, modTime: info.ModTime()}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && st != nil {
		id.hasDevIno = true
		id.dev = uint64(st.Dev)
		id.ino = uint64(st.Ino)
		id.nlink = uint64(st.Nlink)
	}
	return id, nil, false
}

// publishTempNoReplace atomically publishes tmpPath (held open by tmpFile) to dstPath without
// replacing an existing entry. After a rename the old name is no longer ours;
// after a link, cleanup is limited to the private sibling, never dstPath.
// Record publication even when a later stat, close or identity check fails.
func publishTempNoReplace(tmpPath string, tmpFile *os.File, dstPath string, hooks PublishFaultHooks) (publishOutcome, error) {
	preID, idErr := captureFileIdentity(tmpFile)
	if idErr != nil {
		return publishOutcome{state: PublishUncertain, tempLeftover: true}, errors.Join(idErr, tmpFile.Close())
	}
	removeTmp := func() bool {
		info, err := os.Lstat(tmpPath)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil || !preID.matchesOwnedCleanup(info) || info.Size() != preID.info.Size() {
			return true
		}
		rmErr := os.Remove(tmpPath)
		return rmErr != nil && !errors.Is(rmErr, os.ErrNotExist)
	}

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
		leftover := removeTmp()
		_ = tmpFile.Close()
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

	// Rename consumed the old name. Only the hard-link path leaves a sibling to unlink.
	leftover := !usedRename && removeTmp()
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
	if leftover {
		postErr = errors.Join(postErr, errors.New("private staging entry could not be cleaned safely"))
	}
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

// removeCreatedWhileOpen cleans a private staging entry only when the observed
// name, descriptor and expected bytes still match. It is not a destination rollback.
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
