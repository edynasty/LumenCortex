//go:build unix

package debugger

import (
	"os"
	"runtime"
	"testing"

	"github.com/edynasty/LumenCortex/evalruntime"
)

// testLLDB resolves the adapter the way the registry does, but lets a host name
// it explicitly. It skips instead of failing on a host without lldb-dap.
func testLLDB(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("LC_TEST_LLDB"); path != "" {
		return path
	}
	path, err := FindLLDB()
	if err != nil {
		t.Skipf("no lldb-dap on this host: %v", err)
	}
	return path
}

// testSandbox is the sandbox of the real-adapter tests. Linux runs programs
// only behind a caller-isolated bubblewrap, so the tests need one named
// explicitly rather than weakening the production gate.
func testSandbox(t *testing.T) evalruntime.SandboxOptions {
	t.Helper()
	if runtime.GOOS != "linux" {
		return evalruntime.SandboxOptions{}
	}
	path := os.Getenv("LC_TEST_SANDBOX_BWRAP")
	if path == "" {
		t.Skip("Linux DAP tests need LC_TEST_SANDBOX_BWRAP, a caller-isolated bubblewrap path; the program sandbox refuses an unverified caller")
	}
	return evalruntime.SandboxOptions{BubblewrapPath: path, RequireCallerIsolation: true}
}

// xcodeReadPaths is the Xcode surface LLDB needs read access to.
func xcodeReadPaths(path string) []string {
	if root := xcodeRoot(path); root != "" {
		return []string{root}
	}
	return nil
}
