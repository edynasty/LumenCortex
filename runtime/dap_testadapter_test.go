//go:build unix

package runtime

import (
	"os"
	"runtime"
	"testing"

	"github.com/edynasty/LumenCortex/evalruntime"
	"github.com/edynasty/LumenCortex/internal/debugger"
)

// testLLDB resolves lldb-dap via the Core registry, honoring an explicit host
// path. A host without it skips rather than fails.
func testLLDB(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("LC_TEST_LLDB"); path != "" {
		return path
	}
	path, err := debugger.FindLLDB()
	if err != nil {
		t.Skipf("no lldb-dap on this host: %v", err)
	}
	return path
}

// testSandbox is the production Linux gate; macOS uses seatbelt automatically.
func testSandbox(t *testing.T) evalruntime.SandboxOptions {
	t.Helper()
	if runtime.GOOS != "linux" {
		return evalruntime.SandboxOptions{}
	}
	path := os.Getenv("LC_TEST_SANDBOX_BWRAP")
	if path == "" {
		t.Skip("Linux DAP tests need LC_TEST_SANDBOX_BWRAP, a caller-isolated bubblewrap path")
	}
	return evalruntime.SandboxOptions{BubblewrapPath: path, RequireCallerIsolation: true}
}

// testReadPaths is the Xcode read surface lldb-dap needs on macOS.
func testReadPaths(adapter string) []string {
	for _, marker := range []string{"/Contents/Developer/", "/usr/lib/"} {
		for i := 1; i+len(marker) <= len(adapter); i++ {
			if adapter[i:i+len(marker)] == marker {
				return []string{adapter[:i]}
			}
		}
	}
	return nil
}
