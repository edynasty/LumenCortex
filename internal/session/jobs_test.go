package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openJobsStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), ".lumencortex"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Create(context.Background(), Session{ID: "session_jobs", Goal: "jobs"}); err != nil {
		t.Fatal(err)
	}
	return store
}

func jobRow(id, state string, created time.Time) Job {
	return Job{
		ID: id, Kind: "shell", Source: JobSourceAgent, SessionID: "session_jobs", RunID: "run_1",
		Workspace: "/w", Command: "make", State: state, RunnerID: "runner_1", RunnerPID: 42,
		CreatedAt: created,
	}
}

// The queue's reads and its guarded writes: a row is inserted queued, listed newest first under each
// filter, counted by state, and moved by exactly one compare-and-set.
func TestJobRowsRoundTripAndFilters(t *testing.T) {
	ctx := context.Background()
	store := openJobsStore(t)
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for i, spec := range []struct {
		id    string
		state string
	}{{"job_a", JobQueued}, {"job_b", JobRunning}, {"job_c", JobCompleted}, {"job_d", JobQueued}} {
		job := jobRow(spec.id, spec.state, base.Add(time.Duration(i)*time.Minute))
		if err := store.InsertJob(ctx, job, 0, 0); err != nil {
			t.Fatalf("insert %s: %v", spec.id, err)
		}
	}

	got, err := store.GetJob(ctx, "job_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobQueued || got.SessionID != "session_jobs" || got.RunID != "run_1" || got.Command != "make" {
		t.Fatalf("job=%+v", got)
	}
	if got.ExitCode != nil || !got.FinishedAt.IsZero() || !got.StartedAt.IsZero() {
		t.Fatalf("empty fields not empty: %+v", got)
	}
	if _, err := store.GetJob(ctx, "job_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown job err=%v, want ErrNotFound", err)
	}

	all, err := store.ListJobs(ctx, JobFilter{SessionID: "session_jobs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[0].ID != "job_d" || all[3].ID != "job_a" {
		t.Fatalf("newest first order: %v", jobIDs(all))
	}
	queued, err := store.ListJobs(ctx, JobFilter{SessionID: "session_jobs", States: []string{JobQueued}})
	if err != nil {
		t.Fatal(err)
	}
	if len(queued) != 2 {
		t.Fatalf("queued=%v", jobIDs(queued))
	}
	byRun, err := store.ListJobs(ctx, JobFilter{RunID: "run_1", Limit: 2})
	if err != nil || len(byRun) != 2 {
		t.Fatalf("by run=%v err=%v", jobIDs(byRun), err)
	}
	active, err := store.ActiveJobsForRun(ctx, "run_1")
	if err != nil || len(active) != 3 {
		t.Fatalf("active=%v err=%v", jobIDs(active), err)
	}
	runners, err := store.JobRunners(ctx)
	if err != nil || len(runners) != 1 || runners[0].ID != "runner_1" || runners[0].PID != 42 {
		t.Fatalf("runners=%+v err=%v", runners, err)
	}
	oldest, err := store.QueuedJob(ctx, "runner_1")
	if err != nil || oldest.ID != "job_a" {
		t.Fatalf("oldest queued=%+v err=%v", oldest, err)
	}

	// One guarded transition: the second writer with the same from-state does not move the row.
	now := base.Add(time.Hour)
	moved, err := store.UpdateJob(ctx, "job_a", JobQueued, JobChange{State: JobRunning, StartedAt: &now})
	if err != nil || !moved {
		t.Fatalf("claim: moved=%v err=%v", moved, err)
	}
	moved, err = store.UpdateJob(ctx, "job_a", JobQueued, JobChange{State: JobCancelled})
	if err != nil || moved {
		t.Fatalf("second claim of a running job: moved=%v err=%v", moved, err)
	}
	exit := 3
	text := "exit status 3"
	finished := now.Add(time.Second)
	moved, err = store.UpdateJob(ctx, "job_a", JobRunning, JobChange{
		State: JobFailed, ExitCode: &exit, Error: &text, FinishedAt: &finished,
	})
	if err != nil || !moved {
		t.Fatalf("finish: moved=%v err=%v", moved, err)
	}
	got, err = store.GetJob(ctx, "job_a")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != JobFailed || got.ExitCode == nil || *got.ExitCode != 3 || got.Error != "exit status 3" {
		t.Fatalf("finished job=%+v", got)
	}
	if !got.StartedAt.Equal(now) || !got.FinishedAt.Equal(finished) {
		t.Fatalf("timestamps: started=%v finished=%v", got.StartedAt, got.FinishedAt)
	}
}

func jobIDs(jobs []Job) []string {
	out := make([]string, len(jobs))
	for i, job := range jobs {
		out[i] = job.ID
	}
	return out
}

// The insert's transaction holds the queue limit and the run's active limit: two concurrent
// submissions never both take the last slot.
func TestInsertJobEnforcesLimitsInItsTransaction(t *testing.T) {
	ctx := context.Background()
	store := openJobsStore(t)
	base := time.Now().UTC()
	if err := store.InsertJob(ctx, jobRow("job_1", JobQueued, base), 1, 8); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertJob(ctx, jobRow("job_2", JobQueued, base.Add(time.Second)), 1, 8); !errors.Is(err, ErrJobQueueFull) {
		t.Fatalf("second queued job err=%v, want ErrJobQueueFull", err)
	}
	if err := store.InsertJob(ctx, jobRow("job_3", JobQueued, base), 0, 1); !errors.Is(err, ErrJobRunLimit) {
		t.Fatalf("second active job of a run err=%v, want ErrJobRunLimit", err)
	}
	// A host job has no run: the per-run limit does not apply to it.
	host := jobRow("job_host", JobQueued, base)
	host.RunID, host.SessionID = "", ""
	if err := store.InsertJob(ctx, host, 0, 1); err != nil {
		t.Fatalf("host job: %v", err)
	}
}

// A manager adopts the queued jobs of a dead runner and leaves the running ones where they are.
func TestAdoptQueuedJobsMovesOnlyQueued(t *testing.T) {
	ctx := context.Background()
	store := openJobsStore(t)
	base := time.Now().UTC()
	for _, state := range []string{JobQueued, JobRunning, JobCompleted} {
		job := jobRow("job_"+state, state, base)
		if err := store.InsertJob(ctx, job, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	moved, err := store.AdoptQueuedJobs(ctx, "runner_1", "runner_2", 4242)
	if err != nil {
		t.Fatal(err)
	}
	if moved != 1 {
		t.Fatalf("adopted=%d, want only the queued job", moved)
	}
	for state, wantRunner := range map[string]string{JobQueued: "runner_2", JobRunning: "runner_1", JobCompleted: "runner_1"} {
		job, err := store.GetJob(ctx, "job_"+state)
		if err != nil {
			t.Fatal(err)
		}
		if job.RunnerID != wantRunner {
			t.Fatalf("%s runner=%q, want %q", state, job.RunnerID, wantRunner)
		}
		if state == JobQueued && job.RunnerPID != 4242 {
			t.Fatalf("adopted pid=%d", job.RunnerPID)
		}
	}
}

// Retention deletes only finished jobs that ended before the cutoff, and reports their ids.
func TestDeleteFinishedJobsBefore(t *testing.T) {
	ctx := context.Background()
	store := openJobsStore(t)
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	oldFinished := base
	recentFinished := base.Add(72 * time.Hour)
	for _, job := range []Job{
		func() Job {
			row := jobRow("job_old", JobCompleted, base)
			row.FinishedAt = oldFinished
			return row
		}(),
		func() Job {
			row := jobRow("job_recent", JobCompleted, base.Add(48*time.Hour))
			row.FinishedAt = recentFinished
			return row
		}(),
		jobRow("job_active", JobRunning, base),
	} {
		if err := store.InsertJob(ctx, job, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.DeleteFinishedJobsBefore(ctx, base.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 1 || deleted[0] != "job_old" {
		t.Fatalf("deleted=%v", deleted)
	}
	if _, err := store.GetJob(ctx, "job_old"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old job survived: err=%v", err)
	}
	if _, err := store.GetJob(ctx, "job_recent"); err != nil {
		t.Fatalf("recent job was deleted: %v", err)
	}
	if _, err := store.GetJob(ctx, "job_active"); err != nil {
		t.Fatalf("active job was deleted: %v", err)
	}
}

// A session's jobs go with it: the foreign key cascades and the purge list names the table.
func TestJobsAreDeletedWithTheirSession(t *testing.T) {
	ctx := context.Background()
	store := openJobsStore(t)
	base := time.Now().UTC()
	if err := store.InsertJob(ctx, jobRow("job_cascade", JobQueued, base), 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, "session_jobs"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetJob(ctx, "job_cascade"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job survived its session: err=%v", err)
	}
}
