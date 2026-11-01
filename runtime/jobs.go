package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/jobs"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// JobInfo is one background job as the Engine API reports it (ADR-0019 §3): the durable record plus its
// output byte counts. The command is content and belongs to the job's session.
type JobInfo struct {
	ID              string     `json:"jobId"`
	SessionID       string     `json:"sessionId,omitempty"`
	RunID           string     `json:"runId,omitempty"`
	ScheduleID      string     `json:"scheduleId,omitempty"`
	Source          string     `json:"source"`
	State           string     `json:"state"`
	Command         string     `json:"command"`
	ExitCode        *int       `json:"exitCode,omitempty"`
	Error           string     `json:"error,omitempty"`
	CreatedAt       time.Time  `json:"createdAt"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	StdoutBytes     int64      `json:"stdoutBytes"`
	StderrBytes     int64      `json:"stderrBytes"`
	OutputTruncated bool       `json:"outputTruncated"`
}

// JobStream is one bounded stream of a job's output: its head, its tail, the byte total the command
// produced and whether anything between them was cut.
type JobStream struct {
	Head      string `json:"head"`
	Tail      string `json:"tail"`
	Total     int64  `json:"total"`
	Truncated bool   `json:"truncated"`
	// Omitted is how many bytes between the head and the tail were cut.
	Omitted int64 `json:"omitted"`
}

// JobOutput is a job's output as it stands: a running job's partial output is readable.
type JobOutput struct {
	JobID  string    `json:"jobId"`
	State  string    `json:"state"`
	Stdout JobStream `json:"stdout"`
	Stderr JobStream `json:"stderr"`
}

// JobQuery selects jobs for Jobs. Every zero field is unset; States matches any of the named states.
type JobQuery struct {
	SessionID string   `json:"sessionId,omitempty"`
	States    []string `json:"states,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}

// JobRequest submits a background command through the Engine API. The workspace is the session's agent
// workspace, or the Engine's when no session is named. TimeoutSeconds 0 leaves the job unbounded.
type JobRequest struct {
	SessionID      string `json:"sessionId,omitempty"`
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

// jobInfo maps a stored job to the API type.
func jobInfo(job session.Job) JobInfo {
	info := JobInfo{
		ID:              job.ID,
		SessionID:       job.SessionID,
		RunID:           job.RunID,
		ScheduleID:      job.ScheduleID,
		Source:          job.Source,
		State:           job.State,
		Command:         job.Command,
		ExitCode:        job.ExitCode,
		Error:           job.Error,
		CreatedAt:       job.CreatedAt,
		StdoutBytes:     job.StdoutBytes,
		StderrBytes:     job.StderrBytes,
		OutputTruncated: job.OutputTruncated,
	}
	if !job.StartedAt.IsZero() {
		started := job.StartedAt
		info.StartedAt = &started
	}
	if !job.FinishedAt.IsZero() {
		finished := job.FinishedAt
		info.FinishedAt = &finished
	}
	return info
}

// openJobs creates the Engine's job manager: the durable queue and the worker pool of this process. It
// is started after the run reconciliation, so a session whose run died is settled before its jobs are
// recovered, and before the Engine accepts a submission.
func (e *Engine) openJobs() (*jobs.Manager, error) {
	manager, err := jobs.New(jobs.Options{
		Store:    e.store,
		StateDir: e.repoDir,
		Limits:   e.executionLimits,
		Logger:   e.log,
		Notify:   func(job session.Job) { e.jobStateChanged(job) },
	})
	if err != nil {
		return nil, err
	}
	return manager, nil
}

// jobStateChanged publishes one committed job transition on the event bus and records it in the
// journal. The journal row is projected through the content whitelist, so it carries identifiers, the
// source, the state and the exit code only, never the command or its output.
func (e *Engine) jobStateChanged(job session.Job) {
	payload := map[string]any{
		"jobId":      job.ID,
		"sessionId":  job.SessionID,
		"runId":      job.RunID,
		"scheduleId": job.ScheduleID,
		"source":     job.Source,
		"state":      job.State,
	}
	if job.ExitCode != nil {
		payload["exitCode"] = *job.ExitCode
	}
	e.events.publish("job.state", job.SessionID, payload)
	_ = e.store.Journal(context.Background(), "job.state", payload)
}

// Jobs lists jobs newest first. A zero query lists the most recent jobs of the workspace.
func (e *Engine) Jobs(ctx context.Context, query JobQuery) ([]JobInfo, error) {
	if e.jobs == nil {
		return nil, ErrJobsUnavailable
	}
	jobs, err := e.jobs.List(ctx, session.JobFilter{
		SessionID: query.SessionID, States: query.States, Limit: query.Limit,
	})
	if err != nil {
		return nil, err
	}
	out := make([]JobInfo, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, jobInfo(job))
	}
	return out, nil
}

// Job reads one job.
func (e *Engine) Job(ctx context.Context, id string) (JobInfo, error) {
	if e.jobs == nil {
		return JobInfo{}, ErrJobsUnavailable
	}
	job, err := e.jobs.Get(ctx, id)
	if err != nil {
		return JobInfo{}, err
	}
	return jobInfo(job), nil
}

// JobOutput reads a job's bounded output, head and tail per stream, while it runs or after it ended.
func (e *Engine) JobOutput(ctx context.Context, id string) (JobOutput, error) {
	if e.jobs == nil {
		return JobOutput{}, ErrJobsUnavailable
	}
	out, err := e.jobs.Output(ctx, id, 64<<10)
	if err != nil {
		return JobOutput{}, err
	}
	return JobOutput{
		JobID:  out.JobID,
		State:  out.State,
		Stdout: JobStream{Head: out.Stdout.Head, Tail: out.Stdout.Tail, Total: out.Stdout.Total, Truncated: out.Stdout.Truncated, Omitted: out.Stdout.Omitted},
		Stderr: JobStream{Head: out.Stderr.Head, Tail: out.Stderr.Tail, Total: out.Stderr.Total, Truncated: out.Stderr.Truncated, Omitted: out.Stderr.Omitted},
	}, nil
}

// SubmitJob queues a background command submitted by the host. It returns as soon as the row is
// committed: the job runs on this Engine's workers, in the session's agent workspace.
func (e *Engine) SubmitJob(ctx context.Context, req JobRequest) (JobInfo, error) {
	if e.jobs == nil {
		return JobInfo{}, ErrJobsUnavailable
	}
	workspace := e.workspace
	if req.SessionID != "" {
		agentWorkspace, err := e.agentWorkspace(ctx, req.SessionID)
		if err != nil {
			return JobInfo{}, err
		}
		workspace = agentWorkspace
	}
	if req.TimeoutSeconds < 0 || req.TimeoutSeconds > config.MaxShellTimeoutSeconds {
		return JobInfo{}, fmt.Errorf("timeoutSeconds must be between 0 and %d", config.MaxShellTimeoutSeconds)
	}
	job, err := e.jobs.Submit(ctx, jobs.Spec{
		Command:   req.Command,
		Workspace: workspace,
		SessionID: req.SessionID,
		Source:    session.JobSourceHost,
		Timeout:   time.Duration(req.TimeoutSeconds) * time.Second,
	})
	if err != nil {
		return JobInfo{}, err
	}
	return jobInfo(job), nil
}

// CancelJob ends a job: a queued job never starts, a running one has its process group killed. A
// terminal job is returned unchanged.
func (e *Engine) CancelJob(ctx context.Context, id string) (JobInfo, error) {
	if e.jobs == nil {
		return JobInfo{}, ErrJobsUnavailable
	}
	job, err := e.jobs.Cancel(ctx, id)
	if err != nil {
		return JobInfo{}, err
	}
	return jobInfo(job), nil
}

// ActiveJobsForRun reports a run's queued and running jobs. The run's completion gate uses it to refuse
// a final answer while a job is unobserved; the run's end cancels them through CancelRunJobs.
func (e *Engine) ActiveJobsForRun(ctx context.Context, runID string) ([]JobInfo, error) {
	if e.jobs == nil {
		return nil, ErrJobsUnavailable
	}
	active, err := e.jobs.ActiveForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]JobInfo, 0, len(active))
	for _, job := range active {
		out = append(out, jobInfo(job))
	}
	return out, nil
}

// CancelRunJobs cancels every queued and running job of a run, with reason recorded on the cancelled
// jobs, and reports how many it moved. A run that ends any other way than completion calls it.
func (e *Engine) CancelRunJobs(ctx context.Context, runID, reason string) (int, error) {
	if e.jobs == nil {
		return 0, ErrJobsUnavailable
	}
	return e.jobs.CancelRun(ctx, runID, reason)
}

// SubmitScheduled queues a scheduled task's command (ADR-0019 §4). The job runs with source schedule
// and is not tied to a run, so the scheduler can see whether its previous firing is still active.
func (e *Engine) SubmitScheduled(ctx context.Context, scheduleID, sessionID, command string, timeout time.Duration) (string, error) {
	if e.jobs == nil {
		return "", ErrJobsUnavailable
	}
	workspace := e.workspace
	if sessionID != "" {
		agentWorkspace, err := e.agentWorkspace(ctx, sessionID)
		if err != nil {
			return "", err
		}
		workspace = agentWorkspace
	}
	job, err := e.jobs.Submit(ctx, jobs.Spec{
		Command:    command,
		Workspace:  workspace,
		SessionID:  sessionID,
		ScheduleID: scheduleID,
		Source:     session.JobSourceSchedule,
		Timeout:    timeout,
	})
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

// JobActive reports whether a job is queued or running: the scheduler skips an occurrence while the
// previous firing's job is still active. A job that no longer exists (job retention deleted the
// finished row, or its session was deleted) is not active.
func (e *Engine) JobActive(ctx context.Context, id string) (bool, error) {
	if e.jobs == nil {
		return false, ErrJobsUnavailable
	}
	job, err := e.jobs.Get(ctx, id)
	if errors.Is(err, session.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !session.JobTerminal(job.State), nil
}

// ErrJobsUnavailable reports that the Engine has no job manager: it is closed, or was opened without
// one.
var ErrJobsUnavailable = errors.New("job manager is not available")

// runJobs is the job controller one run's tools see: it binds the run's session, run id and workspace,
// and refuses a job of another session.
type runJobs struct {
	manager   *jobs.Manager
	sessionID string
	runID     string
	workspace string
	logger    *slog.Logger
}

// Submit queues a background command of this run. The tool call that started it is recorded on the row.
func (r runJobs) Submit(ctx context.Context, command string, timeout time.Duration) (toolset.JobInfo, error) {
	job, err := r.manager.Submit(ctx, jobs.Spec{
		Command:    command,
		Workspace:  r.workspace,
		SessionID:  r.sessionID,
		RunID:      r.runID,
		ToolCallID: protocol.ToolCallID(ctx),
		Source:     session.JobSourceAgent,
		Timeout:    timeout,
	})
	if err != nil {
		r.log(ctx, "job.submit_failed", nil, err)
		return toolset.JobInfo{}, err
	}
	r.log(ctx, "job.submitted", map[string]any{"job_id": job.ID, "timeout_ms": job.Timeout.Milliseconds()}, nil)
	return runJobInfo(job), nil
}

// Get reads one of this session's jobs; a job of another session is refused as unknown.
func (r runJobs) Get(ctx context.Context, id string) (toolset.JobInfo, error) {
	job, err := r.own(ctx, id)
	if err != nil {
		return toolset.JobInfo{}, err
	}
	return runJobInfo(job), nil
}

// Output reads a job's bounded output.
func (r runJobs) Output(ctx context.Context, id string) (toolset.JobOutput, error) {
	if _, err := r.own(ctx, id); err != nil {
		return toolset.JobOutput{}, err
	}
	out, err := r.manager.Output(ctx, id, 32<<10)
	if err != nil {
		return toolset.JobOutput{}, err
	}
	return toolset.JobOutput{
		Stdout: toolset.JobStream{Head: out.Stdout.Head, Tail: out.Stdout.Tail, Total: out.Stdout.Total, Truncated: out.Stdout.Truncated, Omitted: out.Stdout.Omitted},
		Stderr: toolset.JobStream{Head: out.Stderr.Head, Tail: out.Stderr.Tail, Total: out.Stderr.Total, Truncated: out.Stderr.Truncated, Omitted: out.Stderr.Omitted},
	}, nil
}

// Cancel ends one of this session's jobs.
func (r runJobs) Cancel(ctx context.Context, id string) (toolset.JobInfo, error) {
	if _, err := r.own(ctx, id); err != nil {
		return toolset.JobInfo{}, err
	}
	job, err := r.manager.Cancel(ctx, id)
	if err != nil {
		return toolset.JobInfo{}, err
	}
	r.log(ctx, "job.cancel", map[string]any{"job_id": job.ID, "state": job.State}, nil)
	return runJobInfo(job), nil
}

// List reads this session's recent jobs, newest first.
func (r runJobs) List(ctx context.Context, limit int) ([]toolset.JobInfo, error) {
	jobs, err := r.manager.List(ctx, session.JobFilter{SessionID: r.sessionID, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]toolset.JobInfo, 0, len(jobs))
	for _, job := range jobs {
		out = append(out, runJobInfo(job))
	}
	return out, nil
}

// Active reads this run's queued and running jobs.
func (r runJobs) Active(ctx context.Context) ([]toolset.JobInfo, error) {
	active, err := r.manager.ActiveForRun(ctx, r.runID)
	if err != nil {
		return nil, err
	}
	out := make([]toolset.JobInfo, 0, len(active))
	for _, job := range active {
		out = append(out, runJobInfo(job))
	}
	return out, nil
}

// Wait blocks until one of the named jobs is terminal, the timeout passes or ctx ends. The outcome is
// logged: a run that spent its budget waiting is visible in the diagnostics log, with the job that
// ended (if any) and how long the wait took.
func (r runJobs) Wait(ctx context.Context, ids []string, timeout time.Duration) (toolset.JobInfo, bool, error) {
	for _, id := range ids {
		if _, err := r.own(ctx, id); err != nil {
			return toolset.JobInfo{}, false, err
		}
	}
	started := time.Now()
	job, ok, err := r.manager.Wait(ctx, ids, timeout)
	if err != nil {
		return toolset.JobInfo{}, false, err
	}
	if r.logger != nil {
		attrs := map[string]any{"waited_ms": time.Since(started).Milliseconds(), "timed_out": !ok}
		if ok {
			attrs["job_id"], attrs["state"] = job.ID, job.State
		}
		r.log(ctx, "job.wait", attrs, nil)
	}
	return runJobInfo(job), ok, nil
}

// own reads a job and refuses one that belongs to another session.
func (r runJobs) own(ctx context.Context, id string) (session.Job, error) {
	job, err := r.manager.Get(ctx, id)
	if err != nil {
		return session.Job{}, err
	}
	if job.SessionID != r.sessionID {
		return session.Job{}, fmt.Errorf("job %s belongs to another session", id)
	}
	return job, nil
}

func (r runJobs) log(ctx context.Context, event string, attrs map[string]any, err error) {
	if r.logger == nil {
		return
	}
	fields := []any{"session_id", r.sessionID, "run_id", r.runID, "tool_call_id", protocol.ToolCallID(ctx), "event", event}
	for key, value := range attrs {
		fields = append(fields, key, value)
	}
	if err != nil {
		fields = append(fields, "error", err)
		r.logger.Warn("job tool", fields...)
		return
	}
	r.logger.Info("job tool", fields...)
}

// runJobInfo maps a stored job to the toolset's view.
func runJobInfo(job session.Job) toolset.JobInfo {
	return toolset.JobInfo{
		ID:         job.ID,
		State:      job.State,
		Command:    job.Command,
		ExitCode:   job.ExitCode,
		Error:      job.Error,
		CreatedAt:  job.CreatedAt,
		StartedAt:  job.StartedAt,
		FinishedAt: job.FinishedAt,
	}
}

// runJobs binds the job queue to one run: the tools may submit, read and cancel the jobs of this
// session, and a job of another session is refused.
func (e *Engine) runJobs(sessionID, runID, workspace string) toolset.JobController {
	if e.jobs == nil {
		return nil
	}
	return runJobs{
		manager:   e.jobs,
		sessionID: sessionID,
		runID:     runID,
		workspace: workspace,
		logger:    e.jobs.Logger(),
	}
}

// runJobGate is the agent loop's view of the run's active jobs, for the completion gate (ADR-0019 §3).
type runJobGate struct{ manager *jobs.Manager }

func (g runJobGate) ActiveJobs(ctx context.Context, runID string) ([]agent.ActiveJob, error) {
	active, err := g.manager.ActiveForRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	out := make([]agent.ActiveJob, 0, len(active))
	for _, job := range active {
		out = append(out, agent.ActiveJob{ID: job.ID, State: job.State, Command: job.Command})
	}
	return out, nil
}

// jobGate is the run's job view, nil when this Engine has no job queue.
func (e *Engine) jobGate() agent.RunJobs {
	if e.jobs == nil {
		return nil
	}
	return runJobGate{manager: e.jobs}
}

// endRunJobs cancels the jobs a run left queued or running when it ended, however it ended (ADR-0019
// §3): no job outlives the run that wanted it unobserved. It runs after the loop returned, on a context
// of its own, so a cancelled run still cancels its jobs.
func (e *Engine) endRunJobs(ctx context.Context, sessionID, runID string) {
	if e.jobs == nil || runID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	n, err := e.jobs.CancelRun(ctx, runID, "run_ended")
	if err != nil {
		e.events.publish("jobs.cancel_failed", sessionID, map[string]any{"runId": runID, "error": err.Error()})
		return
	}
	if n > 0 {
		e.events.publish("jobs.run_ended", sessionID, map[string]any{"runId": runID, "cancelled": n})
	}
}

// The run-start row of the session's open background jobs (ADR-0019 §3): queued and running jobs (a
// host's, a schedule's, another process's) and jobs whose owner died (outcome_unknown, never re-run).
// It names them so a resumed run checks the runtime's state instead of trusting its transcript.
const (
	maxJobsIndexEntries = 8
	maxJobsIndexBytes   = 600
	jobsIndexPrefix     = "[runtime] Background jobs of this session that are open or ended without a known outcome: "
	jobsIndexTail       = ". Check a job with job_status before you rely on, wait for or repeat its work; an outcome_unknown job was not finished and is never re-run."
)

// jobsIndexStates are the job states the row names.
var jobsIndexStates = []string{session.JobQueued, session.JobRunning, session.JobOutcomeUnknown}

// jobsIndexRow is the row a run appends when its session has such jobs; ok is false when it has none.
func jobsIndexRow(open []session.Job) (protocol.Message, bool) {
	if len(open) == 0 {
		return protocol.Message{}, false
	}
	entries := make([]string, 0, min(len(open), maxJobsIndexEntries))
	for i, job := range open {
		if i == maxJobsIndexEntries {
			break
		}
		command := job.Command
		if len(command) > 48 {
			command = truncateUTF8(command, 45) + "..."
		}
		entries = append(entries, fmt.Sprintf("%s %s (%s)", job.ID, job.State, command))
	}
	for n := len(entries); n >= 1; n-- {
		line := jobsIndexPrefix + strings.Join(entries[:n], "; ")
		if remaining := len(open) - n; remaining > 0 {
			line += fmt.Sprintf("; and %d more (job_list)", remaining)
		}
		line += jobsIndexTail
		if len(line) <= maxJobsIndexBytes {
			return protocol.Message{Role: "user", Origin: protocol.OriginJobs, Content: line}, true
		}
	}
	return protocol.Message{Role: "user", Origin: protocol.OriginJobs, Content: truncateUTF8(jobsIndexPrefix+entries[0]+jobsIndexTail, maxJobsIndexBytes)}, true
}
