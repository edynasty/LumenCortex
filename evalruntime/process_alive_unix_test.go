//go:build !windows

package evalruntime

import "syscall"

// Linux sandbox PIDs must first be mapped through hostProcessOf.
func processAlive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
