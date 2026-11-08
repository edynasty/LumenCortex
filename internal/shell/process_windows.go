//go:build windows

package shell

import "os/exec"

// guardedGroup is cmd.exe alone: Windows has no POSIX process groups and no crash guard.
type guardedGroup struct {
	cmd *exec.Cmd
}

func newGuardedGroup(cmd *exec.Cmd) (*guardedGroup, error) {
	return &guardedGroup{cmd: cmd}, nil
}

// started has nothing to admit: there is no cgroup on Windows.
func (g *guardedGroup) started(*exec.Cmd) error { return nil }

// Containment: a Windows command is contained as cmd.exe alone.
func Containment() (string, error) { return "process", nil }

// term has no graceful equivalent for cmd.exe: the grace period passes and kill follows.
func (g *guardedGroup) term() error { return nil }

func (g *guardedGroup) kill() error {
	if g.cmd.Process == nil {
		return nil
	}
	return g.cmd.Process.Kill()
}

// release kills cmd.exe for reap; there is no leaf whose cleanup could fail.
func (g *guardedGroup) release(reap bool) error {
	if reap {
		_ = g.kill()
	}
	return nil
}
