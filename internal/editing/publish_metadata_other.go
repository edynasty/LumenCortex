//go:build !darwin && !linux

package editing

import "os"

// replicateSwapMetadata reproduces nothing beyond the mode the caller applies. Windows ACLs,
// alternate data streams and attributes, and FreeBSD extended attributes are not carried across a
// replacement on these platforms; their behavior is unverified natively.
func replicateSwapMetadata(_, _ *os.File, _ string) error {
	return nil
}
