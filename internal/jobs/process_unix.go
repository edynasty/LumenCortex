//go:build !windows

package jobs

import (
	"errors"
	"os/exec"
	"syscall"
)

// The signals a stop uses: a timeout asks first (SIGTERM) and kills after the grace period; every other
// stop kills the group at once.
const (
	sigTerm = syscall.SIGTERM
	sigKill = syscall.SIGKILL
)

// setProcessGroup makes the command lead its own process group, so signalling the group reaches every
// process the shell started, not only the shell.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup sends sig to the process group led by pid. cmd is used when pid is unknown. A group that
// is already gone is not an error.
func signalGroup(cmd *execCommand, pid int, sig syscall.Signal) error {
	if pid <= 0 && cmd != nil && cmd.cmd.Process != nil {
		pid = cmd.cmd.Process.Pid
	}
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// processAlive reports whether pid names a live process (signal 0 succeeds or is refused as another
// user's). A zombie of this process still counts as alive until it is reaped, which the job manager
// does through Wait.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
