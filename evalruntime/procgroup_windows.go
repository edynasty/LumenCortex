//go:build windows

package evalruntime

import "os/exec"

// startProcessGroup is a no-op on Windows, where the manager refuses to run at
// all (no supported sandbox backend).
func startProcessGroup(cmd *exec.Cmd) {}
