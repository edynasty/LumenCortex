//go:build linux

package shell

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/cgroupv2"
)

// proc is a process as the host sees it: pid plus start time, so a check is never fooled by pid reuse.
type proc struct {
	pid   int
	start string
}

func procStat(pid int) (fields []string, ok bool) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, false
	}
	end := bytes.LastIndexByte(raw, ')')
	if end < 0 {
		return nil, false
	}
	return strings.Fields(string(raw[end+1:])), true
}

func identify(t *testing.T, dir, name string) proc {
	t.Helper()
	waitFor(t, filepath.Join(dir, name))
	pid := readPID(t, filepath.Join(dir, name))
	fields, ok := procStat(pid)
	if !ok {
		t.Fatalf("%s (%d) is not running", name, pid)
	}
	p := proc{pid: pid, start: fields[19]}
	t.Cleanup(func() {
		if p.running() {
			_ = syscall.Kill(p.pid, syscall.SIGKILL)
		}
	})
	return p
}

func (p proc) running() bool {
	fields, ok := procStat(p.pid)
	return ok && fields[19] == p.start && fields[0] != "Z" && fields[0] != "X"
}

func (p proc) pgid() int {
	fields, _ := procStat(p.pid)
	pgid, _ := strconv.Atoi(fields[2])
	return pgid
}

func (p proc) sid() int {
	fields, _ := procStat(p.pid)
	sid, _ := strconv.Atoi(fields[3])
	return sid
}

// cgroup returns the absolute cgroup directory of p.
func (p proc) cgroup(t *testing.T) string {
	t.Helper()
	rel, err := cgroupv2.ProcessCgroup(p.pid)
	if err != nil {
		t.Fatalf("cgroup of %d: %v", p.pid, err)
	}
	return filepath.Join(cgroupv2.Root, rel)
}

func requireCgroupContainment(t *testing.T) {
	t.Helper()
	kind, err := Containment()
	if kind != "cgroup" {
		t.Skipf("shell commands are contained by %s only on this host: %v", kind, err)
	}
}

func gone(t *testing.T, within time.Duration, procs map[string]proc) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		var alive []string
		for name, p := range procs {
			if p.running() {
				alive = append(alive, fmt.Sprintf("%s=%d", name, p.pid))
			}
		}
		if len(alive) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("still running after %s: %v", within, alive)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func running(t *testing.T, procs map[string]proc) {
	t.Helper()
	for name, p := range procs {
		if !p.running() {
			t.Fatalf("%s (%d) is not running", name, p.pid)
		}
	}
}

// commandTree is a command that leaves a tree of every kind behind it: a same-group child, a grandchild,
// a setsid session leader and a double-forked daemon in its own session, each recording its pid.
const commandTree = `echo $$ > tool
sleep 60 & echo $! > child
(sleep 60 & echo $! > grandchild; wait) &
setsid /bin/sh -c 'echo $$ > session; exec sleep 60' </dev/null >/dev/null 2>&1 &
/bin/sh -c 'setsid /bin/sh -c "echo \$\$ > daemon; exec sleep 60" </dev/null >/dev/null 2>&1 &' &
`

// TestContainmentOwnerHelper is the owning process of the containment tests: it runs one command with
// the runner configured by LC_CONTAIN_MODE and is killed while (or after) the command runs.
func TestContainmentOwnerHelper(t *testing.T) {
	dir := os.Getenv("LC_CONTAIN_DIR")
	if dir == "" {
		t.Skip("helper process of the Linux containment tests")
	}
	r := New(dir)
	switch os.Getenv("LC_CONTAIN_MODE") {
	case "background":
		// The command ends on its own; its background tree is released and the owner keeps living.
		_, _ = r.Run(context.Background(), commandTree+"sleep 0.2", nil)
		_ = os.WriteFile(filepath.Join(dir, "returned"), []byte("1"), 0o600)
		time.Sleep(60 * time.Second)
	default:
		_, _ = r.Run(context.Background(), commandTree+"wait", nil)
	}
}

func startOwner(t *testing.T, mode string) (*exec.Cmd, string) {
	t.Helper()
	dir := t.TempDir()
	owner := exec.Command(os.Args[0], "-test.run=^TestContainmentOwnerHelper$")
	owner.Env = append(os.Environ(), "LC_CONTAIN_DIR="+dir, "LC_CONTAIN_MODE="+mode)
	owner.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := owner.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Process.Kill(); _ = owner.Wait() })
	return owner, dir
}

func commandProcs(t *testing.T, dir string) map[string]proc {
	t.Helper()
	procs := map[string]proc{}
	for _, name := range []string{"tool", "child", "grandchild", "session", "daemon"} {
		procs[name] = identify(t, dir, name)
	}
	return procs
}

// Every process of a running command - including those in a new session, which a process-group kill
// misses - is in the command's own leaf, and dies with the owner however the owner dies, even when the
// command's crash guard or the pool's keeper died first. An unrelated process of the same user and a
// second owner's command are untouched.
func TestOwnerDeathStopsTheWholeCommandTree(t *testing.T) {
	requireCgroupContainment(t)
	for _, tc := range []struct {
		name     string
		signal   syscall.Signal
		preKill  string // "guard" or "keeper": killed with SIGKILL before the owner
		bystands bool
	}{
		{name: "owner-SIGKILL", signal: syscall.SIGKILL, bystands: true},
		{name: "owner-SIGTERM", signal: syscall.SIGTERM},
		{name: "guard-crash-then-owner-SIGKILL", signal: syscall.SIGKILL, preKill: "guard"},
		{name: "keeper-crash-then-owner-SIGKILL", signal: syscall.SIGKILL, preKill: "keeper"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := exec.Command("sleep", "60")
			if err := other.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
			var bystander map[string]proc
			if tc.bystands {
				_, otherDir := startOwner(t, "foreground")
				bystander = commandProcs(t, otherDir)
			}
			owner, dir := startOwner(t, "foreground")
			procs := commandProcs(t, dir)
			leaf := procs["tool"].cgroup(t)
			for name, p := range procs {
				if got := p.cgroup(t); got != leaf {
					t.Fatalf("%s is in %s, not in the command's leaf %s", name, got, leaf)
				}
			}
			if procs["session"].sid() == procs["tool"].sid() || procs["daemon"].sid() == procs["tool"].sid() {
				t.Fatalf("the setsid processes did not leave the command's session")
			}
			if bystander != nil && bystander["tool"].cgroup(t) == leaf {
				t.Fatalf("two owners' commands share leaf %s", leaf)
			}
			switch tc.preKill {
			case "guard":
				guard := procs["tool"].pgid()
				if err := syscall.Kill(guard, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			case "keeper":
				keepers, err := cgroupv2.ReadFile(filepath.Join(filepath.Dir(leaf), "keeper"), "cgroup.procs")
				if err != nil || keepers == "" {
					t.Fatalf("keeper of %s: %q %v", filepath.Dir(leaf), keepers, err)
				}
				for _, field := range strings.Fields(keepers) {
					pid, _ := strconv.Atoi(field)
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
			if tc.preKill != "" {
				// The command keeps running; only the owner's death may stop it.
				time.Sleep(100 * time.Millisecond)
				running(t, procs)
			}
			if err := owner.Process.Signal(tc.signal); err != nil {
				t.Fatal(err)
			}
			_ = owner.Wait()
			gone(t, 2*time.Second, procs)
			if !(proc{pid: other.Process.Pid}).runningIgnoringStart() {
				t.Fatal("an unrelated process of the same user was killed")
			}
			if bystander != nil {
				running(t, bystander)
			}
		})
	}
}

func (p proc) runningIgnoringStart() bool {
	fields, ok := procStat(p.pid)
	return ok && fields[0] != "Z" && fields[0] != "X"
}

// Cancellation and a timeout stop every process of the command, setsid sessions included, and remove
// its leaf; Reap does the same for a command that ends on its own.
func TestCancelTimeoutAndReapStopTheWholeCommandTree(t *testing.T) {
	requireCgroupContainment(t)
	for _, mode := range []string{"cancel", "timeout", "reap"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			r := New(dir)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			command, timeout := commandTree+"wait", time.Duration(0)
			switch mode {
			case "timeout":
				timeout = time.Second
			case "reap":
				r.Reap = true
				command = commandTree + "sleep 0.5"
			}
			done := make(chan Result, 1)
			go func() {
				result, _ := r.RunTimeout(ctx, command, timeout, nil)
				done <- result
			}()
			procs := commandProcs(t, dir)
			leaf := procs["tool"].cgroup(t)
			if mode == "cancel" {
				cancel()
			}
			var result Result
			select {
			case result = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("the command did not return")
			}
			switch mode {
			case "cancel":
				if !result.Cancelled {
					t.Fatalf("result %+v", result)
				}
			case "timeout":
				if !result.TimedOut {
					t.Fatalf("result %+v", result)
				}
			}
			gone(t, 2*time.Second, procs)
			if _, err := os.Stat(leaf); !os.IsNotExist(err) {
				t.Fatalf("the command's leaf %s is still there: %v", leaf, err)
			}
		})
	}
}

// Without Reap, a command that ends on its own releases what it left in the background: the tree keeps
// running while the owner lives, and dies with the owner.
func TestReleasedBackgroundLivesUntilTheOwnerDies(t *testing.T) {
	requireCgroupContainment(t)
	owner, dir := startOwner(t, "background")
	procs := commandProcs(t, dir)
	waitFor(t, filepath.Join(dir, "returned"))
	// The foreground shell ended with the command; what it started in the background is released.
	delete(procs, "tool")
	time.Sleep(200 * time.Millisecond)
	running(t, procs)
	if err := owner.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = owner.Wait()
	gone(t, 2*time.Second, procs)
}

// The command's own code already runs in its leaf: it is admitted before the gate releases it.
func TestCommandRunsInItsOwnLeaf(t *testing.T) {
	requireCgroupContainment(t)
	r := New(t.TempDir())
	result, err := r.Run(context.Background(), "cat /proc/self/cgroup", nil)
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("Run: %+v %v", result, err)
	}
	line := strings.TrimSpace(string(result.Stdout.Head))
	if !regexp.MustCompile(`^0::/.*/lc-shell-[0-9]+-[0-9a-f]+\.scope/c[0-9]+$`).MatchString(line) {
		t.Fatalf("the command ran in %q, not in its own shell leaf", line)
	}
}
