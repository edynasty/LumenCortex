package session

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// The job states 0007_jobs.sql constrains. A job starts queued, a worker claims it (running), and one
// terminal state follows: completed, failed, timed_out or cancelled. outcome_unknown is recorded for a
// running job whose owner process died: the command may have had effects, so it is never run again.
const (
	JobQueued         = "queued"
	JobRunning        = "running"
	JobCompleted      = "completed"
	JobFailed         = "failed"
	JobTimedOut       = "timed_out"
	JobCancelled      = "cancelled"
	JobOutcomeUnknown = "outcome_unknown"
)

// The job sources 0007_jobs.sql constrains: the run that submitted it, the host through the Engine
// API, or a scheduled task.
const (
	JobSourceAgent    = "agent"
	JobSourceHost     = "host"
	JobSourceSchedule = "schedule"
)

// JobActiveStates are the states a job can leave: a job in one of them is queued or running.
var JobActiveStates = []string{JobQueued, JobRunning}

// JobTerminalStates are the states no transition follows.
var JobTerminalStates = []string{JobCompleted, JobFailed, JobTimedOut, JobCancelled, JobOutcomeUnknown}

// Job limit errors: InsertJob returns them, and the job manager hands them to its caller unchanged.
var (
	ErrJobQueueFull = errors.New("job queue is full")
	ErrJobRunLimit  = errors.New("run has too many active jobs")
)

// JobTerminal reports whether no transition can follow state.
func JobTerminal(state string) bool {
	switch state {
	case JobCompleted, JobFailed, JobTimedOut, JobCancelled, JobOutcomeUnknown:
		return true
	}
	return false
}

// Job is one row of the jobs table: the durable record of a background command. It is the job's source
// of truth; the output lives in files beside the database and only its byte counts are kept here.
// SessionID, RunID, ToolCallID and ScheduleID are "" when the column is NULL.
type Job struct {
	ID              string
	Kind            string
	Source          string
	SessionID       string
	RunID           string
	ToolCallID      string
	ScheduleID      string
	Workspace       string
	Command         string
	Timeout         time.Duration
	State           string
	ExitCode        *int
	Error           string
	PID             int
	RunnerID        string
	RunnerPID       int
	StdoutBytes     int64
	StderrBytes     int64
	OutputTruncated bool
	CancelRequested bool
	CreatedAt       time.Time
	StartedAt       time.Time
	FinishedAt      time.Time
}

// JobRunner names a job manager that owns a queued or running job: its identity and the process that
// holds it, so a manager that opens can tell a live owner from a dead one.
type JobRunner struct {
	ID  string
	PID int
}

// JobFilter selects jobs for ListJobs. Every zero field is unset; States matches any of the named
// states. Limit is clamped to 1…500, 50 when unset.
type JobFilter struct {
	SessionID string
	RunID     string
	RunnerID  string
	Source    string
	States    []string
	Limit     int
}

// JobChange is the field set of one transition. State is always written; every other field is written
// only when it is not nil, so a transition that only records the pid leaves the rest as it was. Passing
// the state the row already has updates fields without moving it: the UPDATE is guarded by that state
// either way.
type JobChange struct {
	State           string
	ExitCode        *int
	Error           *string
	PID             *int
	StdoutBytes     *int64
	StderrBytes     *int64
	OutputTruncated *bool
	CancelRequested *bool
	StartedAt       *time.Time
	FinishedAt      *time.Time
}

// jobColumns is the read projection of a row, in the order scanJob consumes it. Nullable columns are
// coalesced so a scan needs no intermediate holders beyond the two numeric ones.
const jobColumns = `job_id, kind, source, COALESCE(session_id, ''), COALESCE(run_id, ''),
	COALESCE(tool_call_id, ''), COALESCE(schedule_id, ''), workspace, command, timeout_ms, state,
	exit_code, COALESCE(error, ''), COALESCE(pid, 0), runner_id, runner_pid, stdout_bytes, stderr_bytes,
	output_truncated, cancel_requested, created_at, COALESCE(started_at, ''), COALESCE(finished_at, '')`

// jobTimeLayout is the timestamp format of the jobs table: RFC 3339 in UTC with a fixed-width fraction,
// so a text comparison of two timestamps is their chronological order (RFC 3339 with a variable-width
// fraction would sort "…00.5Z" before "…00Z").
const jobTimeLayout = "2006-01-02T15:04:05.000000000Z"

func jobTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(jobTimeLayout)
}

func parseJobTime(raw string) time.Time {
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range []string{jobTimeLayout, time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func jobNullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var job Job
	var timeoutMS int64
	var exitCode sql.NullInt64
	var truncated, cancelRequested int
	var created, started, finished string
	err := row.Scan(&job.ID, &job.Kind, &job.Source, &job.SessionID, &job.RunID, &job.ToolCallID,
		&job.ScheduleID, &job.Workspace, &job.Command, &timeoutMS, &job.State, &exitCode, &job.Error,
		&job.PID, &job.RunnerID, &job.RunnerPID, &job.StdoutBytes, &job.StderrBytes, &truncated,
		&cancelRequested, &created, &started, &finished)
	if err != nil {
		return Job{}, err
	}
	job.Timeout = time.Duration(timeoutMS) * time.Millisecond
	if exitCode.Valid {
		code := int(exitCode.Int64)
		job.ExitCode = &code
	}
	job.OutputTruncated = truncated == 1
	job.CancelRequested = cancelRequested == 1
	job.CreatedAt = parseJobTime(created)
	job.StartedAt = parseJobTime(started)
	job.FinishedAt = parseJobTime(finished)
	return job, nil
}

// InsertJob commits a queued job. The queue limit and the run's active-job limit are counted in the
// same transaction as the insert, so two concurrent submissions never both take the last free slot.
// maxQueued and maxRunActive of 0 disable that check.
func (s *Store) InsertJob(ctx context.Context, job Job, maxQueued, maxRunActive int) error {
	if job.Kind == "" {
		job.Kind = "shell"
	}
	if job.State == "" {
		job.State = JobQueued
	}
	return s.writeTx(ctx, func(conn *sql.Conn) error {
		if maxQueued > 0 {
			var queued int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE state = ?`, JobQueued).Scan(&queued); err != nil {
				return err
			}
			if queued >= maxQueued {
				return ErrJobQueueFull
			}
		}
		if maxRunActive > 0 && job.RunID != "" {
			var active int
			if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE run_id = ? AND state IN (?, ?)`,
				job.RunID, JobQueued, JobRunning).Scan(&active); err != nil {
				return err
			}
			if active >= maxRunActive {
				return ErrJobRunLimit
			}
		}
		var exitCode any
		if job.ExitCode != nil {
			exitCode = *job.ExitCode
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO jobs(
			job_id, kind, source, session_id, run_id, tool_call_id, schedule_id, workspace, command,
			timeout_ms, state, exit_code, error, pid, runner_id, runner_pid, stdout_bytes, stderr_bytes,
			output_truncated, cancel_requested, created_at, started_at, finished_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			job.ID, job.Kind, job.Source, jobNullable(job.SessionID), jobNullable(job.RunID),
			jobNullable(job.ToolCallID), jobNullable(job.ScheduleID), job.Workspace, job.Command,
			job.Timeout.Milliseconds(), job.State, exitCode, jobNullable(job.Error), jobNullableInt(job.PID),
			job.RunnerID, job.RunnerPID, job.StdoutBytes, job.StderrBytes, job.OutputTruncated,
			job.CancelRequested, jobTime(job.CreatedAt), jobTime(job.StartedAt), jobTime(job.FinishedAt))
		return err
	})
}

func jobNullableInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

// GetJob reads one job; an unknown id is ErrNotFound.
func (s *Store) GetJob(ctx context.Context, id string) (Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE job_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

// ListJobs reads jobs newest first under the filter.
func (s *Store) ListJobs(ctx context.Context, filter JobFilter) ([]Job, error) {
	query := `SELECT ` + jobColumns + ` FROM jobs WHERE 1 = 1`
	args := make([]any, 0, 8)
	if filter.SessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, filter.SessionID)
	}
	if filter.RunID != "" {
		query += ` AND run_id = ?`
		args = append(args, filter.RunID)
	}
	if filter.RunnerID != "" {
		query += ` AND runner_id = ?`
		args = append(args, filter.RunnerID)
	}
	if filter.Source != "" {
		query += ` AND source = ?`
		args = append(args, filter.Source)
	}
	if len(filter.States) > 0 {
		query += ` AND state IN (` + jobPlaceholders(len(filter.States)) + `)`
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	query += ` ORDER BY created_at DESC, job_id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		job, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

func jobPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// QueuedJob returns the oldest queued job of a runner, ErrNotFound when it has none. Two workers may
// read the same row; the compare-and-set of the claim decides which one moves it.
func (s *Store) QueuedJob(ctx context.Context, runnerID string) (Job, error) {
	job, err := scanJob(s.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE state = ? AND runner_id = ? ORDER BY created_at, job_id LIMIT 1`,
		JobQueued, runnerID))
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	return job, err
}

// UpdateJob is one guarded transition: the row moves only while it is still in state from, and it
// reports whether this caller moved it. Two writers never both move a job.
func (s *Store) UpdateJob(ctx context.Context, id, from string, change JobChange) (bool, error) {
	if change.State == "" {
		return false, errors.New("job state is required")
	}
	sets := []string{"state = ?"}
	args := []any{change.State}
	add := func(column string, value any) {
		sets = append(sets, column+" = ?")
		args = append(args, value)
	}
	if change.ExitCode != nil {
		add("exit_code", *change.ExitCode)
	}
	if change.Error != nil {
		add("error", jobNullable(*change.Error))
	}
	if change.PID != nil {
		add("pid", jobNullableInt(*change.PID))
	}
	if change.StdoutBytes != nil {
		add("stdout_bytes", *change.StdoutBytes)
	}
	if change.StderrBytes != nil {
		add("stderr_bytes", *change.StderrBytes)
	}
	if change.OutputTruncated != nil {
		add("output_truncated", *change.OutputTruncated)
	}
	if change.CancelRequested != nil {
		add("cancel_requested", *change.CancelRequested)
	}
	if change.StartedAt != nil {
		add("started_at", jobTime(*change.StartedAt))
	}
	if change.FinishedAt != nil {
		add("finished_at", jobTime(*change.FinishedAt))
	}
	args = append(args, id, from)
	result, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET `+strings.Join(sets, ", ")+` WHERE job_id = ? AND state = ?`, args...)
	if err != nil {
		return false, err
	}
	moved, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return moved == 1, nil
}

// ActiveJobsForRun reads a run's queued and running jobs, newest first.
func (s *Store) ActiveJobsForRun(ctx context.Context, runID string) ([]Job, error) {
	return s.ListJobs(ctx, JobFilter{RunID: runID, States: JobActiveStates, Limit: 500})
}

// JobRunners lists the managers that own a queued or running job. A manager that opens compares each
// with its own identity and the liveness of the process that holds it.
func (s *Store) JobRunners(ctx context.Context) ([]JobRunner, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT runner_id, runner_pid FROM jobs WHERE state IN (?, ?) ORDER BY runner_id`,
		JobQueued, JobRunning)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRunner
	for rows.Next() {
		var runner JobRunner
		if err := rows.Scan(&runner.ID, &runner.PID); err != nil {
			return nil, err
		}
		out = append(out, runner)
	}
	return out, rows.Err()
}

// AdoptQueuedJobs moves the queued jobs of a dead runner to the manager that found them, so it runs
// them. A job that never started has had no effect and is safe to run.
func (s *Store) AdoptQueuedJobs(ctx context.Context, fromRunnerID, toRunnerID string, toPID int) (int64, error) {
	result, err := s.db.ExecContext(ctx,
		`UPDATE jobs SET runner_id = ?, runner_pid = ? WHERE state = ? AND runner_id = ?`,
		toRunnerID, toPID, JobQueued, fromRunnerID)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// DeleteFinishedJobsBefore deletes the finished jobs that ended before cutoff and returns their ids, so
// the caller can remove their output directories. Timestamps are compared after parsing, not in SQL:
// the text form is not fixed-width.
func (s *Store) DeleteFinishedJobsBefore(ctx context.Context, cutoff time.Time) ([]string, error) {
	var deleted []string
	err := s.writeTx(ctx, func(conn *sql.Conn) error {
		rows, err := conn.QueryContext(ctx,
			`SELECT job_id, COALESCE(finished_at, '') FROM jobs WHERE state IN (`+
				jobPlaceholders(len(JobTerminalStates))+`)`, jobStrings(JobTerminalStates)...)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id, finished string
			if err := rows.Scan(&id, &finished); err != nil {
				rows.Close()
				return err
			}
			at := parseJobTime(finished)
			if at.IsZero() || !at.Before(cutoff) {
				continue
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		for _, id := range ids {
			if _, err := conn.ExecContext(ctx, `DELETE FROM jobs WHERE job_id = ?`, id); err != nil {
				return err
			}
			deleted = append(deleted, id)
		}
		return nil
	})
	return deleted, err
}

func jobStrings(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
