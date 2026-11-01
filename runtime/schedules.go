package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/edynasty/LumenCortex/internal/schedule"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/toolset"
)

// Scheduled tasks (ADR-0019 §4). The scheduler lives in internal/schedule; this file binds it to the
// Engine: a shell task submits a job to the job queue, an agent task starts a run through the
// RunSupervisor the host registered, and every change and firing reaches the event bus and the journal.

// maxScheduleEverySeconds bounds an interval so a host's seconds value cannot overflow a duration.
const maxScheduleEverySeconds = 365 * 24 * 60 * 60

// errJobQueueUnavailable reports a shell schedule in a process without the job queue. The scheduler
// records it as a failed firing with this text; the task keeps its schedule.
var errJobQueueUnavailable = errors.New("job queue unavailable")

// scheduledJobs is the job queue's scheduled-task surface (ADR-0019 §3, §4): a shell timer submits a
// job with source "schedule", and the overlap check asks whether the job a firing started still runs.
// OpenContext binds the Engine's own queue (Engine.SubmitScheduled, Engine.JobActive) before the
// scheduler starts; while it is nil a shell task fails with errJobQueueUnavailable instead of silently
// doing nothing.
type scheduledJobs interface {
	SubmitScheduled(ctx context.Context, scheduleID, sessionID, command string, timeout time.Duration) (jobID string, err error)
	JobActive(ctx context.Context, jobID string) (bool, error)
}

// bindScheduledJobs connects a job queue to the scheduler and wakes it, so the shell tasks it could not
// fire are retried.
func (e *Engine) bindScheduledJobs(jobs scheduledJobs) {
	e.schedMu.Lock()
	e.scheduledJobs = jobs
	e.schedMu.Unlock()
	if e.schedules != nil {
		e.schedules.Wake()
	}
}

func (e *Engine) scheduledJobQueue() scheduledJobs {
	e.schedMu.Lock()
	defer e.schedMu.Unlock()
	return e.scheduledJobs
}

// setAgentRunner registers the supervisor agent schedules start runs with. NewRunSupervisor calls it,
// and the scheduler retries the agent tasks that had no runner.
func (e *Engine) setAgentRunner(s *RunSupervisor) {
	e.schedMu.Lock()
	e.agentRunner = s
	e.schedMu.Unlock()
	if e.schedules != nil {
		e.schedules.Wake()
	}
}

// clearAgentRunner unregisters a supervisor that is closing: agent tasks then stay due instead of
// starting runs nothing would wait for.
func (e *Engine) clearAgentRunner(s *RunSupervisor) {
	e.schedMu.Lock()
	if e.agentRunner == s {
		e.agentRunner = nil
	}
	e.schedMu.Unlock()
}

func (e *Engine) agentRunnerRef() *RunSupervisor {
	e.schedMu.Lock()
	defer e.schedMu.Unlock()
	return e.agentRunner
}

// scheduleActions is the scheduler's Actions over one Engine: shell tasks go to the job queue, agent
// tasks to the registered RunSupervisor, and the overlap check asks the same two.
type scheduleActions struct{ engine *Engine }

// RunShell fires a shell task into the job queue.
func (a scheduleActions) RunShell(ctx context.Context, t schedule.Task) (string, error) {
	jobs := a.engine.scheduledJobQueue()
	if jobs == nil {
		return "", errJobQueueUnavailable
	}
	if t.Shell == nil {
		return "", errors.New("shell schedule has no command")
	}
	return jobs.SubmitScheduled(ctx, t.ID, t.SessionID, t.Shell.Command,
		time.Duration(t.Shell.TimeoutSeconds)*time.Second)
}

// RunAgent starts a run on the task's session with its prompt and policy. Without a supervisor the
// task stays due (schedule.ErrNoRunner).
func (a scheduleActions) RunAgent(ctx context.Context, t schedule.Task) (string, error) {
	runner := a.engine.agentRunnerRef()
	if runner == nil {
		return "", schedule.ErrNoRunner
	}
	if t.Agent == nil {
		return "", errors.New("agent schedule has no prompt")
	}
	if _, err := runner.Start(ctx, t.SessionID, AgentOptions{Input: t.Agent.Prompt, Policy: t.Agent.Policy}); err != nil {
		if errors.Is(err, ErrRunHeld) {
			// The session is running (the run that scheduled a wake-up, another Engine, another
			// process): an overlap, not a failure. The scheduler skips a recurring occurrence and
			// retries a one-shot.
			return "", schedule.ErrBusy
		}
		return "", err
	}
	return t.SessionID, nil
}

// Active reports whether the previous firing is still running: the job of a shell task, or the run on
// an agent task's session.
func (a scheduleActions) Active(ctx context.Context, t schedule.Task, ref string) (bool, error) {
	switch t.Kind {
	case schedule.KindShell:
		jobs := a.engine.scheduledJobQueue()
		if jobs == nil {
			return false, nil
		}
		return jobs.JobActive(ctx, ref)
	case schedule.KindAgent:
		runner := a.engine.agentRunnerRef()
		if runner == nil {
			return false, nil
		}
		return runner.Active(t.SessionID), nil
	}
	return false, nil
}

// scheduleFired publishes and journals one firing's outcome. It is the scheduler's Notify callback and
// runs on the scheduler goroutine, so it never blocks: the event bus drops for a slow subscriber.
func (e *Engine) scheduleFired(event string, t schedule.Task) {
	if event == "fired" {
		e.events.publish("schedule.fired", t.SessionID, map[string]any{
			"scheduleId": t.ID,
			"kind":       string(t.Kind),
			"status":     t.LastStatus,
			"ref":        t.LastRef,
		})
	} else {
		// An occurrence nobody ran fires nothing: the row advanced, and hosts refresh it.
		e.events.publish("schedule.changed", t.SessionID, map[string]any{
			"scheduleId": t.ID,
			"status":     t.LastStatus,
		})
	}
	// The journal is content-free: the task's name, command and prompt stay in scheduled_tasks.
	_ = e.store.Journal(context.Background(), "schedule."+event, map[string]any{
		"scheduleId": t.ID,
		"kind":       string(t.Kind),
		"status":     t.LastStatus,
	})
}

// scheduleChanged announces a create, update or delete so hosts and other Engines refresh their view.
func (e *Engine) scheduleChanged(ctx context.Context, t schedule.Task) {
	e.events.publish("schedule.changed", t.SessionID, map[string]any{"scheduleId": t.ID})
	_ = e.store.Journal(ctx, "schedule.changed", map[string]any{"scheduleId": t.ID, "kind": string(t.Kind)})
}

// ScheduleTrigger is a trigger as the host sends and reads it: an RFC 3339 time for "once", a period
// in seconds for "interval".
type ScheduleTrigger struct {
	Kind string `json:"kind"`
	// At is the time of a "once" trigger (RFC 3339).
	At string `json:"at,omitempty"`
	// EverySeconds is the period of an "interval" trigger.
	EverySeconds int64 `json:"everySeconds,omitempty"`
	// Cron is the five-field expression of a "cron" trigger.
	Cron string `json:"cron,omitempty"`
	// Timezone is the IANA zone a cron expression is read in; "" is UTC.
	Timezone string `json:"timezone,omitempty"`
}

// ScheduleInfo is one scheduled task as the host reads it (JSON camelCase).
type ScheduleInfo struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	SessionID      string          `json:"sessionId,omitempty"`
	Command        string          `json:"command,omitempty"`
	TimeoutSeconds int             `json:"timeoutSeconds,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Policy         string          `json:"policy,omitempty"`
	Trigger        ScheduleTrigger `json:"trigger"`
	// Cron and Timezone repeat the trigger's expression and zone at the top level: a host that lists
	// timers reads them without descending into the trigger.
	Cron         string     `json:"cron,omitempty"`
	Timezone     string     `json:"timezone,omitempty"`
	MissedPolicy string     `json:"missedPolicy"`
	Enabled      bool       `json:"enabled"`
	NextRunAt    *time.Time `json:"nextRunAt,omitempty"`
	LastFiredAt  *time.Time `json:"lastFiredAt,omitempty"`
	LastStatus   string     `json:"lastStatus,omitempty"`
	LastRef      string     `json:"lastRef,omitempty"`
	LastError    string     `json:"lastError,omitempty"`
	FireCount    int64      `json:"fireCount"`
	CreatedAt    time.Time  `json:"createdAt"`
	UpdatedAt    time.Time  `json:"updatedAt"`
}

// ScheduleRequest creates a task. Enabled nil means enabled; missedPolicy "" means "skip"; policy ""
// means "read-only".
type ScheduleRequest struct {
	Name           string          `json:"name"`
	Kind           string          `json:"kind"`
	SessionID      string          `json:"sessionId,omitempty"`
	Command        string          `json:"command,omitempty"`
	TimeoutSeconds int             `json:"timeoutSeconds,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Policy         string          `json:"policy,omitempty"`
	Trigger        ScheduleTrigger `json:"trigger"`
	MissedPolicy   string          `json:"missedPolicy,omitempty"`
	Enabled        *bool           `json:"enabled,omitempty"`
}

// SchedulePatch updates the fields it names. A trigger or enablement change recomputes nextRunAt.
type SchedulePatch struct {
	Name           *string          `json:"name,omitempty"`
	Enabled        *bool            `json:"enabled,omitempty"`
	Trigger        *ScheduleTrigger `json:"trigger,omitempty"`
	MissedPolicy   *string          `json:"missedPolicy,omitempty"`
	Command        *string          `json:"command,omitempty"`
	TimeoutSeconds *int             `json:"timeoutSeconds,omitempty"`
	Prompt         *string          `json:"prompt,omitempty"`
	Policy         *string          `json:"policy,omitempty"`
}

func (t ScheduleTrigger) scheduleTrigger() (schedule.Trigger, error) {
	switch t.Kind {
	case string(schedule.TriggerOnce):
		at, err := time.Parse(time.RFC3339, t.At)
		if err != nil {
			return schedule.Trigger{}, fmt.Errorf("%w: trigger.at must be RFC 3339", schedule.ErrInvalid)
		}
		return schedule.Trigger{Kind: schedule.TriggerOnce, At: at.UTC()}, nil
	case string(schedule.TriggerInterval):
		if t.EverySeconds <= 0 || t.EverySeconds > maxScheduleEverySeconds {
			return schedule.Trigger{}, fmt.Errorf("%w: trigger.everySeconds must be 1..%d", schedule.ErrInvalid, maxScheduleEverySeconds)
		}
		return schedule.Trigger{Kind: schedule.TriggerInterval, Every: time.Duration(t.EverySeconds) * time.Second}, nil
	case string(schedule.TriggerCron):
		return schedule.Trigger{Kind: schedule.TriggerCron, Cron: t.Cron, Timezone: t.Timezone}, nil
	}
	return schedule.Trigger{}, fmt.Errorf("%w: trigger.kind must be %s, %s or %s", schedule.ErrInvalid,
		schedule.TriggerOnce, schedule.TriggerInterval, schedule.TriggerCron)
}

func scheduleInfo(t schedule.Task) ScheduleInfo {
	info := ScheduleInfo{
		ID:        t.ID,
		Name:      t.Name,
		Kind:      string(t.Kind),
		SessionID: t.SessionID,
		Trigger: ScheduleTrigger{
			Kind:     string(t.Trigger.Kind),
			Cron:     t.Trigger.Cron,
			Timezone: t.Trigger.Timezone,
		},
		Cron:         t.Trigger.Cron,
		Timezone:     t.Trigger.Timezone,
		MissedPolicy: string(t.MissedPolicy),
		Enabled:      t.Enabled,
		NextRunAt:    t.NextRunAt,
		LastFiredAt:  t.LastFiredAt,
		LastStatus:   t.LastStatus,
		LastRef:      t.LastRef,
		LastError:    t.LastError,
		FireCount:    t.FireCount,
		CreatedAt:    t.CreatedAt,
		UpdatedAt:    t.UpdatedAt,
	}
	switch t.Trigger.Kind {
	case schedule.TriggerOnce:
		info.Trigger.At = t.Trigger.At.UTC().Format(time.RFC3339)
	case schedule.TriggerInterval:
		info.Trigger.EverySeconds = int64(t.Trigger.Every / time.Second)
	}
	if t.Shell != nil {
		info.Command, info.TimeoutSeconds = t.Shell.Command, t.Shell.TimeoutSeconds
	}
	if t.Agent != nil {
		info.Prompt, info.Policy = t.Agent.Prompt, t.Agent.Policy
	}
	return info
}

// Schedules lists every scheduled task, oldest first.
func (e *Engine) Schedules(ctx context.Context) ([]ScheduleInfo, error) {
	tasks, err := e.schedules.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ScheduleInfo, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, scheduleInfo(t))
	}
	return out, nil
}

// Schedule returns one scheduled task; session.ErrNotFound when it does not exist.
func (e *Engine) Schedule(ctx context.Context, id string) (ScheduleInfo, error) {
	t, err := e.schedules.Get(ctx, id)
	if err != nil {
		return ScheduleInfo{}, err
	}
	return scheduleInfo(t), nil
}

// CreateSchedule stores a new timer and announces it.
func (e *Engine) CreateSchedule(ctx context.Context, req ScheduleRequest) (ScheduleInfo, error) {
	trigger, err := req.Trigger.scheduleTrigger()
	if err != nil {
		return ScheduleInfo{}, err
	}
	spec := schedule.Spec{
		Name:         req.Name,
		Kind:         schedule.Kind(req.Kind),
		SessionID:    req.SessionID,
		Trigger:      trigger,
		MissedPolicy: schedule.MissedPolicy(req.MissedPolicy),
		Enabled:      req.Enabled,
	}
	switch spec.Kind {
	case schedule.KindShell:
		spec.Shell = &schedule.ShellPayload{Command: req.Command, TimeoutSeconds: req.TimeoutSeconds}
	case schedule.KindAgent:
		spec.Agent = &schedule.AgentPayload{Prompt: req.Prompt, Policy: req.Policy}
	}
	t, err := e.schedules.Create(ctx, spec)
	if err != nil {
		return ScheduleInfo{}, err
	}
	e.scheduleChanged(ctx, t)
	return scheduleInfo(t), nil
}

// UpdateSchedule changes the parts of a task the patch names and announces it.
func (e *Engine) UpdateSchedule(ctx context.Context, id string, patch SchedulePatch) (ScheduleInfo, error) {
	update := schedule.Patch{
		Name:         patch.Name,
		Enabled:      patch.Enabled,
		MissedPolicy: (*schedule.MissedPolicy)(patch.MissedPolicy),
	}
	if patch.Trigger != nil {
		trigger, err := patch.Trigger.scheduleTrigger()
		if err != nil {
			return ScheduleInfo{}, err
		}
		update.Trigger = &trigger
	}
	if patch.Command != nil || patch.TimeoutSeconds != nil || patch.Prompt != nil || patch.Policy != nil {
		current, err := e.schedules.Get(ctx, id)
		if err != nil {
			return ScheduleInfo{}, err
		}
		switch current.Kind {
		case schedule.KindShell:
			if patch.Prompt != nil || patch.Policy != nil {
				return ScheduleInfo{}, fmt.Errorf("%w: a %s task carries no prompt or policy", schedule.ErrInvalid, schedule.KindShell)
			}
			update.Shell = &schedule.ShellPayload{
				Command:        patch.stringValue(patch.Command, shellCommand(current)),
				TimeoutSeconds: patch.intValue(patch.TimeoutSeconds, shellTimeout(current)),
			}
		case schedule.KindAgent:
			if patch.Command != nil || patch.TimeoutSeconds != nil {
				return ScheduleInfo{}, fmt.Errorf("%w: an %s task carries no command", schedule.ErrInvalid, schedule.KindAgent)
			}
			update.Agent = &schedule.AgentPayload{
				Prompt: patch.stringValue(patch.Prompt, agentPrompt(current)),
				Policy: patch.stringValue(patch.Policy, agentPolicy(current)),
			}
		}
	}
	t, err := e.schedules.Update(ctx, id, update)
	if err != nil {
		return ScheduleInfo{}, err
	}
	e.scheduleChanged(ctx, t)
	return scheduleInfo(t), nil
}

// DeleteSchedule removes a task and announces it.
func (e *Engine) DeleteSchedule(ctx context.Context, id string) error {
	t, err := e.schedules.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := e.schedules.Delete(ctx, id); err != nil {
		return err
	}
	e.scheduleChanged(ctx, t)
	return nil
}

func (p SchedulePatch) stringValue(given *string, current string) string {
	if given != nil {
		return *given
	}
	return current
}

func (p SchedulePatch) intValue(given *int, current int) int {
	if given != nil {
		return *given
	}
	return current
}

func shellCommand(t schedule.Task) string {
	if t.Shell != nil {
		return t.Shell.Command
	}
	return ""
}

func shellTimeout(t schedule.Task) int {
	if t.Shell != nil {
		return t.Shell.TimeoutSeconds
	}
	return 0
}

func agentPrompt(t schedule.Task) string {
	if t.Agent != nil {
		return t.Agent.Prompt
	}
	return ""
}

func agentPolicy(t schedule.Task) string {
	if t.Agent != nil {
		return t.Agent.Policy
	}
	return ""
}

// compile-time check that the Engine is the scheduler's action set.
var _ schedule.Actions = scheduleActions{}

// wakeups are the delayed wake-ups one run may schedule (ADR-0019 §4): a one-shot agent task on the
// run's own session, with the run's policy as its ceiling.
type wakeups struct {
	engine    *Engine
	sessionID string
	policy    string
}

// maxPendingWakeups bounds the wake-ups of one session: each is a future run, and an agent that could
// schedule them without limit could keep the workspace busy indefinitely.
const maxPendingWakeups = 3

// wakeupPromptPrefix marks a task a wakeup_schedule tool created, so its prompt reads as an instruction
// of the runtime rather than a user's.
const wakeupPromptPrefix = "[runtime] Scheduled wake-up: "

// wakeupsFor binds the wake-up tools to one run. A run without a session (never the case for a
// prepared run) gets none.
func (e *Engine) wakeupsFor(sessionID, policy string) toolset.WakeupController {
	if sessionID == "" || e.schedules == nil {
		return nil
	}
	if policy == "" {
		policy = schedule.PolicyReadOnly
	}
	return wakeups{engine: e, sessionID: sessionID, policy: policy}
}

// ScheduleWakeup creates the wake-up. A session with the maximum pending is refused: the agent must
// cancel one first.
func (w wakeups) ScheduleWakeup(ctx context.Context, delaySec int, message string) (toolset.Wakeup, error) {
	if delaySec < toolset.MinWakeupDelaySec || delaySec > toolset.MaxWakeupDelaySec {
		return toolset.Wakeup{}, fmt.Errorf("delay_sec must be %d..%d", toolset.MinWakeupDelaySec, toolset.MaxWakeupDelaySec)
	}
	pending, err := w.pending(ctx)
	if err != nil {
		return toolset.Wakeup{}, err
	}
	if len(pending) >= maxPendingWakeups {
		return toolset.Wakeup{}, fmt.Errorf("this session already has %d pending wake-ups: cancel one with wakeup_cancel first", len(pending))
	}
	// Whole seconds: the tool reports the time as RFC 3339, so the row must hold the same instant.
	at := time.Now().UTC().Add(time.Duration(delaySec) * time.Second).Truncate(time.Second)
	task, err := w.engine.schedules.Create(ctx, schedule.Spec{
		Name:      wakeupName(message),
		Kind:      schedule.KindAgent,
		SessionID: w.sessionID,
		Agent:     &schedule.AgentPayload{Prompt: wakeupPromptPrefix + message, Policy: w.policy},
		Trigger:   schedule.Trigger{Kind: schedule.TriggerOnce, At: at},
	})
	if err != nil {
		return toolset.Wakeup{}, err
	}
	w.engine.scheduleChanged(ctx, task)
	return toolset.Wakeup{ID: task.ID, FireAt: at.Format(time.RFC3339)}, nil
}

// CancelWakeup removes a pending wake-up of this session. A task of another session is reported as not
// found, not as a refusal: a run cannot probe other sessions' timers.
func (w wakeups) CancelWakeup(ctx context.Context, id string) error {
	task, err := w.engine.schedules.Get(ctx, id)
	if err != nil {
		return err
	}
	if task.Kind != schedule.KindAgent || task.SessionID != w.sessionID {
		return session.ErrNotFound
	}
	if err := w.engine.schedules.Delete(ctx, id); err != nil {
		return err
	}
	w.engine.scheduleChanged(ctx, task)
	return nil
}

func (w wakeups) pending(ctx context.Context) ([]schedule.Task, error) {
	tasks, err := w.engine.schedules.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]schedule.Task, 0, len(tasks))
	for _, t := range tasks {
		if t.Kind == schedule.KindAgent && t.SessionID == w.sessionID && t.Enabled && t.NextRunAt != nil &&
			strings.HasPrefix(agentPromptOf(t), wakeupPromptPrefix) {
			out = append(out, t)
		}
	}
	return out, nil
}

// wakeupName is the task's display name: the message's first words, inside the name bound.
func wakeupName(message string) string {
	const ellipsis = "..."
	name := "wake-up: " + strings.Join(strings.Fields(message), " ")
	if len(name) <= schedule.MaxNameBytes {
		return name
	}
	cut := schedule.MaxNameBytes - len(ellipsis)
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + ellipsis
}

func agentPromptOf(t schedule.Task) string {
	if t.Agent != nil {
		return t.Agent.Prompt
	}
	return ""
}
