//go:build darwin

package editing

import "golang.org/x/sys/unix"

func renameNoReplacePlatform(tmpPath, dstPath string) error {
	return unix.RenameatxNp(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, dstPath, unix.RENAME_EXCL)
}
