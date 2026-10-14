//go:build windows

package config

// syncDir is a no-op: Windows cannot flush a directory handle; NTFS journals directory metadata.
func syncDir(string) error { return nil }
