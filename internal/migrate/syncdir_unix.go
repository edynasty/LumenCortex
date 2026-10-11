//go:build unix

package migrate

import "os"

// syncDir fsyncs a directory so a created, renamed or removed entry survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
