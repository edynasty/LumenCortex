//go:build windows

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

func signalGroup(pgid int, sig syscall.Signal) error {
	return errors.New("eval: process groups are not supported on windows")
}

func signalPID(pid int, sig syscall.Signal) error {
	return errors.New("eval: process signals are not supported on windows")
}
