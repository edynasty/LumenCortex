//go:build windows

package shell

import "os/exec"

// startGroup keeps exec's default cancellation (kill cmd.exe); Windows has no POSIX process groups.
// WaitDelay still bounds the wait for pipes a surviving child holds.
func startGroup(*exec.Cmd) {}

// killGroup kills cmd.exe.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
