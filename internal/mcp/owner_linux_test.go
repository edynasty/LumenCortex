//go:build linux

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/shell"
)

func TestMCPOwnerCrashHelper(t *testing.T) {
	dir := os.Getenv("LC_FINAL_MCP_OWNER")
	if dir == "" {
		return
	}
	os.Setenv("LCX_MCP_CHILD_HELPER", "1")
	os.Setenv("LCX_MCP_CHILD_PID", filepath.Join(dir, "child"))
	os.Setenv("LCX_MCP_DETACHED", "1")
	c, err := Start(context.Background(), Config{ID: "crash", Command: os.Args[0], Args: []string{"-test.run=^TestHelperMCPServerWithChild$"}, Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c.Status())
	if err := os.WriteFile(filepath.Join(dir, "ready"), b, 0600); err != nil {
		t.Fatal(err)
	}
	select {}
}

type ownedMCP struct {
	cmd           *exec.Cmd
	server, child int
	leaf          string
}

func startMCPOwner(t *testing.T) ownedMCP {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestMCPOwnerCrashHelper$")
	cmd.Env = append(os.Environ(), "LC_FINAL_MCP_OWNER="+dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	var status Status
	for {
		b, err := os.ReadFile(filepath.Join(dir, "ready"))
		if err == nil {
			if err = json.Unmarshal(b, &status); err != nil {
				t.Fatal(err)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("owner handshake not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, err := os.ReadFile(filepath.Join(dir, "child"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", child))
	if err != nil {
		t.Fatal(err)
	}
	leaf := "/sys/fs/cgroup" + strings.TrimPrefix(strings.TrimSpace(string(raw)), "0::")
	if !mcpLinuxLive(child) || !mcpLinuxLive(status.PID) {
		t.Fatal("server tree not live before crash")
	}
	t.Cleanup(func() {
		if mcpLinuxLive(child) {
			syscall.Kill(child, syscall.SIGKILL)
		}
		if mcpLinuxLive(status.PID) {
			syscall.Kill(status.PID, syscall.SIGKILL)
		}
	})
	return ownedMCP{cmd: cmd, server: status.PID, child: child, leaf: leaf}
}
func mcpLinuxLive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return false
	}
	f := strings.Fields(string(b[end+1:]))
	return len(f) > 0 && f[0] != "Z" && f[0] != "X"
}
func TestMCPDetachedTreeDiesWithOwnerAndKeepsOtherOwner(t *testing.T) {
	if kind, err := shell.Containment(); kind != "cgroup" {
		t.Skipf("needs cgroup: %s %v", kind, err)
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(sig.String(), func(t *testing.T) {
			a, b := startMCPOwner(t), startMCPOwner(t)
			if a.leaf == b.leaf {
				t.Fatal("owners share leaf")
			}
			if err := a.cmd.Process.Signal(sig); err != nil {
				t.Fatal(err)
			}
			a.cmd.Wait()
			deadline := time.Now().Add(5 * time.Second)
			for (mcpLinuxLive(a.server) || mcpLinuxLive(a.child)) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if mcpLinuxLive(a.server) || mcpLinuxLive(a.child) {
				t.Fatalf("owner exited; server=%d child=%d still live", a.server, a.child)
			}
			if !mcpLinuxLive(b.server) || !mcpLinuxLive(b.child) {
				t.Fatal("unrelated owner was killed")
			}
			b.cmd.Process.Kill()
			b.cmd.Wait()
			deadline = time.Now().Add(5 * time.Second)
			for (mcpLinuxLive(b.server) || mcpLinuxLive(b.child)) && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if mcpLinuxLive(b.server) || mcpLinuxLive(b.child) {
				t.Fatal("second owner leaked")
			}
		})
	}
}
