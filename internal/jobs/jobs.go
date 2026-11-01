// Package jobs runs the background commands of ADR-0019 §3: a durable queue in the session database, a
// fixed worker pool per process, bounded output on disk, and a recovery rule that never re-runs a
// command whose owner died while it ran.
//
// The jobs table is the queue's source of truth. Every state change is one UPDATE guarded by the state
// it leaves, so two writers (two workers, two Engines on one database) never both move a job, and a
// restart finds every job where its last committed transition left it.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/session"
)

// Job is one row of the jobs table; the manager's API speaks it directly.
type Job = session.Job

// MaxActivePerRun bounds the queued and running jobs of one run (ADR-0019 §3).
const MaxActivePerRun = 8

// maxCommandBytes bounds a job's command, matching the jobs table's CHECK.
const maxCommandBytes = 65536

// killGrace is the time a job that reached its timeout gets between SIGTERM and SIGKILL.
const killGrace = 2 * time.Second

// retentionInterval is how often an open manager sweeps finished jobs.
const retentionInterval = time.Hour

// flushEvery bounds how much of the tail buffer is written to disk between flushes: the head is
// flushed on every write, so a reader sees a job's first output at once.
const flushEvery = 64 << 10

// The kinds of stop a running job can be asked for. The kind decides the signal and the final state.
const (
	stopCancel = "cancel"
	stopTimed  = "timeout"
	stopClosed = "closed"
)

// Errors the manager returns. ErrQueueFull and ErrRunJobLimit come from the store's transactional
// check; ErrClosed reports that the manager no longer accepts or starts work.
var (
	ErrQueueFull   = session.ErrJobQueueFull
	ErrRunJobLimit = session.ErrJobRunLimit
	ErrClosed      = errors.New("job manager is closed")
)

// Store is the part of the session store the manager uses. The SQL stays in internal/session.
type Store interface {
	InsertJob(ctx context.Context, job session.Job, maxQueued, maxRunActive int) error
	GetJob(ctx context.Context, id string) (session.Job, error)
	ListJobs(ctx context.Context, filter session.JobFilter) ([]session.Job, error)
	QueuedJob(ctx context.Context, runnerID string) (session.Job, error)
	UpdateJob(ctx context.Context, id, from string, change session.JobChange) (bool, error)
	ActiveJobsForRun(ctx context.Context, runID string) ([]session.Job, error)
	JobRunners(ctx context.Context) ([]session.JobRunner, error)
	AdoptQueuedJobs(ctx context.Context, fromRunnerID, toRunnerID string, toPID int) (int64, error)
	DeleteFinishedJobsBefore(ctx context.Context, cutoff time.Time) ([]string, error)
}

// Options configures a Manager. Limits is read at Start, on every submission and on every sweep, so a
// saved configuration change applies to the next job without reopening the Engine. A nil Logger
// discards. Notify runs after every committed transition, on the goroutine that made it, and must not
// block.
type Options struct {
	Store    Store
	StateDir string
	Limits   func() config.Execution
	Logger   *slog.Logger
	Notify   func(Job)
	Now      func() time.Time
}

// Spec is one submission. Source defaults to host; SessionID may be empty for a host job. Timeout 0
// means the job may run until it exits or is cancelled.
type Spec struct {
	Command    string
	Workspace  string
	SessionID  string
	RunID      string
	ToolCallID string
	ScheduleID string
	Source     string
	Timeout    time.Duration
}

// Filter selects jobs for List.
type Filter = session.JobFilter

// Manager is the process's job runner: one worker pool, the jobs it is running, and the recovery and
// retention passes of the queue it shares with every other process on the database.
type Manager struct {
	store    Store
	stateDir string
	limits   func() config.Execution
	logger   *slog.Logger
	notify   func(Job)
	now      func() time.Time

	runnerID  string
	runnerPID int

	mu      sync.Mutex
	closed  bool
	started bool
	active  map[string]*runningJob
	// pending holds a stop asked for a job a worker claimed but has not registered yet: the worker
	// reads it before it starts the command, so a cancel in that window still kills the command.
	pending     map[string]*stopReason
	watchers    map[uint64]*watcher
	nextWatcher uint64
	queue       chan struct{}
	stop        chan struct{}
	wg          sync.WaitGroup
}

type watcher struct {
	ids map[string]bool
	ch  chan struct{}
}

// stopReason is why a running job was asked to end: the kind decides the signal and the final state,
// the message is recorded on the row.
type stopReason struct {
	kind    string
	message string
}

type runningJob struct {
	jobID     string
	output    *jobOutput
	cmd       *execCommand
	pid       int
	stop      *stopReason
	timeout   *time.Timer
	killTimer *time.Timer
	done      bool
}

// New returns a manager that has not started any work yet: call Start to recover the queue and open
// the workers.
func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("jobs: a store is required")
	}
	if opts.StateDir == "" {
		return nil, errors.New("jobs: a state directory is required")
	}
	limits := opts.Limits
	if limits == nil {
		limits = func() config.Execution { return (&config.Execution{}).Limits() }
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	notify := opts.Notify
	if notify == nil {
		notify = func(Job) {}
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	runnerID, err := newRunnerID()
	if err != nil {
		return nil, err
	}
	return &Manager{
		store:     opts.Store,
		stateDir:  opts.StateDir,
		limits:    limits,
		logger:    logger,
		notify:    notify,
		now:       now,
		runnerID:  runnerID,
		runnerPID: os.Getpid(),
		active:    map[string]*runningJob{},
		pending:   map[string]*stopReason{},
		watchers:  map[uint64]*watcher{},
		stop:      make(chan struct{}),
	}, nil
}

// Logger is the manager's diagnostics logger; it discards records when the host passed none.
func (m *Manager) Logger() *slog.Logger { return m.logger }

// RunnerID is this manager's identity in the jobs table.
func (m *Manager) RunnerID() string { return m.runnerID }

// Start recovers the queue and opens the worker pool. Recovery adopts the queued jobs of runners whose
// process is gone and records their running jobs as outcome_unknown: such a job may have had effects,
// so it is never run again. The pool size is execution.maxConcurrentJobs as it is now; a later change
// applies to the next Start, that is the next Engine open.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	if m.started {
		m.mu.Unlock()
		return nil
	}
	m.started = true
	workers := m.limits().MaxConcurrentJobs
	if workers < 1 {
		workers = 1
	}
	m.queue = make(chan struct{}, workers)
	m.mu.Unlock()

	if err := m.recover(ctx); err != nil {
		return err
	}
	if err := m.Sweep(ctx); err != nil {
		m.logger.Warn("job retention sweep failed", "error", err)
	}
	for range workers {
		m.wg.Add(1)
		go m.worker()
	}
	m.signal(workers)
	m.wg.Add(1)
	go m.retentionLoop()
	return nil
}

// recover settles the jobs of dead owners. A runner is dead when its process is gone (signal 0 fails);
// this manager's own runner id is always live.
func (m *Manager) recover(ctx context.Context) error {
	runners, err := m.store.JobRunners(ctx)
	if err != nil {
		return err
	}
	now := m.now()
	for _, runner := range runners {
		if runner.ID == m.runnerID || processAlive(runner.PID) {
			continue
		}
		running, err := m.store.ListJobs(ctx, session.JobFilter{
			RunnerID: runner.ID, States: []string{session.JobRunning}, Limit: 500,
		})
		if err != nil {
			return err
		}
		for _, job := range running {
			reason := "the process that ran this job exited before recording how it ended"
			moved, err := m.store.UpdateJob(ctx, job.ID, session.JobRunning, session.JobChange{
				State: session.JobOutcomeUnknown, Error: &reason, FinishedAt: &now,
			})
			if err != nil {
				return err
			}
			if moved {
				m.emit(job.ID)
			}
		}
		if _, err := m.store.AdoptQueuedJobs(ctx, runner.ID, m.runnerID, m.runnerPID); err != nil {
			return err
		}
	}
	return nil
}

// Submit validates and commits a queued job. The row is committed before any worker can see it, so a
// crash after Submit never loses the job. The queue limit is global (execution.maxQueuedJobs) and the
// run's active limit is MaxActivePerRun; both are counted in the insert's transaction.
func (m *Manager) Submit(ctx context.Context, spec Spec) (Job, error) {
	if err := validate(spec); err != nil {
		return Job{}, err
	}
	limits := m.limits()
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return Job{}, ErrClosed
	}
	source := spec.Source
	if source == "" {
		source = session.JobSourceHost
	}
	id, err := newJobID()
	if err != nil {
		return Job{}, err
	}
	job := Job{
		ID:         id,
		Kind:       "shell",
		Source:     source,
		SessionID:  spec.SessionID,
		RunID:      spec.RunID,
		ToolCallID: spec.ToolCallID,
		ScheduleID: spec.ScheduleID,
		Workspace:  spec.Workspace,
		Command:    spec.Command,
		Timeout:    spec.Timeout,
		State:      session.JobQueued,
		RunnerID:   m.runnerID,
		RunnerPID:  m.runnerPID,
		CreatedAt:  m.now(),
	}
	if err := m.store.InsertJob(ctx, job, limits.MaxQueuedJobs, MaxActivePerRun); err != nil {
		return Job{}, err
	}
	m.emit(id)
	m.signal(1)
	return job, nil
}

func validate(spec Spec) error {
	if spec.Command == "" {
		return errors.New("job command is required")
	}
	if len(spec.Command) > maxCommandBytes {
		return fmt.Errorf("job command is longer than %d bytes", maxCommandBytes)
	}
	if spec.Workspace == "" {
		return errors.New("job workspace is required")
	}
	info, err := os.Stat(spec.Workspace)
	if err != nil {
		return fmt.Errorf("job workspace: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("job workspace %s is not a directory", spec.Workspace)
	}
	switch spec.Source {
	case "", session.JobSourceAgent, session.JobSourceHost, session.JobSourceSchedule:
	default:
		return fmt.Errorf("unknown job source %q", spec.Source)
	}
	if spec.Timeout < 0 {
		return errors.New("job timeout must not be negative")
	}
	return nil
}

// Get reads one job.
func (m *Manager) Get(ctx context.Context, id string) (Job, error) { return m.store.GetJob(ctx, id) }

// List reads jobs newest first.
func (m *Manager) List(ctx context.Context, filter Filter) ([]Job, error) {
	return m.store.ListJobs(ctx, filter)
}

// ActiveForRun reads a run's queued and running jobs, newest first.
func (m *Manager) ActiveForRun(ctx context.Context, runID string) ([]Job, error) {
	return m.store.ActiveJobsForRun(ctx, runID)
}

// Output reads a job's bounded stdout and stderr: the head and the tail of each stream, the byte total
// the command produced, and whether anything was cut. It reads a running job's output as it stands, so
// a long command's progress is visible before it ends. maxBytes bounds each stream's head and tail
// together.
func (m *Manager) Output(ctx context.Context, id string, maxBytes int) (Output, error) {
	job, err := m.store.GetJob(ctx, id)
	if err != nil {
		return Output{}, err
	}
	if maxBytes <= 0 {
		maxBytes = 64 << 10
	}
	out := Output{JobID: id, State: job.State}
	m.mu.Lock()
	active := m.active[id]
	m.mu.Unlock()
	if active != nil {
		out.Stdout, out.Stderr = active.output.Read(maxBytes)
		return out, nil
	}
	dir := m.jobDir(id)
	out.Stdout = readStream(dir, "stdout", maxBytes, job.StdoutBytes, job.OutputTruncated)
	out.Stderr = readStream(dir, "stderr", maxBytes, job.StderrBytes, job.OutputTruncated)
	return out, nil
}

// Cancel ends a job. A queued job becomes cancelled at once; a running job's process group is killed
// and the job ends cancelled; a terminal job is returned unchanged.
func (m *Manager) Cancel(ctx context.Context, id string) (Job, error) {
	job, err := m.store.GetJob(ctx, id)
	if err != nil {
		return Job{}, err
	}
	if session.JobTerminal(job.State) {
		return job, nil
	}
	if job.State == session.JobQueued {
		now := m.now()
		reason := "cancelled"
		if _, err := m.store.UpdateJob(ctx, id, session.JobQueued, session.JobChange{
			State: session.JobCancelled, Error: &reason, FinishedAt: &now,
		}); err != nil {
			return Job{}, err
		}
		m.emit(id)
		return m.store.GetJob(ctx, id)
	}
	if !m.cancelRunning(ctx, job, stopReason{kind: stopCancel, message: "cancelled"}) {
		return m.store.GetJob(ctx, id)
	}
	// The worker that owns a running job records its final state; wait for it to land.
	_, _, _ = m.Wait(ctx, []string{id}, 10*time.Second)
	return m.store.GetJob(ctx, id)
}

// CancelRun cancels every queued and running job of a run, with reason recorded on the cancelled jobs,
// and reports how many it moved. A run that ends any other way than completion calls it, so no job
// outlives the run that wanted it unobserved.
func (m *Manager) CancelRun(ctx context.Context, runID, reason string) (int, error) {
	if runID == "" {
		return 0, nil
	}
	active, err := m.store.ActiveJobsForRun(ctx, runID)
	if err != nil {
		return 0, err
	}
	return m.cancelJobs(ctx, active, reason)
}

// CancelSession cancels every queued and running job of a session, as session deletion does.
func (m *Manager) CancelSession(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, nil
	}
	active, err := m.store.ListJobs(ctx, session.JobFilter{
		SessionID: sessionID, States: session.JobActiveStates, Limit: 500,
	})
	if err != nil {
		return 0, err
	}
	return m.cancelJobs(ctx, active, "session deleted")
}

func (m *Manager) cancelJobs(ctx context.Context, active []Job, reason string) (int, error) {
	moved := 0
	var stopping []string
	for _, job := range active {
		if session.JobTerminal(job.State) {
			continue
		}
		if job.State == session.JobQueued {
			now := m.now()
			ok, err := m.store.UpdateJob(ctx, job.ID, session.JobQueued, session.JobChange{
				State: session.JobCancelled, Error: &reason, FinishedAt: &now,
			})
			if err != nil {
				return moved, err
			}
			if ok {
				moved++
				m.emit(job.ID)
			}
			continue
		}
		if m.cancelRunning(ctx, job, stopReason{kind: stopCancel, message: reason}) {
			moved++
			stopping = append(stopping, job.ID)
		}
	}
	// The workers that own the running jobs record their terminal state; wait for it, so a caller that
	// cancelled a run's jobs sees them cancelled when it returns.
	if len(stopping) > 0 {
		_, _, _ = m.Wait(ctx, stopping, 10*time.Second)
	}
	return moved, nil
}

// Wait blocks until one of the named jobs is terminal, the timeout passes or ctx ends. It reports the
// job that ended and whether one did; a timeout is not an error. Jobs this manager runs wake it through
// their own committed transitions; a job of another process is re-read once a second. An empty id list
// waits for nothing.
func (m *Manager) Wait(ctx context.Context, ids []string, timeout time.Duration) (Job, bool, error) {
	if len(ids) == 0 {
		return Job{}, false, nil
	}
	w, unwatch := m.watch(ids)
	defer unwatch()
	var deadline <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		deadline = timer.C
	}
	poll := time.NewTimer(time.Second)
	defer poll.Stop()
	for {
		foreign := false
		for _, id := range ids {
			job, err := m.store.GetJob(ctx, id)
			if err != nil {
				return Job{}, false, err
			}
			if session.JobTerminal(job.State) {
				return job, true, nil
			}
			if job.RunnerID != m.runnerID {
				foreign = true
			}
		}
		var pollC <-chan time.Time
		if foreign {
			poll.Reset(time.Second)
			pollC = poll.C
		}
		select {
		case <-ctx.Done():
			return Job{}, false, ctx.Err()
		case <-m.stop:
			return Job{}, false, ErrClosed
		case <-deadline:
			return Job{}, false, nil
		case <-w.ch:
		case <-pollC:
		}
	}
}

// Sweep deletes the finished jobs that ended more than execution.jobRetentionDays ago, with their
// output, and removes the output of jobs whose row is gone (a purged session). Start runs it once and
// an open manager runs it hourly.
func (m *Manager) Sweep(ctx context.Context) error {
	limits := m.limits()
	retention := time.Duration(limits.JobRetentionDays) * 24 * time.Hour
	cutoff := m.now().Add(-retention)
	ids, err := m.store.DeleteFinishedJobsBefore(ctx, cutoff)
	if err != nil {
		return err
	}
	root := filepath.Join(m.stateDir, "jobs")
	for _, id := range ids {
		if err := os.RemoveAll(filepath.Join(root, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := m.store.GetJob(ctx, entry.Name()); !errors.Is(err, session.ErrNotFound) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// Close stops accepting work, cancels the jobs this manager runs and the ones it still had queued
// (which would otherwise wait for a process that is going away), waits for its workers and its sweep
// goroutine, and leaves no child process behind.
func (m *Manager) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	active := make([]*runningJob, 0, len(m.active))
	for _, rj := range m.active {
		active = append(active, rj)
	}
	m.mu.Unlock()
	close(m.stop)
	for _, rj := range active {
		m.terminate(rj, stopReason{kind: stopClosed, message: "engine closed"})
	}
	if err := m.cancelQueued(context.Background(), "engine closed"); err != nil {
		m.logger.Warn("cancel queued jobs on close", "error", err)
	}
	m.wg.Wait()
	return nil
}

func (m *Manager) cancelQueued(ctx context.Context, reason string) error {
	queued, err := m.store.ListJobs(ctx, session.JobFilter{
		RunnerID: m.runnerID, States: []string{session.JobQueued}, Limit: 500,
	})
	if err != nil {
		return err
	}
	for _, job := range queued {
		now := m.now()
		moved, err := m.store.UpdateJob(ctx, job.ID, session.JobQueued, session.JobChange{
			State: session.JobCancelled, Error: &reason, FinishedAt: &now,
		})
		if err != nil {
			return err
		}
		if moved {
			m.emit(job.ID)
		}
	}
	return nil
}

func (m *Manager) retentionLoop() {
	defer m.wg.Done()
	ticker := time.NewTicker(retentionInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := m.Sweep(context.Background()); err != nil {
				m.logger.Warn("job retention sweep failed", "error", err)
			}
		case <-m.stop:
			return
		}
	}
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		for {
			select {
			case <-m.stop:
				return
			default:
			}
			job, ok := m.claim()
			if !ok {
				break
			}
			m.execute(context.Background(), job)
		}
		select {
		case <-m.queue:
		case <-m.stop:
			return
		}
	}
}

// claim moves the oldest queued job of this runner to running. Two workers may read the same row; the
// compare-and-set decides, and the loser tries the next row.
func (m *Manager) claim() (Job, bool) {
	for range 8 {
		job, err := m.store.QueuedJob(context.Background(), m.runnerID)
		if err != nil {
			if !errors.Is(err, session.ErrNotFound) {
				m.logger.Warn("claim job", "error", err)
			}
			return Job{}, false
		}
		now := m.now()
		moved, err := m.store.UpdateJob(context.Background(), job.ID, session.JobQueued, session.JobChange{
			State: session.JobRunning, StartedAt: &now,
		})
		if err != nil {
			m.logger.Warn("claim job", "job_id", job.ID, "error", err)
			return Job{}, false
		}
		if !moved {
			continue
		}
		job.State = session.JobRunning
		job.StartedAt = now
		m.emit(job.ID)
		return job, true
	}
	return Job{}, false
}

// testHookAfterClaim, when set, runs between a claim and the start of the command. A test uses it to
// cancel in that window.
var testHookAfterClaim func()

// execute runs one claimed job: the command in its own process group, its output to the job's files,
// and one terminal transition when it ends.
func (m *Manager) execute(ctx context.Context, job Job) {
	if hook := testHookAfterClaim; hook != nil {
		hook()
	}
	out, err := newJobOutput(m.jobDir(job.ID), m.limits().JobOutputBytes)
	if err != nil {
		m.finish(job, nil, session.JobFailed, intPtr(-1), "job output: "+err.Error())
		return
	}
	rj := &runningJob{jobID: job.ID, output: out}
	m.mu.Lock()
	if m.closed {
		delete(m.pending, job.ID)
		m.mu.Unlock()
		_ = out.Close()
		m.finish(job, nil, session.JobCancelled, intPtr(-1), "engine closed")
		return
	}
	if pending := m.pending[job.ID]; pending != nil {
		delete(m.pending, job.ID)
		rj.stop = pending
	}
	m.active[job.ID] = rj
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		rj.done = true
		delete(m.active, job.ID)
		delete(m.pending, job.ID)
		timeout, killTimer := rj.timeout, rj.killTimer
		m.mu.Unlock()
		if timeout != nil {
			timeout.Stop()
		}
		if killTimer != nil {
			killTimer.Stop()
		}
	}()

	cmd, err := startCommand(job.Workspace, job.Command, out.stdout, out.stderr)
	if err != nil {
		_ = out.Close()
		m.finish(job, nil, session.JobFailed, intPtr(-1), "start: "+err.Error())
		return
	}
	pid := cmd.Pid()
	m.mu.Lock()
	rj.cmd, rj.pid = cmd, pid
	stop := rj.stop
	if stop == nil && job.Timeout > 0 {
		rj.timeout = time.AfterFunc(job.Timeout, func() {
			m.terminate(rj, stopReason{kind: stopTimed, message: timedOutMessage(job.Timeout)})
		})
	}
	m.mu.Unlock()
	if stop != nil {
		// Asked to stop between the claim and the start: kill it now.
		signalGroup(cmd, pid, sigKill)
	}
	if _, err := m.store.UpdateJob(ctx, job.ID, session.JobRunning, session.JobChange{
		State: session.JobRunning, PID: &pid,
	}); err != nil {
		m.logger.Warn("record job pid", "job_id", job.ID, "error", err)
	}

	waitErr := cmd.Wait()
	_ = out.Close()
	m.mu.Lock()
	stop = rj.stop
	m.mu.Unlock()

	exitCode := -1
	if state := cmd.ProcessState(); state != nil {
		exitCode = state.ExitCode()
	}
	switch {
	case stop != nil && stop.kind == stopTimed:
		m.finish(job, out, session.JobTimedOut, intPtr(-1), stop.message)
	case stop != nil:
		m.finish(job, out, session.JobCancelled, intPtr(-1), stop.message)
	case waitErr == nil:
		m.finish(job, out, session.JobCompleted, intPtr(0), "")
	default:
		m.finish(job, out, session.JobFailed, intPtr(exitCode), waitErr.Error())
	}
}

func timedOutMessage(timeout time.Duration) string {
	return fmt.Sprintf("job timed out after %s; its process group was terminated and its effects are unknown", timeout)
}

// finish commits a job's terminal state with its output totals. A job another writer already moved
// stays where that writer put it.
func (m *Manager) finish(job Job, out *jobOutput, state string, exitCode *int, errText string) {
	now := m.now()
	change := session.JobChange{State: state, ExitCode: exitCode, Error: &errText, FinishedAt: &now}
	if out != nil {
		stdoutTotal, stderrTotal, truncated := out.Totals()
		change.StdoutBytes = &stdoutTotal
		change.StderrBytes = &stderrTotal
		change.OutputTruncated = &truncated
	}
	moved, err := m.store.UpdateJob(context.Background(), job.ID, session.JobRunning, change)
	if err != nil {
		m.logger.Error("finish job", "job_id", job.ID, "error", err)
		return
	}
	if !moved {
		m.logger.Warn("job was already moved by another writer", "job_id", job.ID)
		return
	}
	m.emit(job.ID)
}

// cancelRunning stops a running job; it reports whether this caller moved it. A job this manager runs
// is killed here and its worker records the state. A job another live process runs has its recorded
// process group killed and its row moved here; a job whose owner is gone is left for recovery.
func (m *Manager) cancelRunning(ctx context.Context, job Job, stop stopReason) bool {
	m.mu.Lock()
	rj := m.active[job.ID]
	if rj != nil {
		m.mu.Unlock()
		m.terminate(rj, stop)
		return true
	}
	if job.RunnerID == m.runnerID {
		// A worker claimed the job and has not registered it yet: it reads this before it starts the
		// command, so the cancel still reaches the process group.
		m.pending[job.ID] = &stop
		m.mu.Unlock()
		return true
	}
	m.mu.Unlock()
	if !processAlive(job.RunnerPID) {
		return false
	}
	if job.PID > 0 {
		_ = signalGroup(nil, job.PID, sigKill)
	}
	now := m.now()
	moved, err := m.store.UpdateJob(ctx, job.ID, session.JobRunning, session.JobChange{
		State: session.JobCancelled, Error: &stop.message, FinishedAt: &now,
	})
	if err != nil {
		m.logger.Warn("cancel running job", "job_id", job.ID, "error", err)
		return false
	}
	if moved {
		m.emit(job.ID)
	}
	return moved
}

// terminate asks a running job to stop. A timeout sends SIGTERM to the group, then SIGKILL after the
// grace period; every other kind kills the group at once. Only the first stop reason counts.
func (m *Manager) terminate(rj *runningJob, stop stopReason) {
	m.mu.Lock()
	if rj.done || rj.stop != nil {
		m.mu.Unlock()
		return
	}
	rj.stop = &stop
	cmd, pid := rj.cmd, rj.pid
	if stop.kind == stopTimed && (pid > 0 || cmd != nil) {
		rj.killTimer = time.AfterFunc(killGrace, func() {
			m.mu.Lock()
			done := rj.done
			m.mu.Unlock()
			if !done {
				signalGroup(cmd, pid, sigKill)
			}
		})
	}
	m.mu.Unlock()
	if stop.kind == stopTimed {
		signalGroup(cmd, pid, sigTerm)
		return
	}
	signalGroup(cmd, pid, sigKill)
}

// emit reads a job back after a committed transition, logs it with its correlation fields, and tells
// the subscriber and the waiters. The log record carries identifiers, the state and the duration, never
// the command or its output.
func (m *Manager) emit(id string) {
	job, err := m.store.GetJob(context.Background(), id)
	if err != nil {
		m.logger.Warn("read job after transition", "job_id", id, "error", err)
		return
	}
	m.logTransition(job)
	m.notify(job)
	m.wake(job)
}

// logTransition records one committed state change with the diagnostics correlation fields.
func (m *Manager) logTransition(job Job) {
	attrs := []any{
		"job_id", job.ID,
		"state", job.State,
		"source", job.Source,
		"session_id", job.SessionID,
		"run_id", job.RunID,
		"schedule_id", job.ScheduleID,
	}
	if !job.StartedAt.IsZero() {
		end := job.FinishedAt
		if end.IsZero() {
			end = m.now()
		}
		attrs = append(attrs, "duration_ms", end.Sub(job.StartedAt).Milliseconds())
	}
	if job.ExitCode != nil {
		attrs = append(attrs, "exit_code", *job.ExitCode)
	}
	switch job.State {
	case session.JobFailed:
		attrs = append(attrs, "error_code", "job_failed")
	case session.JobTimedOut:
		attrs = append(attrs, "error_code", "job_timed_out")
	case session.JobOutcomeUnknown:
		attrs = append(attrs, "error_code", "job_outcome_unknown")
	}
	m.logger.Info("job.state", attrs...)
}

func (m *Manager) signal(n int) {
	m.mu.Lock()
	queue := m.queue
	m.mu.Unlock()
	if queue == nil {
		return
	}
	for range n {
		select {
		case queue <- struct{}{}:
		default:
			return
		}
	}
}

func (m *Manager) watch(ids []string) (*watcher, func()) {
	w := &watcher{ids: make(map[string]bool, len(ids)), ch: make(chan struct{}, 1)}
	for _, id := range ids {
		w.ids[id] = true
	}
	m.mu.Lock()
	m.nextWatcher++
	id := m.nextWatcher
	m.watchers[id] = w
	m.mu.Unlock()
	return w, func() {
		m.mu.Lock()
		delete(m.watchers, id)
		m.mu.Unlock()
	}
}

func (m *Manager) wake(job Job) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.watchers {
		if !w.ids[job.ID] {
			continue
		}
		select {
		case w.ch <- struct{}{}:
		default:
		}
	}
}

func (m *Manager) jobDir(id string) string { return filepath.Join(m.stateDir, "jobs", id) }

// intPtr takes the address of an int for a JobChange field; the module targets Go 1.23, which has no
// new(expr).
func intPtr(value int) *int { return &value }

func newRunnerID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("job runner id: %w", err)
	}
	return "runner_" + hex.EncodeToString(buf), nil
}

func newJobID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("job id: %w", err)
	}
	return "job_" + hex.EncodeToString(buf), nil
}
