//go:build unix

package runlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func lockExclusive(f *os.File) error {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrHeld
		}
		return fmt.Errorf("runlock: flock: %w", err)
	}
	return nil
}

func unlock(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
