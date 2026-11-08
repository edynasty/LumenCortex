//go:build !windows

package shell

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

// A FIFO safely models a special file whose read does not finish. Never scan /.
func TestRecursiveCanarySearchTimeoutLeavesOtherSessionAlive(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "controlled-root")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	other := exec.Command("sleep", "30")
	if err := other.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Process.Kill(); _ = other.Wait() }()
	r := New(dir)
	r.Interpreter = System()
	r.Interpreter.Args = []string{"-c"}
	result, err := r.RunTimeout(context.Background(), `echo $$ > shell.pid; ps -o pgid= -p $$ > shell.pgid; grep -rl "harbor-canary" controlled-root/blocked | head -40`, 150*time.Millisecond, nil)
	if err != nil || !result.TimedOut || result.Cancelled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if alive(readPID(t, filepath.Join(dir, "shell.pid"))) {
		t.Fatal("pipeline shell survived")
	}
	if !alive(other.Process.Pid) {
		t.Fatal("timeout killed another session")
	}
	// The process group must be gone, not just the shell leader.
	pid := readPID(t, filepath.Join(dir, "shell.pgid"))
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("ps", "-eo", "pgid=,stat=").Output()
		running := false
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == strconv.Itoa(pid) && !strings.HasPrefix(fields[1], "Z") {
				running = true
			}
		}
		if !running {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pipeline process group survived timeout")
}

func TestCallerCancellationGraceAndNoStartAfterCancellation(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		result, _ := New(dir).Run(ctx, `trap 'echo term > term.seen; exit 0' TERM; echo $$ > shell.pid; while :; do sleep 1; done`, nil)
		done <- result
	}()
	waitFor(t, filepath.Join(dir, "shell.pid"))
	cancel()
	select {
	case result := <-done:
		if !result.Cancelled || result.TimedOut {
			t.Fatalf("result=%+v", result)
		}
	case <-time.After(TermGrace + waitDelay + 2*time.Second):
		t.Fatal("cancel did not release Wait")
	}
	if _, err := os.Stat(filepath.Join(dir, "term.seen")); err != nil {
		t.Fatalf("shell did not receive SIGTERM: %v", err)
	}
	marker := filepath.Join(dir, "must-not-run")
	result, err := New(dir).Run(ctx, "touch "+marker, nil)
	if err != context.Canceled || !result.Cancelled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("cancelled command started")
	}
}
