//go:build unix

package debugger

import (
	"os"
	"path/filepath"
	"testing"
)

// testExecutable is an absolute executable path for adapter validation.
func testExecutable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "adapter")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
