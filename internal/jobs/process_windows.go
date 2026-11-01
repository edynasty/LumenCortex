//go:build windows

package jobs

import (
	"os/exec"
	"syscall"
)

// Windows has no POSIX process groups: a stop kills the shell process, and WaitDelay bounds the wait
// for the pipes a surviving child holds.
const (
	sigTerm = syscall.Signal(15)
	sigKill = syscall.Signal(9)
)

func shellPath() string { return "cmd.exe" }

func shellArgs(command string) []string { return []string{"/C", command} }

func setProcessGroup(*exec.Cmd) {}

// signalGroup kills the command's process; a job owned by another process cannot be signalled here.
func signalGroup(cmd *execCommand, pid int, sig syscall.Signal) error {
	_ = pid
	_ = sig
	if cmd == nil || cmd.cmd.Process == nil {
		return nil
	}
	return cmd.cmd.Process.Kill()
}

// processAlive cannot be answered with signal 0 on Windows; an owner of another process is treated as
// gone, so its running jobs become outcome_unknown instead of being killed.
func processAlive(pid int) bool { return false }
