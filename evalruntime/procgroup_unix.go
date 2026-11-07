//go:build !windows

package evalruntime

import (
	"os/exec"
	"syscall"
)

// startProcessGroup makes the worker lead its own process group. The manager
// signals that group directly (signalGroup), so no exec.Cmd.Cancel is set: a
// Cancel requires CommandContext, and this process is not driven by a context.
func startProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
