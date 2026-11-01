//go:build !windows

package mcp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestHelperMCPServerWithChild is a stdio server that starts a long-lived child process before it
// answers the handshake, the way playwright-mcp starts its Chromium: the child is what a client leaks
// when it kills the server alone.
func TestHelperMCPServerWithChild(t *testing.T) {
	if os.Getenv("LCX_MCP_CHILD_HELPER") != "1" {
		return
	}
	child := exec.Command("sleep", "300")
	if err := child.Start(); err != nil {
		os.Exit(3)
	}
	if path := os.Getenv("LCX_MCP_CHILD_PID"); path != "" {
		_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
	}
	os.Setenv("LCX_MCP_HELPER", "1")
	TestHelperMCPServer(t)
}

// startChildHelper starts the helper server and returns the client and the pid of its child.
func startChildHelper(t *testing.T) (*Client, int) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("LCX_MCP_CHILD_HELPER", "1")
	t.Setenv("LCX_MCP_CHILD_PID", pidFile)
	t.Setenv("LCX_MCP_MODERN", "0")
	client, err := Start(context.Background(), Config{
		ID:           "helper-child",
		Name:         "Helper with a child",
		Command:      os.Args[0],
		Args:         []string{"-test.run=TestHelperMCPServerWithChild"},
		Workspace:    t.TempDir(),
		ProtocolMode: ModeLegacy,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		_ = client.Close()
		t.Fatalf("the helper did not record its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		_ = client.Close()
		t.Fatalf("child pid %q: %v", raw, err)
	}
	if !processAlive(pid) {
		_ = client.Close()
		t.Fatalf("the helper's child %d is not running", pid)
	}
	return client, pid
}

// TestMCPStdioServerExitKillsItsProcessGroup: a server that dies (a crash, here a SIGKILL) leaves the
// child it started behind unless its process group is killed when it goes.
func TestMCPStdioServerExitKillsItsProcessGroup(t *testing.T) {
	client, child := startChildHelper(t)
	defer func() { _ = client.Close() }()
	if err := client.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, child, 5*time.Second)
}

// TestMCPStdioCloseKillsItsProcessGroup: Close ends the server and the child it started, including a
// server that exits on its own after stdin closes and leaves the child running.
func TestMCPStdioCloseKillsItsProcessGroup(t *testing.T) {
	client, child := startChildHelper(t)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	waitProcessGone(t, child, 5*time.Second)
}

func processAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func waitProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("process %d outlived the mcp server", pid)
}
