//go:build linux

package editing

import (
	"os"

	"golang.org/x/sys/unix"
)

func renameNoReplacePlatform(tmpPath, dstPath string) error {
	return unix.Renameat2(unix.AT_FDCWD, tmpPath, unix.AT_FDCWD, dstPath, unix.RENAME_NOREPLACE)
}

func renameSwapPlatform(srcPath, dstPath string) error {
	return unix.Renameat2(unix.AT_FDCWD, srcPath, unix.AT_FDCWD, dstPath, unix.RENAME_EXCHANGE)
}

type vnodeWriteWatch struct{}

func newVnodeWriteWatch(_ *os.File) (*vnodeWriteWatch, error) {
	return &vnodeWriteWatch{}, nil
}

func (*vnodeWriteWatch) wrote() bool { return false }

func (*vnodeWriteWatch) Close() error { return nil }
