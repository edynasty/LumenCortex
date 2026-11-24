package editing

import "syscall"

// Simulation only: the Windows/FreeBSD exchange primitive (always ENOSYS) on the host.
func init() { renameSwapForPublish = func(_, _ string) error { return syscall.ENOSYS } }
