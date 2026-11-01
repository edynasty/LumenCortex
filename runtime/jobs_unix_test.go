//go:build !windows

package runtime

import "syscall"

// processRunning reports whether pid names a live process, the way the job manager checks a runner.
func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
