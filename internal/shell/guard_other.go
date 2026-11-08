//go:build !linux && !windows

package shell

import "errors"

// waitExitedNoReap is only needed to supervise a guard of a cgroup leaf, which exists on Linux only.
func waitExitedNoReap(int) error { return errors.New("guard supervision needs Linux") }
