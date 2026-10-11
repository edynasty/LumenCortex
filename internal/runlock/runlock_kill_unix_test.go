//go:build unix

package runlock

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	helperEnv = "LCX_RUNLOCK_HELPER_DIR"
	helperID  = "session_crossproc"
)

// TestHelperHold is the child process of TestCrossProcessKill: it takes the lock
// and sleeps until it is killed.
func TestHelperHold(t *testing.T) {
	dir := os.Getenv(helperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	lock, err := TryAcquire(dir, helperID)
	if err != nil {
		fmt.Fprintf(os.Stdout, "error %v\n", err)
		os.Exit(3)
	}
	_ = lock
	fmt.Fprint(os.Stdout, "held\n")
	time.Sleep(5 * time.Minute)
	os.Exit(4) // not reached: the parent kills this process
}

// TestCrossProcessKill proves the lock excludes another process and that the
// kernel releases it when the holder dies without running any cleanup.
func TestCrossProcessKill(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHold$", "-test.count=1")
	cmd.Env = append(os.Environ(), helperEnv+"="+dir)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	line := make(chan string, 1)
	go func() {
		text, _ := bufio.NewReader(out).ReadString('\n')
		line <- text
	}()
	select {
	case text := <-line:
		if text != "held\n" {
			t.Fatalf("helper said %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("helper did not report held")
	}
	content, _ := os.ReadFile(dir + "/" + helperID + ".lock")
	if !strings.HasPrefix(string(content), fmt.Sprintf("pid=%d ", cmd.Process.Pid)) {
		t.Fatalf("lock file not written by the helper: %q", content)
	}
	if lock, err := TryAcquire(dir, helperID); !errors.Is(err, ErrHeld) {
		t.Fatalf("TryAcquire while another process holds: lock=%v err=%v, want ErrHeld", lock, err)
	}

	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	status, _ := cmd.ProcessState.Sys().(syscall.WaitStatus)
	if !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("helper was not killed by SIGKILL: %v", cmd.ProcessState)
	}
	lock, err := TryAcquire(dir, helperID)
	if err != nil {
		t.Fatalf("TryAcquire after kill -9: %v (the kernel should have released the lock)", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
}
