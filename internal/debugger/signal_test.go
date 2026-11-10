//go:build unix

package debugger

import (
	"errors"
	"syscall"
)

// sendSignal0 reports whether the process still exists.
func sendSignal0(pid int) error {
	if pid <= 1 {
		return errors.New("invalid pid")
	}
	return syscall.Kill(pid, 0)
}
