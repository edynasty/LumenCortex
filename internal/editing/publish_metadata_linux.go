//go:build linux

package editing

import "os"

// replicateSwapMetadata gives the staged file tmp (still empty) the owner, group and extended
// attributes of orig; POSIX ACLs are the system.posix_acl_* attributes and are copied with them.
// Anything it cannot reproduce exactly (another owner, a security label it may not set, an inherited
// default ACL the original lacks) is reported, and the update is refused before the destination is
// touched. Inode flags (chattr) and birth time are not reproduced.
func replicateSwapMetadata(orig, tmp *os.File, _ string) error {
	if err := replicateOwnership(orig, tmp); err != nil {
		return err
	}
	return copyXattrs(orig, tmp, nil)
}
