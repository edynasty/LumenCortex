//go:build linux

package cgroupv2

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestKeeperReplacementClosesOldControlDescriptors(t *testing.T) {
	p, err := NewPool(Options{Unit: "lc-keeper-fd-test", LeafPrefix: "c"})
	if err != nil {
		t.Skipf("delegated cgroup unavailable: %v", err)
	}
	defer p.Close()
	leaf, err := p.NewLeaf(nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "read _")
	gate, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	if err := leaf.Admit(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	before := count()
	for round := 1; round <= 5; round++ {
		if err := syscall.Kill(p.KeeperPID(), syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for p.KeeperRespawns() < round && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if p.KeeperRespawns() != round {
			t.Fatalf("keeper not replaced: %v", p.Err())
		}
	}
	if got := count(); got > before {
		t.Fatalf("keeper replacements leaked descriptors: before=%d after=%d", before, got)
	}
}
