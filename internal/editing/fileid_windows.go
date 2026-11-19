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

const (
	windowsAttrReparsePoint = 0x0400
	winErrFileExists        = syscall.Errno(80)
	winErrAlreadyExists     = syscall.Errno(183)
)

var (
	modkernel32     = syscall.NewLazyDLL("kernel32.dll")
	procMoveFileExW = modkernel32.NewProc("MoveFileExW")
)

// fileIdentity captures the Windows handle identity (VolumeSerialNumber + 64-bit FileIndex +
// CreationTime + LastWriteTime) of a file created by the current Commit call.
type fileIdentity struct {
	valid         bool
	info          os.FileInfo
	volSerial     uint32
	idxHigh       uint32
	idxLow        uint32
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
		creationTime:  d.CreationTime,
		lastWriteTime: d.LastWriteTime,
	}, nil
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

// publishTempNoReplace closes tmpFile and publishes tmpPath to dstPath via Win32 MoveFileExW with
// dwFlags=0 (no-replace), removing only tmpPath if publication fails and never touching dstPath.
func publishTempNoReplace(tmpPath string, tmpFile *os.File, dstPath string) (fileIdentity, error) {
	if closeErr := tmpFile.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return fileIdentity{}, closeErr
	}
	if err := moveFileNoReplaceWindows(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return fileIdentity{}, err
	}
	rf, err := os.Open(dstPath)
	if err != nil {
		return fileIdentity{}, nil
	}
	id, _ := captureFileIdentity(rf)
	_ = rf.Close()
	return id, nil
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

func removeCreatedAtRollback(path string, id fileIdentity, expectedContent []byte) bool {
	if !id.valid || !pathMatchesIdentity(path, id, true) {
		return false
	}
	rf, err := os.Open(path)
	if err != nil {
		return false
	}
	openedID, idErr := captureFileIdentity(rf)
	if idErr != nil ||
		openedID.volSerial != id.volSerial ||
		openedID.idxHigh != id.idxHigh ||
		openedID.idxLow != id.idxLow ||
		openedID.creationTime != id.creationTime ||
		openedID.lastWriteTime != id.lastWriteTime ||
		openedID.info.Size() != int64(len(expectedContent)) {
		_ = rf.Close()
		return false
	}
	cur, readErr := readLimited(rf, MaxFileBytes)
	_ = rf.Close()
	if readErr != nil || !bytes.Equal(cur, expectedContent) {
		return false
	}
	if !pathMatchesIdentity(path, id, true) {
		return false
	}
	if named, err := os.Lstat(path); err != nil || named.Size() != int64(len(expectedContent)) {
		return false
	}
	return os.Remove(path) == nil
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
