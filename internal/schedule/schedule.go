// Package schedule runs the workspace's durable timers (ADR-0019 §4): one goroutine and one
// time.Timer per Engine over the scheduled_tasks table, firing a shell command into the job queue or
// starting an agent run on a session. The rows and their SQL belong to internal/session; this package
// decides when a row is due, claims the occurrence so two processes never fire it twice, and records
// the outcome.
package schedule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// ErrInvalid reports a spec or patch the scheduler refuses before anything is written.
var ErrInvalid = errors.New("invalid schedule")

// ErrBusy reports that the previous firing of the task is still running, or that the action cannot
// start it now (a run started by another process, a job the queue has not finished releasing, an
// Engine at its agent concurrency limit). The occurrence of a recurring task is recorded as
// skipped_overlap, never as a failure; a one-shot task stays pending and is tried again busyRetry later.
var ErrBusy = errors.New("the previous firing is still running")

// ErrNoRunner reports an agent task in a process with no RunSupervisor: the task stays due and is
// retried when a runner registers (Actions returns it; the scheduler reverts the claim).
var ErrNoRunner = errors.New("no agent runner in this process")

// Kind is what a firing does.
type Kind string

const (
	// KindShell submits the task's command as a background job (source schedule).
	KindShell Kind = "shell"
	// KindAgent starts a run on the task's session with the task's prompt and policy.
	KindAgent Kind = "agent"
)

// TriggerKind is when a task fires.
type TriggerKind string

const (
	// TriggerOnce fires at one time and then stops (next_run_at becomes NULL).
	TriggerOnce TriggerKind = "once"
	// TriggerInterval fires every Every from its scheduled slot.
	TriggerInterval TriggerKind = "interval"
	// TriggerCron fires at each time Cron matches in the task's Timezone.
	TriggerCron TriggerKind = "cron"
)

// MissedPolicy decides what an occurrence that came due while no scheduler ran does.
type MissedPolicy string

const (
	// MissedSkip advances past the missed occurrences without firing.
	MissedSkip MissedPolicy = "skip"
	// MissedRunOnce fires once and then advances past the rest.
	MissedRunOnce MissedPolicy = "run_once"
)

// Policies an agent task may run with. They are the toolset's policy words; the default is
// read-only, so a timer never widens what a run may do unless the task says so.
const (
	PolicyReadOnly  = "read-only"
	PolicyWorkspace = "workspace"
	PolicyFull      = "full"
)

// Bounds of a task's payload, from 0008_schedules.sql and ADR-0019 §4.
const (
	MaxNameBytes      = 200
	MaxCronBytes      = 200
	MaxCommandBytes   = 65536
	MaxPromptBytes    = 65536
	MaxPayloadBytes   = 65536
	MaxTimeoutSec     = 86400
	DefaultMinEvery   = 60 * time.Second
	oncePastTolerance = 5 * time.Second
	// maxSleep caps one timer sleep, so a change another process wrote is noticed within a minute
	// even when the next local task is hours away.
	maxSleep = time.Minute
	// minSleep keeps the loop from spinning when a row is still due after a tick: a lost claim is
	// re-read a moment later, not in a tight loop.
	minSleep = 100 * time.Millisecond
	// noRunnerRetry is how long a task whose agent runner is absent waits before this process tries
	// it again. Without it the task would stay due and spin the loop.
	noRunnerRetry = 30 * time.Second
	// busyRetry is how long a one-shot task whose previous firing or session is still running waits
	// before it is tried again (a wake-up that comes due while the run that scheduled it still holds
	// the session). A refused start costs one lock attempt, so the wait is short: the task fires a
	// moment after the session is free.
	busyRetry = 2 * time.Second
	// maxUpdateAttempts bounds how often Update re-reads a row that a firing moved between its read and
	// its compare-and-set write. A firing moves a row once per occurrence, so a second loss is rare.
	maxUpdateAttempts = 3
	// missedGrace is how long overdue an occurrence must be before a starting scheduler applies the
	// missed policy to it. A live scheduler re-reads its due tasks at least every maxSleep and fires
	// them one after another, so an occurrence a few seconds or a minute overdue may be about to fire
	// in another Engine on the same database; past two such sleeps it counts as missed.
	missedGrace = 2 * maxSleep
)

// ShellPayload is the command a shell task submits.
type ShellPayload struct {
	Command        string `json:"command"`
	TimeoutSeconds int    `json:"timeoutSeconds,omitempty"`
}

// AgentPayload is the prompt an agent task starts and the policy it runs with. ToolAllowlist and
// ToolDenylist are the host tool filters the run gets (canonical tool names, nil for none): a wake-up
// carries those of the run that scheduled it, so the woken run is offered no tool that run was not. An
// empty allowlist is stored as none; a run with an empty allowlist is offered no wakeup_schedule, so
// no wake-up carries one.
type AgentPayload struct {
	Prompt        string   `json:"prompt"`
	Policy        string   `json:"policy,omitempty"`
	ToolAllowlist []string `json:"toolAllowlist,omitempty"`
	ToolDenylist  []string `json:"toolDenylist,omitempty"`
}

// Trigger is when a task fires: once at At, every Every from its scheduled slot, or at each time
// Cron (a standard five-field expression) matches in Timezone (an IANA name; "" is UTC).
type Trigger struct {
	Kind     TriggerKind
	At       time.Time
	Every    time.Duration
	Cron     string
	Timezone string
}

// location resolves the trigger's timezone; "" is UTC.
func (t Trigger) location() (*time.Location, error) { return loadTimezone(t.Timezone) }

// normalized names the zone a stored task carries: the schema defaults the column to UTC, so a task
// that names none holds "UTC" too.
func (t Trigger) normalized() Trigger {
	if t.Timezone == "" {
		t.Timezone = "UTC"
	}
	return t
}

// Task is one scheduled task as the scheduler sees it. It mirrors a scheduled_tasks row with the
// payload decoded for its kind.
type Task struct {
	ID           string
	Name         string
	Kind         Kind
	SessionID    string
	Shell        *ShellPayload
	Agent        *AgentPayload
	Trigger      Trigger
	MissedPolicy MissedPolicy
	Enabled      bool
	NextRunAt    *time.Time
	LastFiredAt  *time.Time
	LastStatus   string
	LastRef      string
	LastError    string
	FireCount    int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Spec creates a task. Enabled nil means enabled: a created task that should not run yet is disabled
// explicitly.
type Spec struct {
	Name         string
	Kind         Kind
	SessionID    string
	Shell        *ShellPayload
	Agent        *AgentPayload
	Trigger      Trigger
	MissedPolicy MissedPolicy
	Enabled      *bool
}

// Patch updates the parts of a task it names; every other field keeps its value. A trigger or
// enablement change recomputes next_run_at from the current time.
type Patch struct {
	Name         *string
	Enabled      *bool
	Trigger      *Trigger
	MissedPolicy *MissedPolicy
	Shell        *ShellPayload
	Agent        *AgentPayload
}

// Store is the part of session.Store the scheduler uses: the rows, and the compare-and-set writes
// that make a firing exactly-once across processes.
type Store interface {
	CreateSchedule(ctx context.Context, t session.ScheduledTask) error
	GetSchedule(ctx context.Context, id string) (session.ScheduledTask, error)
	ListSchedules(ctx context.Context) ([]session.ScheduledTask, error)
	UpdateSchedule(ctx context.Context, t session.ScheduledTask, from *time.Time) (bool, error)
	DeleteSchedule(ctx context.Context, id string) error
	DueSchedules(ctx context.Context, at time.Time) ([]session.ScheduledTask, error)
	EarliestScheduleDue(ctx context.Context) (*time.Time, error)
	ClaimSchedule(ctx context.Context, id string, from time.Time, next *time.Time, firedAt time.Time) (bool, error)
	RevertScheduleClaim(ctx context.Context, id string, claimed, restoreNext, restoreFiredAt *time.Time) (bool, error)
	MarkScheduleMissed(ctx context.Context, id string, from time.Time, next *time.Time, disable bool) (bool, error)
	RecordScheduleResult(ctx context.Context, id, status, ref, errText string) error
	DisableSchedule(ctx context.Context, id string, from time.Time, status, errText string) (bool, error)
	RecordScheduleObserved(ctx context.Context, id, ref, status string) (bool, error)
}

// Actions is what a firing does, bound by the runtime: the job queue for a shell task and the
// RunSupervisor for an agent task.
type Actions interface {
	RunShell(ctx context.Context, t Task) (ref string, err error)
	RunAgent(ctx context.Context, t Task) (ref string, err error)
	// Active reports whether the job or run the previous firing started (ref) is still running.
	Active(ctx context.Context, t Task, ref string) (bool, error)
}

// newID returns a schedule id. The prefix keeps the table readable and the id inside the schema's
// 64-character bound.
func newID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "schedule_" + hex.EncodeToString(buf), nil
}

// fromRow decodes a stored row. A payload the kind cannot read leaves Shell or Agent nil; the firing
// records that as a failure instead of running a task it cannot describe.
func fromRow(row session.ScheduledTask) Task {
	t := Task{
		ID:        row.ID,
		Name:      row.Name,
		Kind:      Kind(row.Kind),
		SessionID: row.SessionID,
		Trigger: Trigger{
			Kind:     TriggerKind(row.TriggerKind),
			Every:    row.Interval,
			Cron:     row.Cron,
			Timezone: row.Timezone,
		},
		MissedPolicy: MissedPolicy(row.MissedPolicy),
		Enabled:      row.Enabled,
		NextRunAt:    row.NextRunAt,
		LastFiredAt:  row.LastFiredAt,
		LastStatus:   row.LastStatus,
		LastRef:      row.LastRef,
		LastError:    row.LastError,
		FireCount:    row.FireCount,
		CreatedAt:    row.CreatedAt,
		UpdatedAt:    row.UpdatedAt,
	}
	if row.RunAt != nil {
		t.Trigger.At = *row.RunAt
	}
	switch t.Kind {
	case KindShell:
		var payload ShellPayload
		if err := json.Unmarshal(row.Payload, &payload); err == nil {
			t.Shell = &payload
		}
	case KindAgent:
		var payload AgentPayload
		if err := json.Unmarshal(row.Payload, &payload); err == nil {
			t.Agent = &payload
		}
	}
	return t
}

// toRow encodes a task for the store. An invalid task is refused here too, so a patch that produced
// an unusable row never reaches SQL.
func toRow(t Task) (session.ScheduledTask, error) {
	payload, err := payloadJSON(t)
	if err != nil {
		return session.ScheduledTask{}, err
	}
	row := session.ScheduledTask{
		ID:           t.ID,
		Name:         t.Name,
		Kind:         string(t.Kind),
		SessionID:    t.SessionID,
		Payload:      payload,
		TriggerKind:  string(t.Trigger.Kind),
		Interval:     t.Trigger.Every,
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
	if t.Trigger.Kind == TriggerOnce {
		at := t.Trigger.At.UTC()
		row.RunAt = &at
	}
	return row, nil
}

func payloadJSON(t Task) (json.RawMessage, error) {
	switch t.Kind {
	case KindShell:
		if t.Shell == nil {
			return nil, invalidf("a shell task needs a shell payload")
		}
		raw, err := json.Marshal(t.Shell)
		if err != nil {
			return nil, err
		}
		return raw, nil
	case KindAgent:
		if t.Agent == nil {
			return nil, invalidf("an agent task needs an agent payload")
		}
		raw, err := json.Marshal(t.Agent)
		if err != nil {
			return nil, err
		}
		return raw, nil
	}
	return nil, invalidf("kind must be %s or %s", KindShell, KindAgent)
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// validate checks a whole task: its name and payload bounds, its kind and payload, its trigger and
// missed policy, and the interval floor the schema enforces.
func validate(t Task, minEvery time.Duration) error {
	if t.Name == "" || len(t.Name) > MaxNameBytes || strings.ContainsAny(t.Name, "\r\n\x00") {
		return invalidf("name must be 1-%d bytes on one line", MaxNameBytes)
	}
	switch t.Kind {
	case KindShell:
		if t.Agent != nil || t.Shell == nil {
			return invalidf("a %s task carries only a shell payload", KindShell)
		}
		if t.SessionID != "" {
			return invalidf("a %s task has no session", KindShell)
		}
		if n := len(t.Shell.Command); n == 0 || n > MaxCommandBytes {
			return invalidf("command must be 1-%d bytes", MaxCommandBytes)
		}
		if t.Shell.TimeoutSeconds < 0 || t.Shell.TimeoutSeconds > MaxTimeoutSec {
			return invalidf("timeoutSeconds must be 0 (none) to %d", MaxTimeoutSec)
		}
	case KindAgent:
		if t.Shell != nil || t.Agent == nil {
			return invalidf("an %s task carries only an agent payload", KindAgent)
		}
		if t.SessionID == "" {
			return invalidf("an %s task needs a session id", KindAgent)
		}
		if n := len(t.Agent.Prompt); n == 0 || n > MaxPromptBytes {
			return invalidf("prompt must be 1-%d bytes", MaxPromptBytes)
		}
		switch t.Agent.Policy {
		case PolicyReadOnly, PolicyWorkspace, PolicyFull:
		default:
			return invalidf("policy must be %s, %s or %s", PolicyReadOnly, PolicyWorkspace, PolicyFull)
		}
	default:
		return invalidf("kind must be %s or %s", KindShell, KindAgent)
	}
	switch t.Trigger.Kind {
	case TriggerOnce:
		if t.Trigger.At.IsZero() {
			return invalidf("a once task needs a time")
		}
		if t.Trigger.Every != 0 {
			return invalidf("a once task has no interval")
		}
		if t.Trigger.Cron != "" {
			return invalidf("a once task has no cron expression")
		}
	case TriggerInterval:
		if t.Trigger.Every < minEvery {
			return invalidf("interval must be at least %s", minEvery)
		}
		if !t.Trigger.At.IsZero() {
			return invalidf("an interval task has no time")
		}
		if t.Trigger.Cron != "" {
			return invalidf("an interval task has no cron expression")
		}
	case TriggerCron:
		if !t.Trigger.At.IsZero() || t.Trigger.Every != 0 {
			return invalidf("a cron task has no time and no interval")
		}
		if t.Trigger.Cron == "" || len(t.Trigger.Cron) > MaxCronBytes {
			return invalidf("a cron task needs an expression of 1-%d bytes", MaxCronBytes)
		}
		expr, err := parseCron(t.Trigger.Cron)
		if err != nil {
			return err
		}
		loc, err := t.Trigger.location()
		if err != nil {
			return err
		}
		if _, err := expr.next(time.Now().UTC(), loc); err != nil {
			return invalidf("%v", err)
		}
	default:
		return invalidf("trigger kind must be %s or %s", TriggerOnce, TriggerInterval)
	}
	switch t.MissedPolicy {
	case MissedSkip, MissedRunOnce:
	default:
		return invalidf("missedPolicy must be %s or %s", MissedSkip, MissedRunOnce)
	}
	payload, err := payloadJSON(t)
	if err != nil {
		return err
	}
	if len(payload) > MaxPayloadBytes {
		return invalidf("payload must be at most %d bytes", MaxPayloadBytes)
	}
	return nil
}

// nextSlot is the task's next scheduled slot after the one it is on, given now. For an interval it is
// the previous slot plus the interval, advanced past now so a long absence never produces a burst of
// catch-up firings; for a cron expression it is the first match after now. A once task has none.
func nextSlot(t Task, now time.Time) (*time.Time, error) {
	switch t.Trigger.Kind {
	case TriggerOnce:
		return nil, nil
	case TriggerCron:
		expr, err := parseCron(t.Trigger.Cron)
		if err != nil {
			return nil, err
		}
		loc, err := t.Trigger.location()
		if err != nil {
			return nil, err
		}
		at, err := expr.next(now, loc)
		if err != nil {
			return nil, err
		}
		return &at, nil
	}
	if t.Trigger.Every <= 0 {
		return nil, nil
	}
	next := now.Add(t.Trigger.Every)
	if t.NextRunAt != nil {
		next = t.NextRunAt.Add(t.Trigger.Every)
		if !next.After(now) {
			// First slot strictly after now: the remainder of the current interval, never a
			// multiplication that could overflow.
			gap := now.Sub(next)
			next = now.Add(t.Trigger.Every - gap%t.Trigger.Every)
		}
	}
	return &next, nil
}
