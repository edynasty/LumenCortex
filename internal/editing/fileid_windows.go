//go:build windows

package editing

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"unsafe"
)

const closeHandlesBeforeFallbackRename = true

const (
	windowsAttrReparsePoint = 0x0400
	winErrFileExists        = syscall.Errno(80)
	winErrAlreadyExists     = syscall.Errno(183)
)

var (
	modkernel32     = syscall.NewLazyDLL("kernel32.dll")
	procMoveFileExW = modkernel32.NewProc("MoveFileExW")
)

type vnodeWriteWatch struct{}

func newVnodeWriteWatch(_ *os.File) (*vnodeWriteWatch, error) {
	return &vnodeWriteWatch{}, nil
}

func (*vnodeWriteWatch) wrote() bool { return false }

func (*vnodeWriteWatch) Close() error { return nil }

var renameSwapForPublish = func(_, _ string) error {
	return syscall.ENOSYS
}

func isSwapRenameUnsupportedErr(err error) bool {
	return errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL)
}

func quarantineMoveForDelete(srcPath, dstPath string, openFile *os.File) (bool, error) {
	if openFile != nil {
		if err := openFile.Close(); err != nil {
			return true, err
		}
	}
	return true, moveFileNoReplaceWindows(srcPath, dstPath)
}

func quarantineRestoreForDelete(quarantinePath, dstPath string) error {
	return moveFileNoReplaceWindows(quarantinePath, dstPath)
}

func isQuarantineUnsupportedErr(err error) bool {
	return errors.Is(err, syscall.ENOSYS) || errors.Is(err, errNoReplacePublishUnsupported)
}

// fileIdentity captures the Windows handle identity (VolumeSerialNumber + 64-bit FileIndex +
// CreationTime + LastWriteTime) of a file created by the current Commit call.
type fileIdentity struct {
	valid         bool
	info          os.FileInfo
	volSerial     uint32
	idxHigh       uint32
	idxLow        uint32
	nlink         uint32
	creationTime  syscall.Filetime
	lastWriteTime syscall.Filetime
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
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return fileIdentity{}, err
	}
	if d.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|windowsAttrReparsePoint) != 0 {
		return fileIdentity{}, fmt.Errorf("exclusively created target has directory or reparse attributes (0x%x)", d.FileAttributes)
	}
	return fileIdentity{
		valid:         true,
		info:          info,
		volSerial:     d.VolumeSerialNumber,
		idxHigh:       d.FileIndexHigh,
		idxLow:        d.FileIndexLow,
		nlink:         d.NumberOfLinks,
		creationTime:  d.CreationTime,
		lastWriteTime: d.LastWriteTime,
	}, nil
}

func (id fileIdentity) linkCount() uint64 {
	if !id.valid || id.nlink == 0 {
		return 1
	}
	return uint64(id.nlink)
}

func (id fileIdentity) matchesHandleInfo(other os.FileInfo, d syscall.ByHandleFileInformation, checkMtime bool) bool {
	if !id.valid || id.info == nil || other == nil || !other.Mode().IsRegular() {
		return false
	}
	if d.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|windowsAttrReparsePoint) != 0 {
		return false
	}
	if d.VolumeSerialNumber != id.volSerial ||
		d.FileIndexHigh != id.idxHigh ||
		d.FileIndexLow != id.idxLow ||
		d.CreationTime != id.creationTime {
		return false
	}
	if checkMtime && d.LastWriteTime != id.lastWriteTime {
		return false
	}
	return os.SameFile(id.info, other)
}

func (id fileIdentity) matchesInfo(other os.FileInfo) bool {
	if !id.valid || id.info == nil || other == nil || !other.Mode().IsRegular() {
		return false
	}
	return os.SameFile(id.info, other)
}

func (id fileIdentity) matchesOwnedCleanup(other os.FileInfo) bool {
	if !id.matchesInfo(other) {
		return false
	}
	return other.ModTime().Equal(id.info.ModTime())
}

func pathMatchesIdentity(path string, id fileIdentity, checkMtime bool) bool {
	if !id.valid {
		return false
	}
	named, err := os.Lstat(path)
	if err != nil || !named.Mode().IsRegular() {
		return false
	}
	p16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	h, err := syscall.CreateFile(
		p16,
		syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE|syscall.FILE_SHARE_DELETE,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)

	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(h, &d); err != nil {
		return false
	}
	return id.matchesHandleInfo(named, d, checkMtime)
}

func moveFileNoReplaceWindows(tmpPath, dstPath string) error {
	from16, err := syscall.UTF16PtrFromString(tmpPath)
	if err != nil {
		return err
	}
	to16, err := syscall.UTF16PtrFromString(dstPath)
	if err != nil {
		return err
	}
	// dwFlags == 0 (no MOVEFILE_REPLACE_EXISTING): atomic no-replace within the same directory.
	r1, _, e1 := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(from16)),
		uintptr(unsafe.Pointer(to16)),
		0,
	)
	if r1 == 0 {
		if errors.Is(e1, winErrFileExists) || errors.Is(e1, winErrAlreadyExists) || errors.Is(e1, os.ErrExist) {
			return &os.LinkError{Op: "MoveFileExW", Old: tmpPath, New: dstPath, Err: os.ErrExist}
		}
		var errno syscall.Errno
		if errors.As(e1, &errno) && errno != 0 {
			return &os.LinkError{Op: "MoveFileExW", Old: tmpPath, New: dstPath, Err: errno}
		}
		return &os.LinkError{Op: "MoveFileExW", Old: tmpPath, New: dstPath, Err: syscall.EINVAL}
	}
	return nil
}

const (
	winErrFileNotFound  = syscall.Errno(2)
	winErrPathNotFound  = syscall.Errno(3)
	winErrAccessDenied  = syscall.Errno(5)
	winErrNotSameDevice = syscall.Errno(17)
	winErrWriteProtect  = syscall.Errno(19)
	winErrDiskFull      = syscall.Errno(112)
	winErrInvalidName   = syscall.Errno(123)
	winErrFilenameExced = syscall.Errno(206)
)

func isCleanPrePublishErr(err error) bool {
	return isCleanPrePublishErrWindows(err)
}

func isCleanPrePublishErrWindows(err error) bool {
	return errors.Is(err, os.ErrExist) ||
		errors.Is(err, errNoReplacePublishUnsupported) ||
		errors.Is(err, os.ErrPermission) ||
		errors.Is(err, winErrFileExists) ||
		errors.Is(err, winErrAlreadyExists) ||
		errors.Is(err, winErrFileNotFound) ||
		errors.Is(err, winErrPathNotFound) ||
		errors.Is(err, winErrAccessDenied) ||
		errors.Is(err, winErrNotSameDevice) ||
		errors.Is(err, winErrWriteProtect) ||
		errors.Is(err, winErrDiskFull) ||
		errors.Is(err, winErrInvalidName) ||
		errors.Is(err, winErrFilenameExced)
}

func capturePostPublishIdentityWindows(dstPath string, f *os.File, hooks PublishFaultHooks) (fileIdentity, error, bool) {
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
	var d syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &d); err != nil {
		return fileIdentity{}, err, false
	}
	if d.FileAttributes&(syscall.FILE_ATTRIBUTE_DIRECTORY|windowsAttrReparsePoint) != 0 {
		return fileIdentity{}, fmt.Errorf("exclusively created target has directory or reparse attributes (0x%x)", d.FileAttributes), false
	}
	return fileIdentity{
		valid:         true,
		info:          info,
		volSerial:     d.VolumeSerialNumber,
		idxHigh:       d.FileIndexHigh,
		idxLow:        d.FileIndexLow,
		nlink:         d.NumberOfLinks,
		creationTime:  d.CreationTime,
		lastWriteTime: d.LastWriteTime,
	}, nil, false
}

// publishTempNoReplace closes tmpFile and publishes tmpPath to dstPath via Win32 MoveFileExW with
// dwFlags=0 (no-replace), removing only tmpPath if publication fails and never unlinking dstPath on
// single-file post-publication error (ADR-0024 R4-02).
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
		if err != nil || info.Size() != preID.info.Size() || !pathMatchesIdentity(tmpPath, preID, true) {
			return true
		}
		rmErr := os.Remove(tmpPath)
		return rmErr != nil && !errors.Is(rmErr, os.ErrNotExist)
	}

	if closeErr := tmpFile.Close(); closeErr != nil {
		leftover := removeTmp()
		if leftover {
			return publishOutcome{state: PublishUncertain, tempLeftover: true}, closeErr
		}
		return publishOutcome{state: PublishNone}, closeErr
	}
	var moveErr error
	if hooks.PublishUncertainErr != nil {
		moveErr = hooks.PublishUncertainErr(dstPath)
	} else {
		moveErr = moveFileNoReplaceWindows(tmpPath, dstPath)
	}
	if moveErr != nil {
		leftover := removeTmp()
		if preID.valid && pathMatchesIdentity(dstPath, preID, false) {
			return publishOutcome{
				state:        PublishSucceededWithPostError,
				published:    true,
				verified:     false,
				tempLeftover: leftover,
				id:           preID,
			}, moveErr
		}
		if isCleanPrePublishErrWindows(moveErr) && !leftover {
			return publishOutcome{state: PublishNone}, moveErr
		}
		return publishOutcome{state: PublishUncertain, tempLeftover: leftover}, moveErr
	}

	// Destination publication via MoveFileExW has irrevocably succeeded.
	var hookErr error
	if hooks.AfterRenameSuccess != nil {
		hookErr = hooks.AfterRenameSuccess(dstPath)
	} else if hooks.AfterLinkSuccess != nil {
		hookErr = hooks.AfterLinkSuccess(dstPath)
	}

	rf, openErr := os.Open(dstPath)
	var (
		postID          fileIdentity
		postIDErr       error
		idFaultInjected bool
		closeErr        error
	)
	if openErr != nil {
		postIDErr = openErr
		if hooks.CaptureIdentity != nil {
			if cerr := hooks.CaptureIdentity(dstPath, nil); cerr != nil {
				postIDErr = errors.Join(postIDErr, cerr)
				idFaultInjected = true
			}
		}
	} else {
		postID, postIDErr, idFaultInjected = capturePostPublishIdentityWindows(dstPath, rf, hooks)
		closeErr = rf.Close()
	}
	if hooks.CloseAfterPublish != nil {
		closeErr = errors.Join(closeErr, hooks.CloseAfterPublish(dstPath))
	}
	finalID := postID
	if postIDErr != nil {
		if idFaultInjected {
			finalID = fileIdentity{}
		} else if preID.valid {
			finalID = preID
		}
	}
	postErr := errors.Join(hookErr, postIDErr, closeErr)
	if postErr == nil && !preID.matchesInfo(postID.info) {
		postErr = errors.New("published file identity changed")
	}
	if postErr != nil {
		return publishOutcome{
			state:     PublishSucceededWithPostError,
			published: true,
			verified:  false,
			id:        finalID,
		}, postErr
	}
	return publishOutcome{
		state:     PublishSucceededAndVerified,
		published: true,
		verified:  true,
		id:        finalID,
	}, nil
}

func removeCreatedWhileOpen(path string, f *os.File, id fileIdentity, expectedContent []byte) (bool, error) {
	if !id.valid || f == nil {
		var closeErr error
		if f != nil {
			closeErr = f.Close()
		}
		return false, closeErr
	}
	canRemove := false
	if curInfo, statErr := f.Stat(); statErr == nil && curInfo.Size() == int64(len(expectedContent)) {
		if _, seekErr := f.Seek(0, io.SeekStart); seekErr == nil {
			cur, readErr := io.ReadAll(io.LimitReader(f, int64(len(expectedContent))+1))
			if readErr == nil && bytes.Equal(cur, expectedContent) && pathMatchesIdentity(path, id, true) {
				canRemove = true
			}
		}
	}
	closeErr := f.Close()
	if !canRemove || !pathMatchesIdentity(path, id, true) {
		return false, closeErr
	}
	if named, err := os.Lstat(path); err != nil || named.Size() != int64(len(expectedContent)) {
		return false, closeErr
	}
	if err := os.Remove(path); err != nil {
		return false, closeErr
	}
	return true, closeErr
}

// CleanupExclusivelyCreatedFile closes f after unlinking path only if path still points to the exact
// empty regular file handle f that the caller just created with O_CREATE|O_EXCL.
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
