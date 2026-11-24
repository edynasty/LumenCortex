package editing

import "syscall"

// Independent-review scratch: no-exchange capability and exchange primitive returning ENOSYS.
const platformExchangesExistingFiles = false

func init() { renameSwapForPublish = func(_, _ string) error { return syscall.ENOSYS } }
