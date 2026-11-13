package debugger

import (
	"os"
	"testing"
)

// Adapter discovery validates a real executable without requiring a POSIX shell.
// ProgramCommand still refuses a Windows launch because no sandbox is supported.
func testExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
