//go:build windows

package migrate

// syncDir is a no-op: Windows cannot flush a directory handle. NTFS journals directory metadata, and
// file contents are flushed through the file handle.
func syncDir(string) error { return nil }
