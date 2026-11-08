//go:build !windows

package shell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
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

// guardScript is the crash guard of a shell command's process group. It is the group's leader and
// reads one line from the host: "done" releases it, anything else (end of file, which is what the
// kernel delivers when the host process dies, however it dies) kills the whole group. The kill
// addresses the guard's own group (kill 0) while the guard is a live member of it, so the group id
// cannot have been reused by an unrelated process. The group's own stop signals do not stop it.
const guardScript = `trap '' INT TERM HUP QUIT TSTP TTIN TTOU; read line; [ "$line" = done ] && exit 0; kill -KILL 0`

// guardedGroup is the process group of one shell command, led by a crash guard (guardScript), so the
// command and everything it started in its group are killed when the host dies while the command
// runs: a SIGKILLed or crashed LumenCortex never leaves a running foreground command behind.
type guardedGroup struct {
	pgid  int
	guard *exec.Cmd
	ctl   *os.File
}

// newGuardedGroup starts the guard and makes cmd join its group. The guard holds no descriptor of the
// command; the host's end of the control pipe is close-on-exec, so no command inherits it.
func newGuardedGroup(cmd *exec.Cmd) (*guardedGroup, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("shell crash guard: %w", err)
	}
	guard := exec.Command("/bin/sh", "-c", guardScript)
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
	g := &guardedGroup{pgid: guard.Process.Pid, guard: guard, ctl: w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: g.pgid}
	cmd.Cancel = g.kill
	return g, nil
}

// term asks the group to exit; the guard ignores it.
func (g *guardedGroup) term() error { return killGroup(g.pgid, syscall.SIGTERM) }

// kill stops the whole group, guard included.
func (g *guardedGroup) kill() error { return killGroup(g.pgid, syscall.SIGKILL) }

// release ends the guard once the command is over. With reap the group is killed first; without it,
// background processes the command detached keep running, as they would without a guard.
func (g *guardedGroup) release(reap bool) {
	if reap {
		_ = g.kill()
	} else {
		_, _ = g.ctl.Write([]byte("done\n"))
	}
	_ = g.ctl.Close()
	_ = g.guard.Wait()
}
