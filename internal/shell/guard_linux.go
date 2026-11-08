//go:build linux

package shell

import "golang.org/x/sys/unix"

// waitExitedNoReap blocks until the child pid has exited and leaves it a zombie, so its pid stays
// reserved until the host reaps it.
func waitExitedNoReap(pid int) error {
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err != unix.EINTR {
			return err
		}
	}
}
