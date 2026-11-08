//go:build linux

package evalruntime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

func kernelSwitches(t *testing.T, userns, unconfined string) {
	t.Helper()
	dir := t.TempDir()
	for name, value := range map[string]string{"apparmor_restrict_unprivileged_userns": userns, "apparmor_restrict_unprivileged_unconfined": unconfined} {
		if value == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := procSysKernel
	procSysKernel = dir
	t.Cleanup(func() { procSysKernel = old })
}

// The gate accepts only a host where no other caller can borrow the user
// namespaces granted to the dedicated bwrap: both AppArmor switches on, and the
// file executable by its (non-root) group alone, which this process is in.
func TestCallerIsolationGate(t *testing.T) {
	if os.Getegid() == 0 {
		t.Skip("the fixture uses the test's own primary group, which must not be root")
	}
	file := filepath.Join(t.TempDir(), "bwrap")
	if err := os.WriteFile(file, []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		userns, unconfined string
		mode               os.FileMode
		want               string
	}{
		{name: "isolated", userns: "1", unconfined: "1", mode: 0o750},
		{name: "aa-exec borrowing open", userns: "1", unconfined: "0", mode: 0o750, want: "apparmor_restrict_unprivileged_unconfined=0"},
		{name: "userns unrestricted", userns: "0", unconfined: "1", mode: 0o750, want: "apparmor_restrict_unprivileged_userns=0"},
		{name: "switch missing", userns: "1", mode: 0o750, want: "apparmor_restrict_unprivileged_unconfined is unavailable"},
		{name: "executable by others", userns: "1", unconfined: "1", mode: 0o755, want: "executable by others"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kernelSwitches(t, tc.userns, tc.unconfined)
			if err := os.Chmod(file, tc.mode); err != nil {
				t.Fatal(err)
			}
			err := checkCallerIsolation(file)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("checkCallerIsolation = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkCallerIsolation = %v, want %q", err, tc.want)
			}
		})
	}
}

// The gate is enforced at New: without a dedicated path, or on a host that does
// not isolate callers, no runtime starts.
func TestRequireCallerIsolationFailsClosed(t *testing.T) {
	exe := requirePython(t)
	kernelSwitches(t, "1", "0")
	for _, path := range []string{"", "/usr/bin/bwrap"} {
		_, err := New(Options{Root: filepath.Join(t.TempDir(), "state"), Python: exe, Sandbox: SandboxOptions{BubblewrapPath: path, RequireCallerIsolation: true}})
		if !errors.Is(err, evalapi.ErrSandboxUnsupported) {
			t.Fatalf("BubblewrapPath %q: New = %v, want ErrSandboxUnsupported", path, err)
		}
	}
}

// On a host deployed for production (LC_ISOLATED_BWRAP names the dedicated,
// group-restricted bwrap; both AppArmor switches are on; the test runs with that
// group), the gate passes against the real kernel settings and a cell runs.
func TestRequireCallerIsolationRunsOnAnIsolatedHost(t *testing.T) {
	path := os.Getenv("LC_ISOLATED_BWRAP")
	if path == "" {
		t.Skip("LC_ISOLATED_BWRAP is not set: no production-configured bwrap on this host")
	}
	h, _ := newPythonHarness(t, func(opts *Options) {
		opts.Sandbox.BubblewrapPath = path
		opts.Sandbox.RequireCallerIsolation = true
	})
	session := h.create("python", "owner", "owner")
	result := h.expectCompleted(session, "owner", "open('/proc/self/attr/current').read().strip()")
	if len(result.Displays) == 0 || !strings.Contains(result.Displays[0].Text, "unconfined") {
		t.Fatalf("cell result %+v", result.Displays)
	}
	t.Logf("isolated host: bwrap=%s label=%s", path, result.Displays[0].Text)
}
