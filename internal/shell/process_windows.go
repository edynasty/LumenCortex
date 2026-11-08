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

// guardedGroup is cmd.exe alone: Windows has no POSIX process groups and no crash guard.
type guardedGroup struct {
	cmd *exec.Cmd
}

func newGuardedGroup(cmd *exec.Cmd) (*guardedGroup, error) {
	return &guardedGroup{cmd: cmd}, nil
}

// term has no graceful equivalent for cmd.exe: the grace period passes and kill follows.
func (g *guardedGroup) term() error { return nil }

func (g *guardedGroup) kill() error { return KillGroup(g.cmd) }

func (g *guardedGroup) release(reap bool) {
	if reap {
		_ = g.kill()
	}
}
