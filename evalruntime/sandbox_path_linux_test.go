//go:build linux

package evalruntime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

// A host-named bwrap that someone other than root could replace is refused at
// New: the sandbox never runs through an executable its user can swap.
func TestLinuxBubblewrapPathMustBeRootControlled(t *testing.T) {
	exe := requirePython(t)
	userCopy := filepath.Join(t.TempDir(), "bwrap")
	raw, err := os.ReadFile("/usr/bin/bwrap")
	if err != nil {
		t.Skipf("no /usr/bin/bwrap to copy: %v", err)
	}
	if err := os.WriteFile(userCopy, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{userCopy, "bwrap", "/usr/bin/../bin/bwrap", "/usr/bin/no-such-bwrap"} {
		_, err := New(Options{Root: filepath.Join(t.TempDir(), "state"), Python: exe, Sandbox: SandboxOptions{BubblewrapPath: path}})
		if !errors.Is(err, evalapi.ErrSandboxUnsupported) {
			t.Fatalf("BubblewrapPath %q: New = %v, want ErrSandboxUnsupported", path, err)
		}
	}
}

// A root-controlled bwrap named by the host is the one that runs the worker.
func TestLinuxBubblewrapPathRunsTheNamedBinary(t *testing.T) {
	if _, err := os.Stat("/usr/bin/bwrap"); err != nil {
		t.Skipf("no /usr/bin/bwrap: %v", err)
	}
	h, _ := newPythonHarness(t, func(opts *Options) { opts.Sandbox.BubblewrapPath = "/usr/bin/bwrap" })
	if h.m.launcher.exe != "/usr/bin/bwrap" {
		t.Fatalf("launcher runs %q", h.m.launcher.exe)
	}
	session := h.create("python", "owner", "owner")
	h.expectCompleted(session, "owner", "1 + 1")
}
