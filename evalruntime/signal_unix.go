//go:build !windows

package evalruntime

import (
	"errors"
	"syscall"
)

const (
	sigINT  = syscall.SIGINT
	sigTERM = syscall.SIGTERM
	sigKILL = syscall.SIGKILL
)

// signalGroup signals the process group led by pgid. A negative pid addresses the
// group, which is why the worker is started with Setpgid.
func signalGroup(pgid int, sig syscall.Signal) error {
	if pgid <= 0 {
		return errors.New("eval: invalid process group")
	}
	return syscall.Kill(-pgid, sig)
}

// signalPID signals one process: used for descendants that left the worker's
// process group with setsid()/setpgid().
func signalPID(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return errors.New("eval: invalid pid")
	}
	err := syscall.Kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
