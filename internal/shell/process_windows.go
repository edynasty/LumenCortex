//go:build windows

package shell

import "os/exec"

// StartGroup keeps exec's default cancellation (kill cmd.exe); Windows has no POSIX process groups.
// WaitDelay still bounds the wait for pipes a surviving child holds.
func StartGroup(*exec.Cmd) {}

// KillGroup kills cmd.exe.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// TermGroup has no graceful equivalent for cmd.exe: the grace period passes and KillGroup follows.
func TermGroup(int) error { return nil }
