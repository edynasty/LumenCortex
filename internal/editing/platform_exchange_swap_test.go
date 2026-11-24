//go:build darwin || linux

package editing

// platformExchangesExistingFiles: renameSwapPlatform is renameatx_np(RENAME_SWAP) on darwin and
// renameat2(RENAME_EXCHANGE) on linux.
const platformExchangesExistingFiles = true
