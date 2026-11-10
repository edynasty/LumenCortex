//go:build !windows

package procrt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// ProcessAlive reports whether pid is a live (or zombie) process on this host.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// signalGroup signals the process group led by pid when it has one, otherwise
// the process itself. A launcher that starts a process in its own group gets
// whole-tree delivery; one that does not still gets a best-effort stop.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return nil
	}
	return syscall.Kill(pid, sig)
}

// CmdHandle supervises a plain *exec.Cmd that a subsystem started itself. It is
// the one-line hook a launcher registers when its process is not an eval kernel
// — for example the DAP adapter launcher:
//
//	unregister := procrt.RegisterCommand(procrt.Owner{SessionID: sid, Component: procrt.ComponentDAP, ObjectID: debugID}, cmd)
//	defer unregister()
//
// It reports no resource enforcement (Enforcement zero value → none): the
// adapter runs under whatever limits its launcher applied, which this handle
// cannot verify, so it never claims one.
type CmdHandle struct {
	cmd       *exec.Cmd
	interrupt os.Signal
	done      chan struct{}
	once      sync.Once
}

// RegisterCommand registers an already-started command with the default
// registry and returns an idempotent unregister function. The wait goroutine
// the launcher already runs must call Handle.Reaped (or Done is closed by the
// first WaitHandle call): this helper starts its own lightweight waiter that
// does not call Wait (the caller owns reaping).
func RegisterCommand(owner Owner, cmd *exec.Cmd) func() {
	h := NewCmdHandle(cmd)
	return Default().Register(owner, h)
}

// NewCmdHandle builds a handle for an already-started command.
func NewCmdHandle(cmd *exec.Cmd) *CmdHandle {
	return &CmdHandle{cmd: cmd, interrupt: os.Interrupt, done: make(chan struct{})}
}

// PID is the command's process id.
func (h *CmdHandle) PID() int {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// TreePIDs is empty: this handle cannot enumerate descendants, and an empty
// list is not presented as proof that none exist.
func (h *CmdHandle) TreePIDs() []int { return nil }

// State reports running or exited from the process itself.
func (h *CmdHandle) State() string {
	pid := h.PID()
	if pid == 0 || !ProcessAlive(pid) {
		h.markDone()
		return StateExited
	}
	return StateRunning
}

// Usage reports nothing: this handle measures no CPU or memory.
func (h *CmdHandle) Usage() Usage { return Usage{} }

// Enforcement reports no bound, with the reason.
func (h *CmdHandle) Enforcement() Enforcement {
	return Enforcement{Memory: None, CPU: None, Disk: None,
		Notes: []string{"not reported: this handle cannot verify the limits its launcher applied"}}
}

// Interrupt sends SIGINT to the command's group.
func (h *CmdHandle) Interrupt() error {
	if pid := h.PID(); pid > 0 {
		return signalGroup(pid, syscall.SIGINT)
	}
	return nil
}

// Terminate sends SIGTERM to the group and returns the pids still alive after a
// short grace period.
func (h *CmdHandle) Terminate(ctx context.Context) []int {
	if pid := h.PID(); pid > 0 {
		_ = signalGroup(pid, syscall.SIGTERM)
	}
	timer := time.NewTimer(termGrace)
	defer timer.Stop()
	select {
	case <-h.Done():
	case <-timer.C:
	case <-ctx.Done():
	}
	return h.Alive()
}

// Kill sends SIGKILL to the group.
func (h *CmdHandle) Kill() []int {
	if pid := h.PID(); pid > 0 {
		_ = signalGroup(pid, syscall.SIGKILL)
	}
	return h.Alive()
}

// Alive lists the pids this handle can still observe.
func (h *CmdHandle) Alive() []int {
	pid := h.PID()
	if pid > 0 && ProcessAlive(pid) {
		return []int{pid}
	}
	h.markDone()
	return nil
}

// Done is closed once the process is observed gone.
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

// Reaped lets the launcher's own Wait report the exit, so Done closes without a
// second poller.
func (h *CmdHandle) Reaped() { h.markDone() }

func (h *CmdHandle) markDone() {
	if h == nil {
		return
	}
	h.once.Do(func() { close(h.done) })
}

const termGrace = 2 * time.Second
