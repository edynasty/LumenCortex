//go:build !linux

package evalruntime

import "errors"

// checkCallerIsolation is a Linux bubblewrap requirement; no other platform
// reaches it.
func checkCallerIsolation(string) error {
	return errors.New("caller isolation is checked for the Linux bubblewrap backend only")
}
