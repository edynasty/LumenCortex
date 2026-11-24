package editing

import "syscall"

// Simulation only: the Windows/FreeBSD exchange primitive (renameSwapPlatform returns ENOSYS).
func init() { renameSwapForPublish = func(_, _ string) error { return syscall.ENOSYS } }
