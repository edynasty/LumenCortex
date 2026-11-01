package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// gitWorkspace is a temporary Git repository, the workspace an Engine runs in for these tests.
func gitWorkspace(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.com"}, {"config", "user.name", "T"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a.txt"}, {"commit", "-qm", "init"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return root
}

func openJobsEngine(t *testing.T, workspace string) *Engine {
	t.Helper()
	engine, err := Open(Options{Workspace: workspace, RepoDir: filepath.Join(workspace, ".lumencortex-jobs")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	return engine
}

func waitJobState(t *testing.T, engine *Engine, id, state string, timeout time.Duration) JobInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := engine.Job(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if job.State == state {
			return job
		}
		time.Sleep(20 * time.Millisecond)
	}
	job, _ := engine.Job(context.Background(), id)
	t.Fatalf("job %s state=%q, want %q", id, job.State, state)
	return JobInfo{}
}

// The Engine API runs a host job end to end: submit, observe the transitions, read the output, list it,
// cancel one, and find the rows again after a reopen.
func TestEngineJobsLifecycle(t *testing.T) {
	workspace := gitWorkspace(t)
	engine := openJobsEngine(t, workspace)
	ctx := context.Background()
	events, unsubscribe := engine.Events(64)
	defer unsubscribe()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "jobs"})
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := engine.SubmitJob(ctx, JobRequest{
		SessionID: handle.ID,
		Command:   "echo engine-job; echo to-stderr >&2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if submitted.Source != session.JobSourceHost || submitted.SessionID != handle.ID {
		t.Fatalf("submitted=%+v", submitted)
	}
	done := waitJobState(t, engine, submitted.ID, session.JobCompleted, 20*time.Second)
	if done.ExitCode == nil || *done.ExitCode != 0 || done.StartedAt == nil || done.FinishedAt == nil {
		t.Fatalf("completed job=%+v", done)
	}
	if done.StdoutBytes == 0 || done.StderrBytes == 0 {
		t.Fatalf("byte counts=%d/%d", done.StdoutBytes, done.StderrBytes)
	}
	output, err := engine.JobOutput(ctx, submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.Stdout.Head, "engine-job") || !strings.Contains(output.Stderr.Head, "to-stderr") {
		t.Fatalf("output=%+v", output)
	}
	listed, err := engine.Jobs(ctx, JobQuery{SessionID: handle.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].ID != submitted.ID {
		t.Fatalf("jobs=%+v", listed)
	}

	long, err := engine.SubmitJob(ctx, JobRequest{SessionID: handle.ID, Command: "sleep 60"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, engine, long.ID, session.JobRunning, 20*time.Second)
	active, err := engine.ActiveJobsForRun(ctx, "no-such-run")
	if err != nil || len(active) != 0 {
		t.Fatalf("active of an unknown run: %+v err=%v", active, err)
	}
	cancelled, err := engine.CancelJob(ctx, long.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != session.JobCancelled {
		t.Fatalf("cancel=%+v", cancelled)
	}
	if alive, err := engine.JobActive(ctx, long.ID); err != nil || alive {
		t.Fatalf("cancelled job active=%v err=%v", alive, err)
	}

	// The transitions were published on the bus.
	sawState := false
	for {
		select {
		case event := <-events:
			if event.Type == "job.state" && strings.Contains(string(event.Data), submitted.ID) {
				sawState = true
			}
			continue
		default:
		}
		break
	}
	if !sawState {
		t.Fatal("no job.state event was published")
	}

	// The journal carries the transition without the command or its output.
	rows, err := engine.store.ListJobs(ctx, session.JobFilter{SessionID: handle.ID})
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Workspace: workspace, RepoDir: filepath.Join(workspace, ".lumencortex-jobs")})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	again, err := reopened.Jobs(context.Background(), JobQuery{SessionID: handle.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 2 {
		t.Fatalf("jobs after reopen=%+v", again)
	}
	for _, job := range again {
		if job.State == session.JobQueued || job.State == session.JobRunning {
			t.Fatalf("job %s reopened active: %q", job.ID, job.State)
		}
	}
}

// The journal row of a transition carries identifiers, the source, the state and the exit code only:
// never the command.
func TestJobJournalRowIsContentFree(t *testing.T) {
	workspace := gitWorkspace(t)
	engine := openJobsEngine(t, workspace)
	ctx := context.Background()
	submitted, err := engine.SubmitJob(ctx, JobRequest{Command: "echo journal-secret-marker"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, engine, submitted.ID, session.JobCompleted, 20*time.Second)

	db, err := sql.Open("sqlite3", "file:"+engine.store.Path()+"?mode=ro&_busy_timeout=5000")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var payloads []string
	rows, err := db.QueryContext(ctx, `SELECT payload_json FROM journal WHERE event = 'job.state'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatal(err)
		}
		payloads = append(payloads, payload)
	}
	if len(payloads) < 2 {
		t.Fatalf("journal job.state rows=%d, want the queued and completed transitions", len(payloads))
	}
	for _, payload := range payloads {
		if strings.Contains(payload, "journal-secret-marker") {
			t.Fatalf("journal row carries the command: %s", payload)
		}
		var fields map[string]any
		if err := json.Unmarshal([]byte(payload), &fields); err != nil {
			t.Fatalf("payload %q: %v", payload, err)
		}
		if fields["jobId"] != submitted.ID || fields["state"] == "" {
			t.Fatalf("journal payload=%v", fields)
		}
	}
}

// The binding a run's tools see refuses a job of another session and lists only its own run's active
// jobs.
func TestRunJobBindingRefusesOtherSessions(t *testing.T) {
	workspace := gitWorkspace(t)
	engine := openJobsEngine(t, workspace)
	ctx := context.Background()
	mine, err := engine.NewSession(ctx, SessionOptions{Goal: "mine"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := engine.NewSession(ctx, SessionOptions{Goal: "other"})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := engine.SubmitJob(ctx, JobRequest{SessionID: other.ID, Command: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.CancelJob(ctx, foreign.ID)

	binding := engine.runJobs(mine.ID, "run_mine", workspace)
	if _, err := binding.Get(ctx, foreign.ID); err == nil || !strings.Contains(err.Error(), "another session") {
		t.Fatalf("Get of another session's job: err=%v", err)
	}
	if _, err := binding.Cancel(ctx, foreign.ID); err == nil {
		t.Fatal("Cancel accepted another session's job")
	}
	if _, err := binding.Output(ctx, foreign.ID); err == nil {
		t.Fatal("Output accepted another session's job")
	}
	if _, _, err := binding.Wait(ctx, []string{foreign.ID}, time.Second); err == nil {
		t.Fatal("Wait accepted another session's job")
	}
	if jobs, err := binding.List(ctx, 20); err != nil || len(jobs) != 0 {
		t.Fatalf("List of a session with no jobs: %+v err=%v", jobs, err)
	}
	own, err := binding.Submit(ctx, "sleep 30", 0)
	if err != nil {
		t.Fatal(err)
	}
	if own.State != session.JobQueued {
		t.Fatalf("submitted=%+v", own)
	}
	waitJobState(t, engine, own.ID, session.JobRunning, 20*time.Second)
	active, err := binding.Active(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != own.ID {
		t.Fatalf("active=%+v", active)
	}
	if moved, err := engine.CancelRunJobs(ctx, "run_mine", "run ended"); err != nil || moved != 1 {
		t.Fatalf("CancelRunJobs moved=%d err=%v", moved, err)
	}
	job, err := engine.Job(ctx, own.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != session.JobCancelled || job.Error != "run ended" {
		t.Fatalf("job after run end=%+v", job)
	}
}

// Closing the Engine cancels the jobs it runs, so no child process outlives it.
func TestEngineCloseCancelsRunningJobs(t *testing.T) {
	workspace := gitWorkspace(t)
	engine := openJobsEngine(t, workspace)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "close"})
	if err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(workspace, "child.pid")
	job, err := engine.SubmitJob(ctx, JobRequest{
		SessionID: handle.ID,
		Command:   "sleep 60 & echo $! > " + pidFile + "; wait",
	})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, engine, job.ID, session.JobRunning, 20*time.Second)
	childPID := 0
	for range 500 {
		raw, err := os.ReadFile(pidFile)
		if err == nil && strings.TrimSpace(string(raw)) != "" {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child pid was not recorded")
	}
	if err := engine.Close(); err != nil {
		t.Fatal(err)
	}
	if processRunning(childPID) {
		t.Fatalf("child process %d survived Engine.Close", childPID)
	}

	reopened, err := Open(Options{Workspace: workspace, RepoDir: filepath.Join(workspace, ".lumencortex-jobs")})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	closed, err := reopened.Job(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.State != session.JobCancelled || closed.Error != "engine closed" {
		t.Fatalf("job after close=%+v", closed)
	}
}

// Deleting a session cancels its jobs before their rows go with it.
func TestSessionDeletionCancelsItsJobs(t *testing.T) {
	workspace := gitWorkspace(t)
	engine := openJobsEngine(t, workspace)
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "delete"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := engine.SubmitJob(ctx, JobRequest{SessionID: handle.ID, Command: "sleep 60"})
	if err != nil {
		t.Fatal(err)
	}
	waitJobState(t, engine, job.ID, session.JobRunning, 20*time.Second)
	if _, err := engine.DeleteSession(ctx, handle.ID, WorktreeCleanup{}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Job(ctx, job.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("job row survived the session deletion: err=%v", err)
	}
	// The session's output directory is swept with the row.
	if err := engine.jobs.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(engine.repoDir, "jobs", job.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job output survived: err=%v", err)
	}
}
