//go:build !windows

package shell

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestCrashOwnerHelper is the host process of TestOwnerKillStopsTheRunningCommand: it runs one
// command and is SIGKILLed while the command runs.
func TestCrashOwnerHelper(t *testing.T) {
	dir := os.Getenv("LC_SHELL_CRASH_DIR")
	if dir == "" {
		t.Skip("helper process of TestOwnerKillStopsTheRunningCommand")
	}
	_, _ = New(dir).Run(context.Background(), `echo $$ > tool.pid; sleep 30 & echo $! > child.pid; trap '' TERM; wait`, nil)
}

// A host killed with SIGKILL while a command runs cannot clean up; the command's crash guard does. The
// running shell and the child it started are gone shortly after the host dies, and an unrelated process
// of the same user is untouched.
func TestOwnerKillStopsTheRunningCommand(t *testing.T) {
	dir := t.TempDir()
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()

	owner := exec.Command(os.Args[0], "-test.run=^TestCrashOwnerHelper$")
	owner.Env = append(os.Environ(), "LC_SHELL_CRASH_DIR="+dir)
	owner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = owner.Process.Kill(); _ = owner.Wait() }()
	waitFor(t, filepath.Join(dir, "child.pid"))
	tool := readPID(t, filepath.Join(dir, "tool.pid"))
	child := readPID(t, filepath.Join(dir, "child.pid"))
	t.Cleanup(func() {
		_ = syscall.Kill(tool, syscall.SIGKILL)
		_ = syscall.Kill(child, syscall.SIGKILL)
	})
	if !alive(tool) || !alive(child) {
		t.Fatalf("command not running before the owner kill: tool=%v child=%v", alive(tool), alive(child))
	}

	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for alive(tool) || alive(child) {
		if time.Now().After(deadline) {
			t.Fatalf("after the owner's SIGKILL: tool shell alive=%v, its child alive=%v", alive(tool), alive(child))
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !alive(other.Process.Pid) {
		t.Fatal("the crash guard killed an unrelated process")
	}
}
