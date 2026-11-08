//go:build windows

package evalruntime

import "errors"

func checkTrustedExecutable(string) error {
	return errors.New("no trusted executable check on windows")
}
