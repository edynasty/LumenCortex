//go:build !windows

package shell

import (
	"errors"
	"os/exec"
	"syscall"
)

// StartGroup makes the command lead its own process group, so a kill reaches every process it started,
// and makes cancellation kill that group instead of the command alone. The shell tool and the stdio MCP
// client both use it: a command's own children (a shell's background jobs, a browser under an MCP
// server) outlive it otherwise.
func StartGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillGroup(cmd) }
}

// KillGroup sends SIGKILL to the command's process group. A group that is already gone is not an error.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// TermGroup sends SIGTERM to the process group led by pid: a timed-out command gets a chance to exit
// cleanly before KillGroup. A group that is already gone is not an error.
func TermGroup(pid int) error {
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
