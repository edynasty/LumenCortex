//go:build !windows

package evalruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// checkTrustedExecutable accepts path only if no one but root can change what
// it runs: an absolute, non-symlink, executable regular file owned by root and
// writable by neither group nor others, in directories that are likewise owned by
// root and writable by no one else.
func checkTrustedExecutable(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("%q is not a clean absolute path", path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	if err := rootOwnedNotShared(path, info); err != nil {
		return err
	}
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		if err := rootOwnedNotShared(dir, info); err != nil {
			return err
		}
		if dir == "/" {
			return nil
		}
	}
}

func rootOwnedNotShared(path string, info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("file ownership is unavailable")
	}
	if stat.Uid != 0 {
		return fmt.Errorf("%s is owned by uid %d, not root", path, stat.Uid)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (%v)", path, info.Mode().Perm())
	}
	return nil
}
