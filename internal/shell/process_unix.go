//go:build !windows

package shell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"

	"github.com/edynasty/LumenCortex/internal/cgroupv2"
)

// StartGroup makes the command lead its own process group, so a kill reaches every process it started,
// and makes cancellation kill that group instead of the command alone. The stdio MCP client uses it: a
// command's own children (a browser under an MCP server) outlive it otherwise.
func StartGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return KillGroup(cmd) }
}

// KillGroup sends SIGKILL to the command's process group. A group that is already gone is not an error.
func KillGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return killGroup(cmd.Process.Pid, syscall.SIGKILL)
}

func killGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// Lifetime contract of a shell command (Runner):
//
//   - While the command runs, everything it starts belongs to it - including processes that start a
//     new session or process group (setsid, double fork). Cancellation, a timeout, Reap, and the death
//     of the owning process, however it dies, stop all of it.
//   - When the command ends on its own and Reap is false, what it left running in the background
//     (`server > log &`) keeps running, but only for as long as the owning process lives.
//
// On Linux with a delegated cgroup v2 subtree (a systemd user manager) the contract is kernel-enforced:
// every command runs in its own cgroup leaf, admitted before any of its code runs, and cgroup.kill
// reaches every member no matter its session or group. Two independent processes kill the leaf when the
// owner dies: the command's crash guard and the pool's keeper (see cgroupv2), so the death of one of
// them before the owner's does not leave the command behind. Elsewhere (macOS, a Linux without a user
// manager such as a container) the command is contained by its process group only: a process that
// leaves the group is not followed. Containment reports which one applies.

// shellCgroups is this process's pool of command leaves, created on first use and kept for the life of
// the process; when the process ends, the pool's keeper kills what is left in it.
var shellCgroups = sync.OnceValues(func() (*cgroupv2.Pool, error) {
	return cgroupv2.NewPool(cgroupv2.Options{Unit: "lc-shell", LeafPrefix: "c"})
})

// Containment names how shell commands are contained in this process: "cgroup" when every command
// gets its own cgroup leaf, otherwise "process-group" and the reason cgroups are unavailable.
func Containment() (string, error) {
	if _, err := shellCgroups(); err != nil {
		return "process-group", err
	}
	return "cgroup", nil
}

// released holds the leaves of commands that ended while background processes they started still run.
// A leaf is removed once it is empty; whatever remains when the owner dies is killed by the keeper.
var released struct {
	sync.Mutex
	leaves []*cgroupv2.Leaf
}

func sweepReleased() {
	released.Lock()
	defer released.Unlock()
	released.leaves = slices.DeleteFunc(released.leaves, (*cgroupv2.Leaf).RemoveIfEmpty)
}

// guardScript is the crash guard of a shell command. It leads the command's process group and reads one
// line from the host: "done" releases it, anything else (end of file, which is what the kernel delivers
// when the host process dies, however it dies) kills the command's cgroup leaf ($1), when there is one,
// and the whole process group. The group kill addresses the guard's own group (kill 0) while the guard
// is a live member of it, so the group id cannot have been reused by an unrelated process. The group's
// own stop signals do not stop it.
const guardScript = `trap '' INT TERM HUP QUIT TSTP TTIN TTOU; read line; [ "$line" = done ] && exit 0; [ -n "$1" ] && echo 1 >"$1/cgroup.kill"; kill -KILL 0`

// gateScript holds a command until the host has moved it into its cgroup leaf: it waits for one line on
// descriptor 3, closes it, and execs the command, which keeps the pid the host admitted.
const gateScript = `read _ <&3 || exit 126; exec 3<&-; exec "$@"`

// guardedGroup is the process group (and, on Linux, the cgroup leaf) of one shell command, led by a crash
// guard (guardScript).
type guardedGroup struct {
	pgid  int
	guard *exec.Cmd
	ctl   *os.File
	leaf  *cgroupv2.Leaf
	gate  *os.File // host end of the admission gate, until the command is released
	gateR *os.File
}

// newGuardedGroup prepares the containment of cmd before it starts: the cgroup leaf when cgroups are
// available, the guard, and cmd's membership in the guard's process group. With a leaf, cmd is wrapped
// in gateScript so that none of its code runs before started has admitted it. The guard holds no
// descriptor of the command; the host's ends of the control and gate pipes are close-on-exec, so no
// command inherits them.
func newGuardedGroup(cmd *exec.Cmd) (g *guardedGroup, err error) {
	sweepReleased()
	g = &guardedGroup{}
	defer func() {
		if err != nil {
			g.discard()
		}
	}()
	leafDir := ""
	if pool, poolErr := shellCgroups(); poolErr == nil {
		if g.leaf, err = pool.NewLeaf(nil); err != nil {
			return nil, fmt.Errorf("shell command cgroup: %w", err)
		}
		leafDir = g.leaf.Dir()
		if g.gateR, g.gate, err = os.Pipe(); err != nil {
			return nil, fmt.Errorf("shell admission gate: %w", err)
		}
		cmd.Args = append([]string{"/bin/sh", "-c", gateScript, "lc-shell-gate", cmd.Path}, cmd.Args[1:]...)
		cmd.Path = "/bin/sh"
		cmd.ExtraFiles = []*os.File{g.gateR}
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("shell crash guard: %w", err)
	}
	guard := exec.Command("/bin/sh", "-c", guardScript, "lc-shell-guard", leafDir)
	guard.Stdin = r
	guard.Dir = "/"
	guard.Env = []string{"PATH=/usr/bin:/bin"}
	guard.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	err = guard.Start()
	_ = r.Close()
	if err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("shell crash guard: %w", err)
	}
	g.pgid, g.guard, g.ctl = guard.Process.Pid, guard, w
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: g.pgid}
	cmd.Cancel = g.kill
	return g, nil
}

// started admits the just-started command into its leaf and lets it run. When admission fails the
// command is killed, never released: it has run none of its code.
func (g *guardedGroup) started(cmd *exec.Cmd) error {
	if g.leaf == nil {
		return nil
	}
	_ = g.gateR.Close()
	g.gateR = nil
	err := g.leaf.Admit(cmd.Process.Pid)
	if err == nil {
		_, err = g.gate.Write([]byte("go\n"))
	}
	_ = g.gate.Close()
	g.gate = nil
	if err != nil {
		_ = g.kill()
		return fmt.Errorf("shell command cgroup admission: %w", err)
	}
	return nil
}

// term asks the group to exit; the guard ignores it.
func (g *guardedGroup) term() error { return killGroup(g.pgid, syscall.SIGTERM) }

// kill stops the command: every member of its leaf, wherever it moved, and the whole group, guard
// included.
func (g *guardedGroup) kill() error {
	var err error
	if g.leaf != nil {
		err = g.leaf.Kill()
	}
	return errors.Join(err, killGroup(g.pgid, syscall.SIGKILL))
}

// release ends the guard once the command is over (or never started). With reap everything the command
// started is killed first; without it, background processes the command left keep running until they
// end or the owner dies, and their leaf is removed once it is empty.
func (g *guardedGroup) release(reap bool) {
	g.closeGate()
	if reap {
		_ = g.kill()
	} else {
		_, _ = g.ctl.Write([]byte("done\n"))
	}
	_ = g.ctl.Close()
	_ = g.guard.Wait()
	if g.leaf == nil {
		return
	}
	if reap {
		// Remove waits for the killed leaf to empty; a leaf that does not is kept for the sweep.
		if g.leaf.Remove() == nil {
			return
		}
	} else if g.leaf.RemoveIfEmpty() {
		return
	}
	released.Lock()
	released.leaves = append(released.leaves, g.leaf)
	released.Unlock()
}

// closeGate closes what is left of the admission gate: a command that never started keeps both ends.
func (g *guardedGroup) closeGate() {
	for _, f := range []*os.File{g.gate, g.gateR} {
		if f != nil {
			_ = f.Close()
		}
	}
	g.gate, g.gateR = nil, nil
}

// discard undoes a group whose guard never started or failed to start.
func (g *guardedGroup) discard() {
	g.closeGate()
	if g.ctl != nil {
		_ = g.ctl.Close()
	}
	if g.guard != nil {
		_ = g.guard.Wait()
	}
	if g.leaf != nil {
		_ = g.leaf.Remove()
	}
}
