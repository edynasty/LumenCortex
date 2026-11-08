//go:build !linux

package jobs

// These backends retain their documented process-group/process-only contract;
// they do not offer Linux /proc membership observation.
func waitStoppedGroup(int) error { return nil }
