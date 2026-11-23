//go:build darwin || linux

package editing

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// errSwapMetadata reports metadata of an existing file that a staged swap cannot reproduce exactly.
// The update then keeps the destination inode instead of silently dropping that metadata.
func errSwapMetadata(format string, args ...any) error {
	return fmt.Errorf("staged swap cannot reproduce destination metadata: "+format, args...)
}

func listXattrNames(fd int) ([]string, error) {
	size, err := unix.Flistxattr(fd, nil)
	if err != nil || size == 0 {
		return nil, err
	}
	buf := make([]byte, size)
	n, err := unix.Flistxattr(fd, buf)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(string(buf[:n]), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

func getXattr(fd int, name string) ([]byte, error) {
	size, err := unix.Fgetxattr(fd, name, nil)
	if err != nil || size == 0 {
		return nil, err
	}
	buf := make([]byte, size)
	n, err := unix.Fgetxattr(fd, name, buf)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// copyXattrs copies every extended attribute of orig onto tmp and verifies the result. An attribute
// tmp carries that orig lacks is accepted only when allowExtra says the kernel adds it to every new
// file (for example com.apple.provenance).
func copyXattrs(orig, tmp *os.File, allowExtra func(string) bool) error {
	ofd, tfd := int(orig.Fd()), int(tmp.Fd())
	names, err := listXattrNames(ofd)
	if err != nil {
		return errSwapMetadata("list extended attributes: %v", err)
	}
	want := make(map[string][]byte, len(names))
	for _, name := range names {
		value, err := getXattr(ofd, name)
		if err != nil {
			return errSwapMetadata("read extended attribute %q: %v", name, err)
		}
		if err := unix.Fsetxattr(tfd, name, value, 0); err != nil {
			return errSwapMetadata("copy extended attribute %q: %v", name, err)
		}
		want[name] = value
	}
	got, err := listXattrNames(tfd)
	if err != nil {
		return errSwapMetadata("list staged extended attributes: %v", err)
	}
	for _, name := range got {
		value, ok := want[name]
		if !ok {
			if allowExtra != nil && allowExtra(name) {
				continue
			}
			return errSwapMetadata("staged file carries extra extended attribute %q", name)
		}
		staged, err := getXattr(tfd, name)
		if err != nil || !bytes.Equal(staged, value) {
			return errSwapMetadata("extended attribute %q was not reproduced", name)
		}
		delete(want, name)
	}
	if len(want) > 0 {
		return errSwapMetadata("%d extended attributes were not reproduced", len(want))
	}
	return nil
}

// replicateOwnership gives tmp the owner and group of orig, or reports that it cannot.
func replicateOwnership(orig, tmp *os.File) error {
	var ost, tst unix.Stat_t
	if err := unix.Fstat(int(orig.Fd()), &ost); err != nil {
		return errSwapMetadata("stat destination: %v", err)
	}
	if err := unix.Fstat(int(tmp.Fd()), &tst); err != nil {
		return errSwapMetadata("stat staged file: %v", err)
	}
	if ost.Uid != tst.Uid {
		return errSwapMetadata("owner %d differs from staged owner %d", ost.Uid, tst.Uid)
	}
	if ost.Gid != tst.Gid {
		if err := unix.Fchown(int(tmp.Fd()), -1, int(ost.Gid)); err != nil {
			return errSwapMetadata("group %d: %v", ost.Gid, err)
		}
	}
	return nil
}
