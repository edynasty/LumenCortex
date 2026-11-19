//go:build !windows && !darwin && !linux

package editing

import "syscall"

func renameNoReplacePlatform(_, _ string) error {
	return syscall.ENOSYS
}
