//go:build !windows

package shell

import (
	"errors"
	"os/exec"
	"syscall"
)

// startGroup makes the command lead its own process group, so a kill reaches every process the shell
// started, and makes cancellation kill that group instead of the shell alone.
func startGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
}

// killGroup sends SIGKILL to the command's process group. A group that is already gone is not an error.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
