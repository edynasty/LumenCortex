//go:build !windows

package shell

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A cancelled or timed-out command takes its whole process group with it: a background child of the
// shell does not survive, and Run returns promptly instead of waiting for the child to close the pipes.
func TestCancellationKillsTheProcessGroup(t *testing.T) {
	for _, name := range []string{"cancel", "deadline"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			// The child holds stdout open; "wait" keeps the shell itself alive, so the shell is not
			// replaced by exec and only a group kill reaches the child.
			cmd := `sleep 30 & echo $! > child.pid; wait`
			var ctx context.Context
			var cancel context.CancelFunc
			if name == "deadline" {
				ctx, cancel = context.WithTimeout(context.Background(), 300*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
				go func() {
					waitFor(t, pidFile)
					cancel()
				}()
			}
			defer cancel()

			started := time.Now()
			result, err := New(dir).Run(ctx, cmd, nil)
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Fatalf("Run returned after %s; a surviving child held the pipes", elapsed)
			}
			if !result.Cancelled {
				t.Fatalf("err=%v cancelled=false, want Cancelled", err)
			}
			pid := readPID(t, pidFile)
			deadline := time.Now().Add(2 * time.Second)
			for alive(pid) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("background child %d survived the %s", pid, name)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// A command past its timeout has its process group terminated: the result reports TimedOut (not
// Cancelled) with the output read so far, a background child does not survive, and a group that ignores
// SIGTERM is killed once the grace period has passed.
func TestTimeoutTerminatesTheProcessGroup(t *testing.T) {
	for _, name := range []string{"term", "ignores-term"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "child.pid")
			cmd := `sleep 30 & echo $! > child.pid; echo started; wait`
			if name == "ignores-term" {
				// An ignored signal stays ignored across exec, so the shell and its child both outlive SIGTERM.
				cmd = `trap '' TERM; ` + cmd
			}
			started := time.Now()
			result, err := New(dir).RunTimeout(context.Background(), cmd, 300*time.Millisecond, nil)
			elapsed := time.Since(started)
			if err != nil {
				t.Fatalf("err=%v, want a timed-out result", err)
			}
			if !result.TimedOut || result.Cancelled || result.ExitCode != -1 {
				t.Fatalf("timedOut=%v cancelled=%v exit=%d, want timedOut, not cancelled, exit -1", result.TimedOut, result.Cancelled, result.ExitCode)
			}
			if !strings.Contains(string(result.Stdout.Head), "started") {
				t.Fatalf("stdout=%q, want the output written before the timeout", result.Stdout.Head)
			}
			if name == "ignores-term" && elapsed < 300*time.Millisecond+TermGrace {
				t.Fatalf("returned after %s: a group that ignores SIGTERM must get the %s grace before SIGKILL", elapsed, TermGrace)
			}
			if elapsed > 300*time.Millisecond+TermGrace+waitDelay+2*time.Second {
				t.Fatalf("returned after %s", elapsed)
			}
			pid := readPID(t, pidFile)
			deadline := time.Now().Add(2 * time.Second)
			for alive(pid) {
				if time.Now().After(deadline) {
					_ = syscall.Kill(pid, syscall.SIGKILL)
					t.Fatalf("background child %d survived the timeout", pid)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
	t.Run("finishes-in-time", func(t *testing.T) {
		result, err := New(t.TempDir()).RunTimeout(context.Background(), `echo ok; exit 3`, 5*time.Second, nil)
		if err != nil || result.TimedOut || result.ExitCode != 3 {
			t.Fatalf("result=%+v err=%v, want exit 3 without a timeout", result, err)
		}
	})
}

// After a normal exit only a reaping runner kills what the command left in the background; the agent's
// shell tool keeps a detached process running.
func TestReapKillsDetachedChildrenAfterExit(t *testing.T) {
	for _, reap := range []bool{false, true} {
		t.Run(fmt.Sprintf("reap=%v", reap), func(t *testing.T) {
			dir := t.TempDir()
			r := New(dir)
			r.Reap = reap
			result, err := r.Run(context.Background(), `sleep 30 >/dev/null 2>&1 & echo $! > child.pid`, nil)
			if err != nil || result.ExitCode != 0 || result.Cancelled {
				t.Fatalf("err=%v result=%+v", err, result)
			}
			pid := readPID(t, filepath.Join(dir, "child.pid"))
			t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
			deadline := time.Now().Add(2 * time.Second)
			for alive(pid) && reap && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if alive(pid) == reap {
				t.Fatalf("reap=%v: child alive=%v", reap, alive(pid))
			}
		})
	}
}

func waitFor(t *testing.T, path string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) != "" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("the child never started")
}

func readPID(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// alive reports whether pid still runs; a zombie awaiting its reaper counts as gone.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err == nil {
		return !strings.Contains(string(out), ") Z ")
	}
	// No /proc (macOS): ps reports Z for a zombie.
	state, err := psState(pid)
	return err != nil || !strings.HasPrefix(state, "Z")
}

func psState(pid int) (string, error) {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
