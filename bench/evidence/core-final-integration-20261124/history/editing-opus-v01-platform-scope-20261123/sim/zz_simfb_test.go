package editing

import "syscall"

// Simulation only: FreeBSD/other-Unix primitives (no exchange, no no-replace rename; link works).
func init() {
	renameSwapForPublish = func(_, _ string) error { return syscall.ENOSYS }
	renameNoReplaceForPublish = func(_, _ string) error { return syscall.ENOSYS }
	renameNoReplaceForQuarantine = func(_, _ string) error { return syscall.ENOSYS }
}
