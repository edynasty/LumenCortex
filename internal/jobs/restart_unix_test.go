//go:build !windows

package jobs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
)

const restartHelperEnv = "LCX_JOBS_RESTART_HELPER"

// TestHelperRunsJobsAndWaits is the child of TestRestartAfterOwnerDeath: a real manager over a real
// database starts one job that blocks and queues a second, records both ids and waits to be killed.
func TestHelperRunsJobsAndWaits(t *testing.T) {
	dir := os.Getenv(restartHelperEnv)
	if dir == "" {
		t.Skip("helper process only")
	}
	store, err := session.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := New(Options{
		Store:    store,
		StateDir: dir,
		Limits: func() config.Execution {
			// One worker: the second job stays queued behind the blocking first one.
			return config.Execution{MaxConcurrentJobs: 1, MaxQueuedJobs: 8, JobOutputBytes: 64 << 10, JobRetentionDays: 7}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "ran.txt")
	first, err := manager.Submit(context.Background(), Spec{
		Command:   fmt.Sprintf("echo ran >> %s; sleep 300", marker),
		Workspace: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Submit(context.Background(), Spec{
		Command:   fmt.Sprintf("echo adopted >> %s", marker),
		Workspace: dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 500 {
		running, err := manager.Get(context.Background(), first.ID)
		if err != nil {
			t.Fatal(err)
		}
		queued, err := manager.Get(context.Background(), second.ID)
		if err != nil {
			t.Fatal(err)
		}
		if running.State == session.JobRunning && queued.State == session.JobQueued {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(filepath.Join(dir, "ids.txt"), []byte(first.ID+"\n"+second.ID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte("1"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Wait to be killed: the owner of these jobs dies without recording how they ended.
	time.Sleep(5 * time.Minute)
}

// TestRestartAfterOwnerDeath kills a real process that owns a running and a queued job, then opens a
// manager on the same database: the running job becomes outcome_unknown and is never run again, the
// queued one is adopted and runs exactly once.
func TestRestartAfterOwnerDeath(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperRunsJobsAndWaits$", "-test.timeout=120s")
	cmd.Env = append(os.Environ(), restartHelperEnv+"="+dir)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "ready")
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("the helper did not start its jobs")
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "ids.txt"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(raw))
	if len(lines) != 2 {
		t.Fatalf("ids=%q", raw)
	}
	firstID, secondID := lines[0], lines[1]

	// The owner dies the way a crashed process does: no cleanup, no terminal transition.
	if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()

	store, err := session.Open(filepath.Join(dir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// The job's process group survived its owner; kill it so the test leaves nothing behind.
	if first, err := store.GetJob(context.Background(), firstID); err == nil && first.PID > 0 {
		defer func() { _ = syscall.Kill(-first.PID, syscall.SIGKILL) }()
	}
	manager, err := New(Options{Store: store, StateDir: dir, Limits: func() config.Execution {
		return config.Execution{MaxConcurrentJobs: 2, MaxQueuedJobs: 8, JobOutputBytes: 64 << 10, JobRetentionDays: 7}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	unknown, err := manager.Get(context.Background(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.State != session.JobOutcomeUnknown {
		t.Fatalf("running job of the dead owner: state=%q error=%q", unknown.State, unknown.Error)
	}
	if unknown.FinishedAt.IsZero() {
		t.Fatal("outcome_unknown job has no finish time")
	}
	adopted, ok, err := manager.Wait(context.Background(), []string{secondID}, 30*time.Second)
	if err != nil || !ok {
		t.Fatalf("wait adopted: ok=%v err=%v", ok, err)
	}
	if adopted.State != session.JobCompleted || adopted.RunnerID != manager.RunnerID() {
		t.Fatalf("adopted job=%+v", adopted)
	}
	body, err := os.ReadFile(filepath.Join(dir, "ran.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(body)); len(got) != 2 || got[0] != "ran" || got[1] != "adopted" {
		t.Fatalf("marker=%q: the running job was re-run or the queued one was not", got)
	}
	// The outcome_unknown job's row is untouched by the new owner: never re-run, never re-started.
	again, err := manager.Get(context.Background(), firstID)
	if err != nil {
		t.Fatal(err)
	}
	if again.State != session.JobOutcomeUnknown || !again.StartedAt.Equal(unknown.StartedAt) {
		t.Fatalf("outcome_unknown job changed: %+v", again)
	}
}
