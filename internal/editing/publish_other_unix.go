//go:build !windows && !darwin && !linux

package editing

import (
	"os"
	"syscall"
)

func renameNoReplacePlatform(_, _ string) error {
	return syscall.ENOSYS
}

func renameSwapPlatform(_, _ string) error {
	return syscall.ENOSYS
}

type vnodeWriteWatch struct{}

func newVnodeWriteWatch(_ *os.File) (*vnodeWriteWatch, error) {
	return &vnodeWriteWatch{}, nil
}

func (*vnodeWriteWatch) wrote() bool { return false }

func (*vnodeWriteWatch) Close() error { return nil }
