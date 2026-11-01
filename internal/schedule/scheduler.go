package schedule

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// ErrClosed reports an operation on a scheduler that was closed.
var ErrClosed = errors.New("scheduler is closed")

// Options configures a Scheduler. Only Store and Actions are required.
type Options struct {
	// Store is the scheduled_tasks table (session.Store).
	Store Store
	// Actions runs a firing: the job queue for a shell task, the RunSupervisor for an agent task.
	Actions Actions
	// Logger receives the scheduler's warnings; nil discards them.
	Logger *slog.Logger
	// Notify is called after a firing or a missed occurrence is recorded, with the event word
	// ("fired" or "missed") and the task as it now stands. It must not block: the scheduler calls it
	// on its own goroutine.
	Notify func(event string, t Task)
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// MinInterval is the shortest interval a task may have; 0 means DefaultMinEvery (60 s).
	MinInterval time.Duration
}

// Scheduler fires the workspace's due scheduled tasks: one goroutine, one time.Timer reset to the
// earliest due task, and a compare-and-set claim per occurrence so two processes sharing the database
// never run the same occurrence. Firings are serial in this process, so one task never runs twice at
// once here; Actions.Active covers the case where the previous firing is still running.
type Scheduler struct {
	store       Store
	actions     Actions
	log         *slog.Logger
	notify      func(string, Task)
	clock       func() time.Time
	minInterval time.Duration

	mu      sync.Mutex
	started bool
	closed  bool
	cancel  context.CancelFunc
	// deferred holds the tasks this process could not run because no agent runner was registered,
	// with the time to try them again. It is bounded by the number of tasks and pruned as it expires.
	deferred map[string]time.Time

	done  chan struct{}
	kick  chan struct{}
	timer *time.Timer
}

// New builds a scheduler. Nothing runs until Start.
func New(opts Options) (*Scheduler, error) {
	if opts.Store == nil {
		return nil, errors.New("schedule: a store is required")
	}
	if opts.Actions == nil {
		return nil, errors.New("schedule: actions are required")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	clock := opts.Now
	if clock == nil {
		clock = time.Now
	}
	minInterval := opts.MinInterval
	if minInterval <= 0 {
		minInterval = DefaultMinEvery
	}
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	return &Scheduler{
		store:       opts.Store,
		actions:     opts.Actions,
		log:         logger,
		notify:      opts.Notify,
		clock:       clock,
		minInterval: minInterval,
		deferred:    map[string]time.Time{},
		done:        make(chan struct{}),
		kick:        make(chan struct{}, 1),
		timer:       timer,
	}, nil
}

func (s *Scheduler) now() time.Time { return s.clock().UTC() }

// Start applies the missed policy to the tasks that came due while no scheduler ran, then starts the
// scheduler goroutine. A second Start is a no-op. The goroutine lives until Close; ctx supplies values
// only, not a lifetime.
func (s *Scheduler) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	if s.started {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if err := s.applyMissed(runCtx); err != nil {
		cancel()
		return err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return ErrClosed
	}
	s.started = true
	s.cancel = cancel
	s.mu.Unlock()
	go s.loop(runCtx)
	return nil
}

// Close stops the scheduler goroutine and its timer and waits for it to leave. A firing in progress
// finishes or is cancelled first, so nothing of this scheduler runs after Close returns.
func (s *Scheduler) Close() error {
	s.mu.Lock()
	if s.closed {
		started := s.started
		s.mu.Unlock()
		if started {
			// Another caller is closing: wait for the same goroutine, so both return after it left.
			<-s.done
		}
		return nil
	}
	s.closed = true
	cancel, started := s.cancel, s.started
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if started {
		<-s.done
	}
	return nil
}

// Wake re-arms the scheduler after an external change: a task was created or updated, or an agent
// runner registered. Deferred tasks are retried at once.
func (s *Scheduler) Wake() {
	s.mu.Lock()
	s.deferred = map[string]time.Time{}
	s.mu.Unlock()
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// applyMissed handles the tasks already due when the scheduler starts: skip advances past them
// without firing (a one-shot is disabled), run_once fires one occurrence and then advances.
func (s *Scheduler) applyMissed(ctx context.Context) error {
	now := s.now()
	due, err := s.store.DueSchedules(ctx, now)
	if err != nil {
		return err
	}
	for _, row := range due {
		t := fromRow(row)
		if t.NextRunAt == nil {
			continue
		}
		if t.MissedPolicy == MissedRunOnce {
			s.fire(ctx, t, now)
			continue
		}
		next, err := nextSlot(t, now)
		if err != nil {
			s.log.Warn("schedule: computing the next slot failed", "schedule_id", t.ID, "error", err)
			continue
		}
		disable := next == nil
		claimed, err := s.store.MarkScheduleMissed(ctx, t.ID, *t.NextRunAt, next, disable)
		if err != nil {
			if errors.Is(err, session.ErrNotFound) {
				continue
			}
			return err
		}
		if !claimed {
			continue
		}
		t.NextRunAt, t.Enabled, t.LastStatus, t.LastError = next, !disable, session.ScheduleStatusMissed, ""
		attrs := []any{"schedule_id", t.ID, "kind", string(t.Kind), "status", session.ScheduleStatusMissed}
		if t.SessionID != "" {
			attrs = append(attrs, "session_id", t.SessionID)
		}
		s.log.Info("schedule: missed occurrence advanced past without firing", attrs...)
		s.emit("missed", t)
	}
	return nil
}

func (s *Scheduler) loop(ctx context.Context) {
	defer close(s.done)
	for {
		s.tick(ctx)
		if ctx.Err() != nil {
			return
		}
		s.timer.Reset(s.nextDelay(ctx))
		select {
		case <-ctx.Done():
			s.timer.Stop()
			return
		case <-s.kick:
			if !s.timer.Stop() {
				select {
				case <-s.timer.C:
				default:
				}
			}
		case <-s.timer.C:
		}
	}
}

// tick fires every task that is due and not deferred. Firings run one at a time on this goroutine.
func (s *Scheduler) tick(ctx context.Context) {
	now := s.now()
	due, err := s.store.DueSchedules(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Warn("schedule: reading due tasks failed", "error", err)
		}
		return
	}
	for _, row := range due {
		if ctx.Err() != nil {
			return
		}
		if s.isDeferred(row.ID, now) {
			continue
		}
		s.fire(ctx, fromRow(row), now)
	}
	s.pruneDeferred(now)
}

// fire claims one occurrence and runs it. The claim is the only place an occurrence is taken: a
// scheduler that loses the compare-and-set (another process fired it, or a later tick already
// advanced the row) returns without running anything.
func (s *Scheduler) fire(ctx context.Context, t Task, now time.Time) {
	if t.NextRunAt == nil {
		return
	}
	from, previousFired := *t.NextRunAt, t.LastFiredAt
	next, err := nextSlot(t, now)
	if err != nil {
		// The stored trigger yields no further slot (a corrupted expression or an unknown zone). The
		// task cannot be rescheduled, so it is disabled with the reason instead of staying due and
		// failing on every tick.
		disabled, disableErr := s.store.DisableSchedule(ctx, t.ID, from, session.ScheduleStatusFailed, err.Error())
		if disableErr != nil {
			s.log.Warn("schedule: disabling an unschedulable task failed", "schedule_id", t.ID, "error", disableErr)
			return
		}
		if disabled {
			t.Enabled, t.NextRunAt, t.LastStatus, t.LastError = false, nil, session.ScheduleStatusFailed, err.Error()
			s.emit("fired", t)
		}
		return
	}
	claimed, err := s.store.ClaimSchedule(ctx, t.ID, from, next, now)
	if err != nil {
		s.log.Warn("schedule: claiming a firing failed", "schedule_id", t.ID, "error", err)
		return
	}
	if !claimed {
		return
	}
	t.NextRunAt, t.LastFiredAt, t.FireCount = next, &now, t.FireCount+1
	s.run(ctx, t, now, from, previousFired)
}

// run performs a claimed firing: overlap check, then the action, then the recorded outcome.
func (s *Scheduler) run(ctx context.Context, t Task, now time.Time, claimedFrom time.Time, previousFired *time.Time) {
	started := time.Now()
	if err := payloadProblem(t); err != nil {
		s.record(ctx, t, session.ScheduleStatusFailed, "", err.Error(), started)
		return
	}
	if t.LastRef != "" {
		active, err := s.actions.Active(ctx, t, t.LastRef)
		if err != nil {
			s.record(ctx, t, session.ScheduleStatusFailed, t.LastRef, "checking the previous firing: "+err.Error(), started)
			return
		}
		if active {
			s.overlap(ctx, t, previousFired, started)
			return
		}
	}
	var (
		ref string
		err error
	)
	if t.Kind == KindAgent {
		ref, err = s.actions.RunAgent(ctx, t)
	} else {
		ref, err = s.actions.RunShell(ctx, t)
	}
	if errors.Is(err, ErrBusy) {
		s.overlap(ctx, t, previousFired, started)
		return
	}
	if errors.Is(err, ErrNoRunner) {
		// No runner in this process: give the occurrence back so the task stays due, and wait for a
		// Wake or the retry delay instead of spinning on it.
		ok, revertErr := s.store.RevertScheduleClaim(ctx, t.ID, t.NextRunAt, &claimedFrom, previousFired)
		if revertErr != nil {
			s.log.Warn("schedule: giving a claim back failed", "schedule_id", t.ID, "error", revertErr)
			return
		}
		if ok {
			s.deferUntil(t.ID, now.Add(noRunnerRetry))
		}
		return
	}
	if err != nil {
		s.record(ctx, t, session.ScheduleStatusFailed, "", err.Error(), started)
		return
	}
	s.record(ctx, t, session.ScheduleStatusFired, ref, "", started)
}

// overlap handles a claimed occurrence whose previous firing or session is still running. A recurring
// task records the skip and waits for its next slot. A task with no next slot (a one-shot, such as a
// wake-up that comes due while the run that scheduled it still holds the session) would never fire if
// it were skipped: its claim is given back with next_run_at busyRetry from now, so it stays pending,
// in the database where every scheduler sees it, and fires once the session is free.
func (s *Scheduler) overlap(ctx context.Context, t Task, previousFired *time.Time, started time.Time) {
	if t.NextRunAt != nil {
		s.record(ctx, t, session.ScheduleStatusSkippedOverlap, t.LastRef, "", started)
		return
	}
	retry := s.now().Add(busyRetry)
	if _, err := s.store.RevertScheduleClaim(ctx, t.ID, nil, &retry, previousFired); err != nil {
		s.log.Warn("schedule: deferring a busy one-shot task failed", "schedule_id", t.ID, "error", err)
	}
}

func payloadProblem(t Task) error {
	switch t.Kind {
	case KindShell:
		if t.Shell == nil {
			return invalidf("stored shell payload is unreadable")
		}
	case KindAgent:
		if t.Agent == nil {
			return invalidf("stored agent payload is unreadable")
		}
	default:
		return invalidf("stored kind %q is unknown", t.Kind)
	}
	return nil
}

// record writes the outcome of a firing, logs it with its correlation fields and notifies the host. A
// task that vanished meanwhile is not an error: a deleted session takes its tasks with it.
func (s *Scheduler) record(ctx context.Context, t Task, status, ref, errText string, elapsed time.Time) {
	if err := s.store.RecordScheduleResult(ctx, t.ID, status, ref, errText); err != nil {
		if !errors.Is(err, session.ErrNotFound) {
			s.log.Warn("schedule: recording a firing failed", "schedule_id", t.ID, "error", err)
		}
		return
	}
	t.LastStatus, t.LastRef, t.LastError = status, ref, errText
	s.logFiring(t, status, ref, errText, time.Since(elapsed))
	s.emit("fired", t)
}

// logFiring writes one record per firing with the correlation fields a trace reads (ADR-0019 §5):
// which schedule, which session, what kind, how it ended and how long it took.
func (s *Scheduler) logFiring(t Task, status, ref, errText string, elapsed time.Duration) {
	attrs := []any{
		"schedule_id", t.ID, "kind", string(t.Kind), "status", status,
		"duration_ms", elapsed.Milliseconds(),
	}
	if t.SessionID != "" {
		attrs = append(attrs, "session_id", t.SessionID)
	}
	if ref != "" {
		attrs = append(attrs, "ref", ref)
	}
	switch status {
	case session.ScheduleStatusFailed:
		attrs = append(attrs, "error_code", "schedule_failed", "error", errText)
		s.log.Warn("schedule: firing failed", attrs...)
	case session.ScheduleStatusSkippedOverlap:
		s.log.Info("schedule: firing skipped, the previous one is still active", attrs...)
	default:
		s.log.Info("schedule: fired", attrs...)
	}
}

func (s *Scheduler) emit(event string, t Task) {
	if s.notify != nil {
		s.notify(event, t)
	}
}

func (s *Scheduler) isDeferred(id string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.deferred[id]
	return ok && now.Before(at)
}

func (s *Scheduler) deferUntil(id string, at time.Time) {
	s.mu.Lock()
	s.deferred[id] = at
	s.mu.Unlock()
}

func (s *Scheduler) pruneDeferred(now time.Time) {
	s.mu.Lock()
	for id, at := range s.deferred {
		if !now.Before(at) {
			delete(s.deferred, id)
		}
	}
	s.mu.Unlock()
}

// nextDelay is how long the loop may sleep: until the earliest due task, a deferred retry, or the
// cap, so a row another process changed is noticed within a minute.
func (s *Scheduler) nextDelay(ctx context.Context) time.Duration {
	now := s.now()
	delay := maxSleep
	if earliest, err := s.store.EarliestScheduleDue(ctx); err != nil {
		if ctx.Err() == nil {
			s.log.Warn("schedule: reading the next due time failed", "error", err)
		}
	} else if earliest != nil {
		if wait := earliest.Sub(now); wait < delay {
			delay = wait
		}
	}
	s.mu.Lock()
	for _, at := range s.deferred {
		if wait := at.Sub(now); wait < delay {
			delay = wait
		}
	}
	s.mu.Unlock()
	if delay < minSleep {
		// A row that is still due after a tick means its claim was lost (another process advanced it)
		// or its write is about to land: sleeping a moment re-reads the row instead of spinning.
		delay = minSleep
	}
	return delay
}

// Create validates a spec, stores it and re-arms the scheduler.
func (s *Scheduler) Create(ctx context.Context, spec Spec) (Task, error) {
	now := s.now()
	id, err := newID()
	if err != nil {
		return Task{}, err
	}
	t := Task{
		ID:           id,
		Name:         spec.Name,
		Kind:         spec.Kind,
		SessionID:    spec.SessionID,
		Shell:        spec.Shell,
		Agent:        spec.Agent,
		Trigger:      spec.Trigger.normalized(),
		MissedPolicy: spec.MissedPolicy,
		Enabled:      true,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if spec.Enabled != nil {
		t.Enabled = *spec.Enabled
	}
	if t.MissedPolicy == "" {
		t.MissedPolicy = MissedSkip
	}
	if t.Kind == KindAgent && t.Agent != nil && t.Agent.Policy == "" {
		t.Agent = &AgentPayload{Prompt: t.Agent.Prompt, Policy: PolicyReadOnly}
	}
	if t.Trigger.Kind == TriggerOnce && !t.Trigger.At.IsZero() && t.Trigger.At.Before(now.Add(-oncePastTolerance)) {
		return Task{}, invalidf("a once task's time must be in the future")
	}
	if err := validate(t, s.minInterval); err != nil {
		return Task{}, err
	}
	if t.Enabled {
		if t.NextRunAt, err = firstRunAt(t, now); err != nil {
			return Task{}, err
		}
	}
	row, err := toRow(t)
	if err != nil {
		return Task{}, err
	}
	if err := s.store.CreateSchedule(ctx, row); err != nil {
		return Task{}, err
	}
	s.Wake()
	return t, nil
}

// firstRunAt is when a task that starts now first fires: its time for a once task (even a past one,
// which then fires at the next tick), one interval from now for an interval task, and the next match
// for a cron task.
func firstRunAt(t Task, now time.Time) (*time.Time, error) {
	if t.Trigger.Kind == TriggerOnce {
		at := t.Trigger.At.UTC()
		return &at, nil
	}
	return nextSlot(Task{Trigger: t.Trigger}, now)
}

// Update applies a patch and stores the result. Enabling a disabled task, or changing its trigger,
// recomputes next_run_at from the current time; disabling clears it. The write is a compare-and-set on
// the next_run_at the patch was applied to: when a firing (of this scheduler or another process's)
// moved the row in between, the patch is applied again to the row as it now is, so an update never
// hands a claimed occurrence back to fire a second time.
func (s *Scheduler) Update(ctx context.Context, id string, patch Patch) (Task, error) {
	for range maxUpdateAttempts {
		row, err := s.store.GetSchedule(ctx, id)
		if err != nil {
			return Task{}, err
		}
		t, err := applyPatch(fromRow(row), patch, s.now(), s.minInterval)
		if err != nil {
			return Task{}, err
		}
		next, err := toRow(t)
		if err != nil {
			return Task{}, err
		}
		updated, err := s.store.UpdateSchedule(ctx, next, row.NextRunAt)
		if err != nil {
			return Task{}, err
		}
		if updated {
			s.Wake()
			return t, nil
		}
	}
	return Task{}, fmt.Errorf("schedule %s kept changing while it was updated; try again", id)
}

// applyPatch is t with patch applied at now, validated.
func applyPatch(t Task, patch Patch, now time.Time, minInterval time.Duration) (Task, error) {
	recompute := false
	if patch.Name != nil {
		t.Name = *patch.Name
	}
	if patch.MissedPolicy != nil {
		t.MissedPolicy = *patch.MissedPolicy
	}
	if patch.Trigger != nil {
		t.Trigger = patch.Trigger.normalized()
		recompute = true
	}
	switch t.Kind {
	case KindShell:
		if patch.Agent != nil {
			return Task{}, invalidf("a %s task carries no agent payload", KindShell)
		}
		if patch.Shell != nil {
			t.Shell = patch.Shell
		}
	case KindAgent:
		if patch.Shell != nil {
			return Task{}, invalidf("an %s task carries no shell payload", KindAgent)
		}
		if patch.Agent != nil {
			policy := patch.Agent.Policy
			if policy == "" {
				policy = PolicyReadOnly
			}
			t.Agent = &AgentPayload{Prompt: patch.Agent.Prompt, Policy: policy}
		}
	}
	if patch.Enabled != nil && *patch.Enabled != t.Enabled {
		t.Enabled = *patch.Enabled
		recompute = true
	}
	if recompute {
		t.NextRunAt = nil
		if t.Enabled {
			var err error
			if t.NextRunAt, err = firstRunAt(t, now); err != nil {
				return Task{}, err
			}
		}
	}
	t.UpdatedAt = now
	if err := validate(t, minInterval); err != nil {
		return Task{}, err
	}
	return t, nil
}

// Delete removes a task and re-arms the scheduler.
func (s *Scheduler) Delete(ctx context.Context, id string) error {
	if err := s.store.DeleteSchedule(ctx, id); err != nil {
		return err
	}
	s.Wake()
	return nil
}

// Get returns one task.
func (s *Scheduler) Get(ctx context.Context, id string) (Task, error) {
	row, err := s.store.GetSchedule(ctx, id)
	if err != nil {
		return Task{}, err
	}
	return fromRow(row), nil
}

// List returns every task, oldest first.
func (s *Scheduler) List(ctx context.Context) ([]Task, error) {
	rows, err := s.store.ListSchedules(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRow(row))
	}
	return out, nil
}

// ObserveRef records the terminal status of the job or run a firing started (completed, failed,
// timed_out, cancelled). An observation of a ref the task no longer points at is dropped: a later
// firing's status is never overwritten by an earlier one's.
func (s *Scheduler) ObserveRef(ctx context.Context, scheduleID, ref, status string) error {
	if status == "" {
		return invalidf("status is required")
	}
	_, err := s.store.RecordScheduleObserved(ctx, scheduleID, ref, status)
	return err
}
