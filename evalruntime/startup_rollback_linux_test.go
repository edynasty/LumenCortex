//go:build linux

package evalruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/edynasty/LumenCortex/evalapi"
)

// openFDs counts the test process's open descriptors.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read fds: %v", err)
	}
	return len(entries)
}

// poolProcs lists every process still inside the pool's worker leaves.
func poolProcs(t *testing.T, m *Manager) []string {
	t.Helper()
	var out []string
	for _, dir := range m.cgroups.liveLeaves() {
		text, err := readCgroupFile(dir, "cgroup.procs")
		if err == nil && text != "" {
			out = append(out, filepath.Base(dir)+":"+text)
		}
	}
	return out
}

// A worker start that fails at any stage leaves nothing behind before the error
// is returned: no worker cgroup, no process in one, no host descriptor. The
// session stays usable, and the next start is bounded like any other.
func TestLinuxWorkerStartFailureRollsBack(t *testing.T) {
	stages := []string{
		"cgroup",
		"pipe:control-in", "pipe:control-out", "pipe:stdout", "pipe:stderr", "pipe:gate",
		"start", "admit", "release",
		"group", "initialize", "handshake",
	}
	for _, stage := range stages {
		t.Run(stage, func(t *testing.T) {
			h, _ := newPythonHarness(t, func(opts *Options) { opts.Budget.MemoryBytes = 128 << 20 })
			// One complete worker lifecycle first, so the runtime's own lazily
			// opened descriptors (the poller) are part of the baseline.
			warm := h.create("python", "owner", "owner")
			h.expectCompleted(warm, "owner", "1 + 1")
			if err := h.m.Close(context.Background(), warm.ID, "owner"); err != nil {
				t.Fatalf("Close warm-up session: %v", err)
			}
			baseline := openFDs(t)
			baseTargets := fdTargets()
			injected := errors.New("injected failure at " + stage)
			hits := 0
			inject := func() {
				startFault = func(at string) error {
					if at == stage {
						hits++
						return injected
					}
					return nil
				}
			}
			assertRolledBack := func(when string, err error) {
				t.Helper()
				startFault = nil
				if hits != 1 {
					t.Fatalf("%s: stage %s was reached %d times", when, stage, hits)
				}
				hits = 0
				if !errors.Is(err, injected) && !strings.Contains(fmt.Sprint(err), injected.Error()) {
					t.Fatalf("%s: error = %v, want the injected failure", when, err)
				}
				if leaves := h.m.cgroups.liveLeaves(); len(leaves) != 0 {
					t.Fatalf("%s: worker cgroups after the failed start = %v", when, leaves)
				}
				if procs := poolProcs(t, h.m); len(procs) != 0 {
					t.Fatalf("%s: processes after the failed start = %v", when, procs)
				}
				if got := openFDs(t); got > baseline {
					t.Fatalf("%s: open descriptors grew from %d to %d after the failed start: before %v after %v", when, baseline, got, baseTargets, fdTargets())
				}
			}

			// A first start that fails refuses the session and leaves nothing.
			inject()
			_, err := h.m.Create(context.Background(), evalapi.CreateOptions{
				RuntimeID: "python", OwnerID: "owner", AgentID: "owner", Workspace: h.workspace, Permission: "workspace",
			})
			assertRolledBack("create", err)

			// A restart that fails (here: Reset) leaves nothing either, and the
			// session's next cell starts a fresh, again-bounded worker.
			session := h.create("python", "owner", "owner")
			baseline = openFDs(t)
			baseTargets = fdTargets()
			inject()
			assertRolledBack("reset", h.m.Reset(context.Background(), session.ID, "owner"))
			h.expectCompleted(session, "owner", "1 + 1")
			if got := readCgroupValue(t, workerCgroup(t, h.m, session.ID), "memory.max"); got != "134217728" {
				t.Fatalf("worker after recovery has memory.max %s", got)
			}
			if leaves := h.m.cgroups.liveLeaves(); len(leaves) != 1 {
				t.Fatalf("worker cgroups after recovery = %v", leaves)
			}
			if err := h.m.Dispose(context.Background()); err != nil {
				t.Fatalf("Dispose: %v", err)
			}
			if leaves := h.m.cgroups.liveLeaves(); len(leaves) != 0 {
				t.Fatalf("worker cgroups after Dispose = %v", leaves)
			}
		})
	}
}

// A real descriptor exhaustion (pipe2 fails with EMFILE) is rolled back like an
// injected one: the leaf created for the worker is removed before the error.
func TestLinuxWorkerStartDescriptorExhaustionRemovesLeaf(t *testing.T) {
	h, _ := newPythonHarness(t, nil)
	leaf, err := h.m.cgroups.newLeaf(128 << 20)
	if err != nil {
		t.Fatalf("newLeaf: %v", err)
	}
	var before syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &before); err != nil {
		t.Fatalf("getrlimit: %v", err)
	}
	low := before
	low.Cur = 3
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &low); err != nil {
		t.Fatalf("setrlimit: %v", err)
	}
	_, startErr := startProcess(procSpec{cmd: exec.Command("/bin/true"), cgroup: leaf})
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &before); err != nil {
		t.Fatalf("restore rlimit: %v", err)
	}
	if !errors.Is(startErr, syscall.EMFILE) {
		t.Fatalf("start error = %v, want EMFILE", startErr)
	}
	if _, err := os.Stat(leaf.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed start left its cgroup %s (stat: %v)", leaf.dir, err)
	}
	if leaves := h.m.cgroups.liveLeaves(); len(leaves) != 0 {
		t.Fatalf("registered leaves after the failed start = %v", leaves)
	}
}

// fdTargets names the test process's open descriptors, for failure messages.
func fdTargets() []string {
	entries, _ := os.ReadDir("/proc/self/fd")
	var out []string
	for _, entry := range entries {
		target, _ := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		out = append(out, entry.Name()+"="+target)
	}
	return out
}
