//go:build windows

package procrt

import (
	"context"
	"os"
	"os/exec"
	"sync"
	"time"
)

// ProcessAlive reports whether pid names a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p
	return true
}

// CmdHandle supervises a plain *exec.Cmd on Windows. See command_unix.go; the
// escalation is the same, the signals are not.
type CmdHandle struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
}

// RegisterCommand registers an already-started command with the default
// registry.
func RegisterCommand(owner Owner, cmd *exec.Cmd) func() {
	return Default().Register(owner, NewCmdHandle(cmd))
}

// NewCmdHandle builds a handle for an already-started command.
func NewCmdHandle(cmd *exec.Cmd) *CmdHandle {
	return &CmdHandle{cmd: cmd, done: make(chan struct{})}
}

func (h *CmdHandle) PID() int {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

func (h *CmdHandle) TreePIDs() []int { return nil }

func (h *CmdHandle) State() string {
	if h.PID() == 0 || !ProcessAlive(h.PID()) {
		h.markDone()
		return StateExited
	}
	return StateRunning
}

func (h *CmdHandle) Usage() Usage { return Usage{} }

func (h *CmdHandle) Enforcement() Enforcement {
	return Enforcement{Memory: None, CPU: None, Disk: None,
		Notes: []string{"not reported: this handle cannot verify the limits its launcher applied"}}
}

func (h *CmdHandle) Interrupt() error { return nil }

func (h *CmdHandle) Terminate(ctx context.Context) []int {
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	return h.Alive()
}

func (h *CmdHandle) Kill() []int {
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
	}
	time.Sleep(10 * time.Millisecond)
	return h.Alive()
}

func (h *CmdHandle) Alive() []int {
	pid := h.PID()
	if pid > 0 && ProcessAlive(pid) {
		return []int{pid}
	}
	h.markDone()
	return nil
}

func (h *CmdHandle) Done() <-chan struct{} {
	if h == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	if h.State() == StateExited {
		h.markDone()
	}
	return h.done
}

func (h *CmdHandle) Reaped() { h.markDone() }

func (h *CmdHandle) markDone() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.done) })
}
