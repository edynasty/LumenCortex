package editing

import "syscall"

// Test-only simulation: replace the Darwin/Linux capability declaration and exchange primitive.
// Create and delete primitives remain host implementations; this is not Windows/FreeBSD acceptance.
const platformExchangesExistingFiles = false

func init() {
	renameSwapForPublish = func(_, _ string) error { return syscall.ENOSYS }
}
