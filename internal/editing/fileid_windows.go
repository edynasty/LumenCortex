//go:build windows

package editing

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"syscall"
)

const windowsAttrReparsePoint = 0x0400

// fileIdentity captures the Windows handle identity (VolumeSerialNumber + 64-bit FileIndex +
// CreationTime) of a file exclusively created by the current Commit call (O_CREATE | O_EXCL).
type fileIdentity struct {
	valid        bool
	info         os.FileInfo
	volSerial    uint32
	idxHigh      uint32
	idxLow       uint32
	creationTime syscall.Filetime
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
		valid:        true,
		info:         info,
		volSerial:    d.VolumeSerialNumber,
		idxHigh:      d.FileIndexHigh,
		idxLow:       d.FileIndexLow,
		creationTime: d.CreationTime,
	}, nil
}

func (id fileIdentity) matchesHandleInfo(other os.FileInfo, d syscall.ByHandleFileInformation) bool {
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
	return os.SameFile(id.info, other)
}

func (id fileIdentity) matchesInfo(other os.FileInfo) bool {
	if !id.valid || id.info == nil || other == nil || !other.Mode().IsRegular() {
		return false
	}
	return os.SameFile(id.info, other)
}

func pathMatchesIdentity(path string, id fileIdentity) bool {
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
	// Open without following reparse points and with full sharing so we inspect the exact path entry.
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
	return id.matchesHandleInfo(named, d)
}

func removeCreatedWhileOpen(path string, f *os.File, id fileIdentity) (bool, error) {
	if !id.valid {
		return false, f.Close()
	}
	stillOurs := pathMatchesIdentity(path, id)
	closeErr := f.Close()
	if !stillOurs || !pathMatchesIdentity(path, id) {
		return false, closeErr
	}
	if err := os.Remove(path); err != nil {
		return false, closeErr
	}
	return true, closeErr
}

func removeCreatedAtRollback(path string, id fileIdentity, expectedContent []byte) bool {
	if !id.valid || !pathMatchesIdentity(path, id) {
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
		openedID.creationTime != id.creationTime {
		_ = rf.Close()
		return false
	}
	cur, readErr := readLimited(rf, MaxFileBytes)
	_ = rf.Close()
	if readErr != nil || !bytes.Equal(cur, expectedContent) {
		return false
	}
	if !pathMatchesIdentity(path, id) {
		return false
	}
	return os.Remove(path) == nil
}

// CleanupExclusivelyCreatedFile closes f after unlinking path only if path still points to the exact
// regular file handle f that the caller just created with O_CREATE|O_EXCL.
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
