//go:build windows

package runlock

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

const (
	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2
	errorLockViolation      = syscall.Errno(33)
)

var (
	kernel32     = syscall.NewLazyDLL("kernel32.dll")
	procLockFile = kernel32.NewProc("LockFileEx")
	procUnlock   = kernel32.NewProc("UnlockFileEx")
)

func lockExclusive(f *os.File) error {
	return lockFileEx(f, lockfileExclusiveLock|lockfileFailImmediately)
}

// lockShared omits LOCKFILE_EXCLUSIVE_LOCK: any number of handles may hold the
// byte shared, and none while an exclusive handle holds it.
func lockShared(f *os.File) error { return lockFileEx(f, lockfileFailImmediately) }

func lockFileEx(f *os.File, flags uintptr) error {
	var overlapped syscall.Overlapped
	r, _, err := procLockFile.Call(f.Fd(), flags, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r != 0 {
		return nil
	}
	if err == errorLockViolation {
		return ErrHeld
	}
	return fmt.Errorf("runlock: LockFileEx: %w", err)
}

func unlock(f *os.File) error {
	var overlapped syscall.Overlapped
	r, _, err := procUnlock.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r != 0 {
		return nil
	}
	return err
}
