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

// TestRegistryCloseStopsServersTogether: Close stops every server at once (RT-exec-9). Each of these
// servers is a shell that sleeps on with stdout open once the helper behind it exits at stdin EOF, so
// each Client.Close waits its full closeGrace: one after another, Close would take closeGrace per
// server; together they share one grace.
func TestRegistryCloseStopsServersTogether(t *testing.T) {
	root := t.TempDir()
	registry, err := NewRegistry(root, filepath.Join(root, ".lumencortex", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	trustedProject(registry, true)
	t.Setenv("LCX_MCP_HELPER", "1")
	t.Setenv("LCX_MCP_MODERN", "0")
	const servers = 4
	for i := range servers {
		cfg := Config{
			ID:           "lingering" + strconv.Itoa(i),
			Command:      "/bin/sh",
			Args:         []string{"-c", `"$0" -test.run=TestHelperMCPServer; sleep 30`, os.Args[0]},
			ProtocolMode: ModeLegacy,
		}
		if err := registry.Upsert(cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := registry.Start(context.Background(), cfg.ID, root); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	if err := registry.Close(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took >= 2*closeGrace {
		t.Fatalf("Registry.Close of %d servers took %s, want them to share one %s grace", servers, took, closeGrace)
	}
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
