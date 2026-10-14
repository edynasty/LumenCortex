//go:build unix

package config

import "os"

// syncDir fsyncs a directory so a renamed entry survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
