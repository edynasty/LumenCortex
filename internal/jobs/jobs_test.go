package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
)

// newManager opens a real session store in a temp directory and a manager over it, the way the Engine
// does: the state directory holds the database and the job output.
func newManager(t *testing.T, limits config.Execution) (*Manager, *session.Store, string) {
	t.Helper()
	stateDir := t.TempDir()
	store, err := session.Open(filepath.Join(stateDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager, err := New(Options{
		Store:    store,
		StateDir: stateDir,
		Limits:   func() config.Execution { return limits },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, store, stateDir
}

func defaultLimits() config.Execution {
	return config.Execution{MaxConcurrentJobs: 2, MaxQueuedJobs: 32, JobOutputBytes: 64 << 10, JobRetentionDays: 7}
}

func waitState(t *testing.T, manager *Manager, id, state string, timeout time.Duration) Job {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := manager.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == state {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, _ := manager.Get(context.Background(), id)
	t.Fatalf("job %s state=%q, want %q", id, job.State, state)
	return Job{}
}

func TestBackgroundJobCompletes(t *testing.T) {
	manager, _, _ := newManager(t, defaultLimits())
	workspace := t.TempDir()
	job, err := manager.Submit(context.Background(), Spec{Command: "echo hello; echo oops >&2", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	if job.State != session.JobQueued {
		t.Fatalf("submitted state=%q, want queued", job.State)
	}
	done, ok, err := manager.Wait(context.Background(), []string{job.ID}, 10*time.Second)
	if err != nil || !ok {
		t.Fatalf("wait: ok=%v err=%v", ok, err)
	}
	if done.State != session.JobCompleted {
		t.Fatalf("state=%q error=%q, want completed", done.State, done.Error)
	}
	if done.ExitCode == nil || *done.ExitCode != 0 {
		t.Fatalf("exit code=%v, want 0", done.ExitCode)
	}
	if done.StartedAt.IsZero() || done.FinishedAt.IsZero() {
		t.Fatalf("timestamps missing: started=%v finished=%v", done.StartedAt, done.FinishedAt)
	}
	out, err := manager.Output(context.Background(), job.ID, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Stdout.Head, "hello") || !strings.Contains(out.Stderr.Head, "oops") {
		t.Fatalf("output=%+v", out)
	}
	if out.Stdout.Total != int64(len("hello\n")) {
		t.Fatalf("stdout total=%d", out.Stdout.Total)
	}
}

func TestQueueLimitRefusesSubmission(t *testing.T) {
	limits := defaultLimits()
	limits.MaxConcurrentJobs = 1
	limits.MaxQueuedJobs = 2
	manager, _, _ := newManager(t, limits)
	workspace := t.TempDir()
	// One job holds the only worker; two more fill the queue.
	blocker, err := manager.Submit(context.Background(), Spec{Command: "sleep 30", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, manager, blocker.ID, session.JobRunning, 10*time.Second)
	for range 2 {
		if _, err := manager.Submit(context.Background(), Spec{Command: "true", Workspace: workspace}); err != nil {
			t.Fatal(err)
		}
	}
	_, err = manager.Submit(context.Background(), Spec{Command: "true", Workspace: workspace})
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("err=%v, want ErrQueueFull", err)
	}
}

func TestRunJobLimitRefusesNinthActiveJob(t *testing.T) {
	limits := defaultLimits()
	limits.MaxConcurrentJobs = 1
	limits.MaxQueuedJobs = 64
	manager, _, _ := newManager(t, limits)
	workspace := t.TempDir()
	for i := range MaxActivePerRun {
		if _, err := manager.Submit(context.Background(), Spec{Command: "sleep 30", Workspace: workspace, RunID: "run_1"}); err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	_, err := manager.Submit(context.Background(), Spec{Command: "true", Workspace: workspace, RunID: "run_1"})
	if !errors.Is(err, ErrRunJobLimit) {
		t.Fatalf("err=%v, want ErrRunJobLimit", err)
	}
	// Another run is unaffected.
	if _, err := manager.Submit(context.Background(), Spec{Command: "true", Workspace: workspace, RunID: "run_2"}); err != nil {
		t.Fatalf("second run: %v", err)
	}
}

// The worker pool is execution.maxConcurrentJobs: never more running rows than workers.
func TestConcurrencyLimitRespected(t *testing.T) {
	limits := defaultLimits()
	limits.MaxConcurrentJobs = 2
	limits.MaxQueuedJobs = 64
	manager, _, _ := newManager(t, limits)
	workspace := t.TempDir()
	var ids []string
	for range 5 {
		job, err := manager.Submit(context.Background(), Spec{Command: "sleep 1", Workspace: workspace})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, job.ID)
	}
	maxRunning := 0
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		running, err := manager.List(context.Background(), Filter{States: []string{session.JobRunning}, Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(running) > maxRunning {
			maxRunning = len(running)
		}
		done := true
		for _, id := range ids {
			job, err := manager.Get(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if !session.JobTerminal(job.State) {
				done = false
			}
		}
		if done {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if maxRunning > 2 {
		t.Fatalf("observed %d running jobs at once, want at most 2", maxRunning)
	}
	if maxRunning < 2 {
		t.Fatalf("observed at most %d running jobs, want the pool to use 2", maxRunning)
	}
}

func TestCancelQueuedAndRunningKillsProcessGroup(t *testing.T) {
	limits := defaultLimits()
	limits.MaxConcurrentJobs = 1
	manager, _, _ := newManager(t, limits)
	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "child.pid")
	// The one worker runs the job that records the pid of its own child, then waits for it: the child
	// is in the job's process group.
	running, err := manager.Submit(context.Background(), Spec{
		Command:   fmt.Sprintf("sleep 60 & echo $! > %s; wait", pidFile),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, manager, running.ID, session.JobRunning, 10*time.Second)
	var childPID int
	for range 500 {
		raw, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
			fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &childPID)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("job child pid was not recorded")
	}
	queued, err := manager.Submit(context.Background(), Spec{Command: "sleep 30", Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, manager, queued.ID, session.JobQueued, 5*time.Second)

	// The queued job is cancelled at once, the running one after its group is killed.
	cancelled, err := manager.Cancel(context.Background(), queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != session.JobCancelled || cancelled.FinishedAt.IsZero() {
		t.Fatalf("queued cancel: %+v", cancelled)
	}
	stopped, err := manager.Cancel(context.Background(), running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != session.JobCancelled {
		t.Fatalf("running cancel state=%q error=%q", stopped.State, stopped.Error)
	}
	if alive(childPID) {
		t.Fatalf("child process %d survived the job cancel", childPID)
	}
	// A terminal job is returned unchanged.
	again, err := manager.Cancel(context.Background(), running.ID)
	if err != nil || again.State != session.JobCancelled {
		t.Fatalf("cancel of a terminal job: %+v err=%v", again, err)
	}
}

func alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func TestJobTimeoutEndsTimedOutWithPartialOutput(t *testing.T) {
	manager, _, _ := newManager(t, defaultLimits())
	workspace := t.TempDir()
	job, err := manager.Submit(context.Background(), Spec{
		Command:   "echo before-timeout; sleep 30",
		Workspace: workspace,
		Timeout:   300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, ok, err := manager.Wait(context.Background(), []string{job.ID}, 15*time.Second)
	if err != nil || !ok {
		t.Fatalf("wait: ok=%v err=%v", ok, err)
	}
	if done.State != session.JobTimedOut {
		t.Fatalf("state=%q, want timed_out", done.State)
	}
	if done.ExitCode == nil || *done.ExitCode != -1 {
		t.Fatalf("exit code=%v, want -1", done.ExitCode)
	}
	if !strings.Contains(done.Error, "timed out") {
		t.Fatalf("error=%q", done.Error)
	}
	out, err := manager.Output(context.Background(), job.ID, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.Stdout.Head, "before-timeout") {
		t.Fatalf("partial output lost: %+v", out.Stdout)
	}
}

func TestOutputIsBoundedOnDisk(t *testing.T) {
	limits := defaultLimits()
	limits.JobOutputBytes = 64 << 10
	manager, _, stateDir := newManager(t, limits)
	workspace := t.TempDir()
	job, err := manager.Submit(context.Background(), Spec{
		Command:   "head -c 1048576 /dev/zero | tr '\\0' 'x'",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, ok, err := manager.Wait(context.Background(), []string{job.ID}, 20*time.Second)
	if err != nil || !ok || done.State != session.JobCompleted {
		t.Fatalf("job: %+v ok=%v err=%v", done, ok, err)
	}
	if done.StdoutBytes != 1<<20 {
		t.Fatalf("stdout bytes=%d, want 1048576", done.StdoutBytes)
	}
	if !done.OutputTruncated {
		t.Fatal("truncation was not recorded")
	}
	dir := filepath.Join(stateDir, "jobs", job.ID)
	total := int64(0)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	if total > int64(limits.JobOutputBytes) {
		t.Fatalf("job output on disk=%d bytes, want at most %d", total, limits.JobOutputBytes)
	}
	out, err := manager.Output(context.Background(), job.ID, 8<<10)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Stdout.Truncated || out.Stdout.Total != 1<<20 {
		t.Fatalf("output=%+v", out.Stdout)
	}
	if len(out.Stdout.Head)+len(out.Stdout.Tail) > 8<<10 {
		t.Fatalf("read %d bytes, want at most 8192", len(out.Stdout.Head)+len(out.Stdout.Tail))
	}
}

func TestOutputIsReadableWhileRunning(t *testing.T) {
	manager, _, _ := newManager(t, defaultLimits())
	workspace := t.TempDir()
	job, err := manager.Submit(context.Background(), Spec{
		Command:   "echo first-line; sleep 5",
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, manager, job.ID, session.JobRunning, 10*time.Second)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		out, err := manager.Output(context.Background(), job.ID, 8<<10)
		if err != nil {
			t.Fatal(err)
		}
		if out.State != session.JobRunning {
			t.Fatalf("state=%q while running", out.State)
		}
		if strings.Contains(out.Stdout.Head, "first-line") {
			if _, err := manager.Cancel(context.Background(), job.ID); err != nil {
				t.Fatal(err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("output of a running job was not readable")
}

// A job whose owner process died while it ran is never run again: the next manager records
// outcome_unknown. Queued jobs of the dead owner are adopted and run.
func TestRecoveryAdoptsQueuedAndNeverRerunsRunning(t *testing.T) {
	stateDir := t.TempDir()
	store, err := session.Open(filepath.Join(stateDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "ran.txt")
	dead := session.JobRunner{ID: "runner_dead", PID: 999999}
	now := time.Now().UTC()
	running := session.Job{
		ID: "job_dead_running", Kind: "shell", Source: session.JobSourceAgent, Workspace: workspace,
		Command: "echo rerun >> " + marker, State: session.JobRunning, RunnerID: dead.ID, RunnerPID: dead.PID,
		CreatedAt: now.Add(-time.Minute), StartedAt: now.Add(-time.Minute), PID: 999998,
	}
	queued := session.Job{
		ID: "job_dead_queued", Kind: "shell", Source: session.JobSourceAgent, Workspace: workspace,
		Command: "echo adopted >> " + marker, State: session.JobQueued, RunnerID: dead.ID, RunnerPID: dead.PID,
		CreatedAt: now.Add(-2 * time.Minute),
	}
	for _, job := range []session.Job{running, queued} {
		if err := store.InsertJob(context.Background(), job, 0, 0); err != nil {
			t.Fatal(err)
		}
	}

	manager, err := New(Options{Store: store, StateDir: stateDir, Limits: defaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	unknown, err := manager.Get(context.Background(), running.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.State != session.JobOutcomeUnknown || unknown.FinishedAt.IsZero() {
		t.Fatalf("running job of a dead owner: %+v", unknown)
	}
	adopted, ok, err := manager.Wait(context.Background(), []string{queued.ID}, 20*time.Second)
	if err != nil || !ok {
		t.Fatalf("wait adopted: ok=%v err=%v", ok, err)
	}
	if adopted.State != session.JobCompleted {
		t.Fatalf("adopted job state=%q error=%q", adopted.State, adopted.Error)
	}
	if adopted.RunnerID != manager.RunnerID() {
		t.Fatalf("adopted runner=%q, want %q", adopted.RunnerID, manager.RunnerID())
	}
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 1 || lines[0] != "adopted" {
		t.Fatalf("marker=%q: a job of a dead owner was re-run", lines)
	}
}

// A queued job is moved by exactly one writer: the compare-and-set of the transition decides.
func TestCASRaceMovesAJobOnce(t *testing.T) {
	_, store, _ := newManager(t, defaultLimits())
	workspace := t.TempDir()
	job := session.Job{
		ID: "job_race", Kind: "shell", Source: session.JobSourceHost, Workspace: workspace,
		Command: "true", State: session.JobQueued, RunnerID: "runner_race", RunnerPID: os.Getpid(),
		CreatedAt: time.Now().UTC(),
	}
	if err := store.InsertJob(context.Background(), job, 0, 0); err != nil {
		t.Fatal(err)
	}
	const writers = 16
	var wg sync.WaitGroup
	moved := make(chan bool, writers)
	for i := range writers {
		wg.Add(1)
		go func(claim bool) {
			defer wg.Done()
			now := time.Now().UTC()
			change := session.JobChange{State: session.JobCancelled, FinishedAt: &now}
			if claim {
				change = session.JobChange{State: session.JobRunning, StartedAt: &now}
			}
			ok, err := store.UpdateJob(context.Background(), job.ID, session.JobQueued, change)
			if err != nil {
				t.Errorf("update: %v", err)
			}
			moved <- ok
		}(i%2 == 0)
	}
	wg.Wait()
	close(moved)
	winners := 0
	for ok := range moved {
		if ok {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d writers moved the job, want exactly 1", winners)
	}
	final, err := store.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != session.JobCancelled && final.State != session.JobRunning {
		t.Fatalf("final state=%q", final.State)
	}
}

func TestRetentionDeletesOldJobsAndOrphanOutput(t *testing.T) {
	limits := defaultLimits()
	limits.JobRetentionDays = 7
	manager, store, stateDir := newManager(t, limits)
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	dir := filepath.Join(stateDir, "jobs", "job_old")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stdout.head"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	job := session.Job{
		ID: "job_old", Kind: "shell", Source: session.JobSourceHost, Workspace: t.TempDir(),
		Command: "true", State: session.JobCompleted, RunnerID: "runner_old", RunnerPID: 1,
		CreatedAt: old, FinishedAt: old, ExitCode: intPtr(0),
	}
	if err := store.InsertJob(context.Background(), job, 0, 0); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(stateDir, "jobs", "job_purged_session")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := manager.Sweep(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetJob(context.Background(), job.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("old job row survived: err=%v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old job output survived: err=%v", err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan output survived: err=%v", err)
	}
}

func TestCloseCancelsRunningJobsAndLeavesNoGoroutines(t *testing.T) {
	stateDir := t.TempDir()
	store, err := session.Open(filepath.Join(stateDir, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err := New(Options{Store: store, StateDir: stateDir, Limits: defaultLimits})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	pidFile := filepath.Join(workspace, "child.pid")
	job, err := manager.Submit(context.Background(), Spec{
		Command:   fmt.Sprintf("sleep 60 & echo $! > %s; wait", pidFile),
		Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, manager, job.ID, session.JobRunning, 10*time.Second)
	var childPID int
	for range 500 {
		raw, err := os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(raw))) > 0 {
			fmt.Sscanf(strings.TrimSpace(string(raw)), "%d", &childPID)
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	before := goroutines()
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	closed, err := manager.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != session.JobCancelled || closed.Error != "engine closed" {
		t.Fatalf("closed job: state=%q error=%q", closed.State, closed.Error)
	}
	if alive(childPID) {
		t.Fatalf("child process %d survived Close", childPID)
	}
	// Closing twice is a no-op.
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Submit(context.Background(), Spec{Command: "true", Workspace: workspace}); !errors.Is(err, ErrClosed) {
		t.Fatalf("submit after Close: err=%v, want ErrClosed", err)
	}
	if after := goroutines(); after > before {
		t.Fatalf("goroutines after Close=%d, before=%d", after, before)
	}
}

// goroutines reports the number of live goroutines; a test compares it around Close.
func goroutines() int { return runtime.NumGoroutine() }

// A cancel that lands between the claim and the start still kills the command: the worker reads the
// pending stop before it starts the process group.
func TestCancelDuringClaimWindowKillsTheCommand(t *testing.T) {
	limits := defaultLimits()
	limits.MaxConcurrentJobs = 1
	manager, _, _ := newManager(t, limits)
	workspace := t.TempDir()
	marker := filepath.Join(workspace, "ran.txt")
	claimed := make(chan struct{})
	release := make(chan struct{})
	testHookAfterClaim = func() {
		close(claimed)
		<-release
	}
	defer func() { testHookAfterClaim = nil }()

	job, err := manager.Submit(context.Background(), Spec{Command: "echo ran >> " + marker, Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-claimed:
	case <-time.After(10 * time.Second):
		t.Fatal("the worker did not claim the job")
	}
	cancelled := make(chan Job, 1)
	go func() {
		job, err := manager.Cancel(context.Background(), job.ID)
		if err != nil {
			t.Errorf("cancel: %v", err)
		}
		cancelled <- job
	}()
	time.Sleep(100 * time.Millisecond)
	close(release)
	select {
	case done := <-cancelled:
		if done.State != session.JobCancelled {
			t.Fatalf("state=%q error=%q", done.State, done.Error)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cancel did not return")
	}
	if raw, err := os.ReadFile(marker); err == nil {
		t.Fatalf("cancelled job ran: %q", raw)
	}
}
