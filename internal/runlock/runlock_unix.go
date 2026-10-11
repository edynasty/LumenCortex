//go:build unix

package runlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockExclusive(f *os.File) error { return flock(f, syscall.LOCK_EX) }

func lockShared(f *os.File) error { return flock(f, syscall.LOCK_SH) }

func flock(f *os.File, how int) error {
	if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrHeld
		}
		return fmt.Errorf("runlock: flock: %w", err)
	}
	return nil
}

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
