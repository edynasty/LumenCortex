//go:build unix

package dap

import (
	"os"
	"os/exec"
	"runtime"
	"testing"

	"github.com/edynasty/LumenCortex/evalruntime"
)

// testLLDB resolves lldb-dap the way the Core registry does: an explicit host
// path first, xcrun on macOS, then PATH (including versioned names on Linux).
func testLLDB(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("LC_TEST_LLDB"); path != "" {
		return path
	}
	if runtime.GOOS == "darwin" {
		if out, err := exec.Command("xcrun", "--find", "lldb-dap").Output(); err == nil {
			if path := string(out); len(path) > 1 {
				return path[:len(path)-1]
			}
		}
	}
	for _, name := range []string{"lldb-dap", "lldb-dap-21", "lldb-dap-20", "lldb-dap-19", "lldb-dap-18", "lldb-dap-17", "lldb-dap-16", "lldb-dap-15", "lldb-dap-14"} {
		if path, err := exec.LookPath(name); err == nil {
			return path
		}
	}
	t.Skip("no lldb-dap on this host")
	return ""
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

// testReadPaths is the extra read surface the adapter needs inside the sandbox.
func testReadPaths(adapter string) []string {
	for _, marker := range []string{"/Contents/Developer/", "/usr/lib/"} {
		if i := indexOf(adapter, marker); i > 0 {
			return []string{adapter[:i]}
		}
	}
	return nil
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
