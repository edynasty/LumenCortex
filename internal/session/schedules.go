package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Scheduled tasks (ADR-0019 §4, schema 0008). One row per timer; each Engine that opens the workspace
// runs one scheduler over the table, and a firing is claimed by one UPDATE guarded by the next_run_at
// the scheduler read (compare-and-set), so two processes sharing the database never fire the same
// occurrence twice. The SQL lives here like every other table's; internal/schedule decides when to call it.

// Task kinds, trigger kinds and missed policies the table accepts.
const (
	ScheduleKindShell = "shell"
	ScheduleKindAgent = "agent"

	ScheduleOnce     = "once"
	ScheduleInterval = "interval"
	ScheduleCron     = "cron"

	ScheduleSkip    = "skip"
	ScheduleRunOnce = "run_once"
)

// Last-status words the scheduler records in last_status. The column is validated in Go, not by a
// CHECK, so a later status needs no rebuild; these are the ones this build writes.
const (
	ScheduleStatusFired          = "fired"
	ScheduleStatusMissed         = "missed"
	ScheduleStatusSkippedOverlap = "skipped_overlap"
	ScheduleStatusFailed         = "failed"
)

// Column bounds, from 0008_schedules.sql.
const (
	MaxScheduleIDBytes      = 64
	MaxScheduleCronBytes    = 200
	MaxScheduleTimezone     = 64
	MaxScheduleNameBytes    = 200
	MaxSchedulePayloadBytes = 65536
	MaxScheduleStatusBytes  = 40
	MaxScheduleRefBytes     = 128
	MaxScheduleErrorBytes   = 4096
	// MinScheduleIntervalMS is the schema floor of interval_ms: a shorter period is a busy loop, not
	// a timer.
	MinScheduleIntervalMS = 1000
)

// scheduleTimeFormat is the fixed-width UTC layout of every time column of scheduled_tasks. A fixed
// width keeps the SQL comparisons (`next_run_at <= ?`, `next_run_at IS ?`) and the due index correct;
// RFC3339Nano drops trailing zeros, so two values of the same second would not order as strings.
const scheduleTimeFormat = "2006-01-02T15:04:05.000000000Z"

func scheduleTime(t time.Time) string { return t.UTC().Format(scheduleTimeFormat) }

func scheduleTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return scheduleTime(*t)
}

func parseScheduleTime(raw sql.NullString) *time.Time {
	if !raw.Valid || raw.String == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, raw.String)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

// ScheduledTask is one row of scheduled_tasks. Payload is the JSON object of the task's kind (the
// command and timeout of a shell task, the prompt and policy of an agent task); the session layer
// stores it as it is and does not interpret it.
type ScheduledTask struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Kind        string          `json:"kind"`
	SessionID   string          `json:"sessionId,omitempty"`
	Payload     json.RawMessage `json:"payload"`
	TriggerKind string          `json:"triggerKind"`
	RunAt       *time.Time      `json:"runAt,omitempty"`
	Interval    time.Duration   `json:"interval,omitempty"`
	Cron        string          `json:"cron,omitempty"`
	// Timezone is the IANA zone the cron expression is read in; "UTC" unless the task names one.
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

func (t ScheduledTask) validate() error {
	if !idRE.MatchString(t.ID) || len(t.ID) > MaxScheduleIDBytes {
		return invalid("schedule id must be 1-%d characters matching %s", MaxScheduleIDBytes, idRE)
	}
	if t.Name == "" || len(t.Name) > MaxScheduleNameBytes {
		return invalid("schedule name must be 1-%d bytes", MaxScheduleNameBytes)
	}
	if t.Kind != ScheduleKindShell && t.Kind != ScheduleKindAgent {
		return invalid("schedule kind must be %s or %s", ScheduleKindShell, ScheduleKindAgent)
	}
	if err := checkOptionalID("schedule session id", t.SessionID); err != nil {
		return err
	}
	if t.Kind == ScheduleKindAgent && t.SessionID == "" {
		return invalid("an agent schedule needs a session id")
	}
	if len(t.Payload) == 0 || len(t.Payload) > MaxSchedulePayloadBytes {
		return invalid("schedule payload must be 1-%d bytes", MaxSchedulePayloadBytes)
	}
	var object map[string]any
	if err := json.Unmarshal(t.Payload, &object); err != nil || object == nil {
		return invalid("schedule payload must be a JSON object")
	}
	switch t.TriggerKind {
	case ScheduleOnce:
		if t.RunAt == nil || t.Interval != 0 {
			return invalid("a once schedule has run_at and no interval")
		}
	case ScheduleInterval:
		if t.RunAt != nil || t.Interval < time.Duration(MinScheduleIntervalMS)*time.Millisecond {
			return invalid("an interval schedule has interval_ms >= %d and no run_at", MinScheduleIntervalMS)
		}
		if t.Cron != "" {
			return invalid("an interval schedule has no cron expression")
		}
	case ScheduleCron:
		if t.RunAt != nil || t.Interval != 0 {
			return invalid("a cron schedule has no run_at and no interval_ms")
		}
		if t.Cron == "" || len(t.Cron) > MaxScheduleCronBytes {
			return invalid("a cron schedule needs an expression of 1-%d bytes", MaxScheduleCronBytes)
		}
	default:
		return invalid("schedule trigger kind must be %s or %s", ScheduleOnce, ScheduleInterval)
	}
	if len(t.Timezone) > MaxScheduleTimezone {
		return invalid("schedule timezone must be at most %d bytes", MaxScheduleTimezone)
	}
	if t.MissedPolicy != ScheduleSkip && t.MissedPolicy != ScheduleRunOnce {
		return invalid("missed policy must be %s or %s", ScheduleSkip, ScheduleRunOnce)
	}
	if err := checkScheduleStatus(t.LastStatus); err != nil {
		return err
	}
	if len(t.LastRef) > MaxScheduleRefBytes {
		return invalid("schedule last ref must be at most %d bytes", MaxScheduleRefBytes)
	}
	if len(t.LastError) > MaxScheduleErrorBytes {
		return invalid("schedule last error must be at most %d bytes", MaxScheduleErrorBytes)
	}
	return nil
}

// scheduleStatusRE keeps last_status a short machine word: an empty value means "not run yet". It
// allows the underscore the scheduler's own words use (skipped_overlap).
var scheduleStatusRE = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,39}$`)

// checkScheduleStatus keeps last_status a short machine word.
func checkScheduleStatus(v string) error {
	if v == "" {
		return nil
	}
	if !scheduleStatusRE.MatchString(v) {
		return invalid("schedule status must match %s", scheduleStatusRE)
	}
	return nil
}

const scheduleColumns = `schedule_id, name, kind, session_id, payload_json, trigger_kind, run_at,
	interval_ms, cron_expr, timezone, missed_policy, enabled, next_run_at, last_fired_at, last_status,
	last_ref, last_error, fire_count, created_at, updated_at`

type scheduleScanner interface {
	Scan(dest ...any) error
}

func scanSchedule(row scheduleScanner) (ScheduledTask, error) {
	var (
		t                       ScheduledTask
		payload                 []byte
		sessionID, lastStatus   sql.NullString
		lastRef, lastError      sql.NullString
		runAt, nextAt, lastFire sql.NullString
		cron, timezone          sql.NullString
		intervalMS              sql.NullInt64
		enabled                 int64
		createdAt, updatedAt    sql.NullString
	)
	err := row.Scan(&t.ID, &t.Name, &t.Kind, &sessionID, &payload, &t.TriggerKind, &runAt,
		&intervalMS, &cron, &timezone, &t.MissedPolicy, &enabled, &nextAt, &lastFire, &lastStatus,
		&lastRef, &lastError, &t.FireCount, &createdAt, &updatedAt)
	if err != nil {
		return ScheduledTask{}, err
	}
	t.Payload = json.RawMessage(payload)
	t.SessionID = sessionID.String
	t.Cron = cron.String
	t.Timezone = timezone.String
	t.RunAt = parseScheduleTime(runAt)
	if intervalMS.Valid {
		t.Interval = time.Duration(intervalMS.Int64) * time.Millisecond
	}
	t.Enabled = enabled != 0
	t.NextRunAt = parseScheduleTime(nextAt)
	t.LastFiredAt = parseScheduleTime(lastFire)
	t.LastStatus = lastStatus.String
	t.LastRef = lastRef.String
	t.LastError = lastError.String
	if createdAt.Valid {
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, createdAt.String)
	}
	if updatedAt.Valid {
		t.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updatedAt.String)
	}
	return t, nil
}

// CreateSchedule inserts one task. A task whose session is gone is refused with ErrNotFound and a
// duplicate id with ErrExists.
func (s *Store) CreateSchedule(ctx context.Context, t ScheduledTask) error {
	if err := t.validate(); err != nil {
		return err
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.UpdatedAt.IsZero() {
		t.UpdatedAt = t.CreatedAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO scheduled_tasks(`+scheduleColumns+`)
		VALUES(?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, NULLIF(?, ''),
		NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?)`,
		t.ID, t.Name, t.Kind, t.SessionID, string(t.Payload), t.TriggerKind, scheduleTimePtr(t.RunAt),
		scheduleIntervalMS(t.Interval), t.Cron, scheduleTimezone(t.Timezone), t.MissedPolicy,
		boolInt(t.Enabled), scheduleTimePtr(t.NextRunAt), scheduleTimePtr(t.LastFiredAt), t.LastStatus,
		t.LastRef, t.LastError, t.FireCount, scheduleTime(t.CreatedAt), scheduleTime(t.UpdatedAt))
	if err != nil {
		return scheduleWriteError(err, t)
	}
	return nil
}

// scheduleTimezone is the zone column: the schema defaults it to UTC, and a task that names none keeps
// that.
func scheduleTimezone(name string) string {
	if name == "" {
		return "UTC"
	}
	return name
}

func scheduleIntervalMS(d time.Duration) any {
	if d <= 0 {
		return nil
	}
	return d.Milliseconds()
}

// scheduleWriteError names the constraint a write hit, so the caller can tell a vanished session from
// a duplicate id without parsing driver text.
func scheduleWriteError(err error, t ScheduledTask) error {
	text := err.Error()
	switch {
	case strings.Contains(text, "UNIQUE constraint failed"):
		return fmt.Errorf("%w: schedule %s", ErrExists, t.ID)
	case strings.Contains(text, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%w: session %s", ErrNotFound, t.SessionID)
	}
	return err
}

// GetSchedule returns one task; ErrNotFound when it is gone (a deleted session takes its tasks with it).
func (s *Store) GetSchedule(ctx context.Context, id string) (ScheduledTask, error) {
	if err := checkID("schedule id", id); err != nil {
		return ScheduledTask{}, err
	}
	t, err := scanSchedule(s.db.QueryRowContext(ctx, `SELECT `+scheduleColumns+` FROM scheduled_tasks WHERE schedule_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ScheduledTask{}, ErrNotFound
	}
	if err != nil {
		return ScheduledTask{}, err
	}
	return t, nil
}

// ListSchedules returns every task, oldest first.
func (s *Store) ListSchedules(ctx context.Context) ([]ScheduledTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM scheduled_tasks ORDER BY created_at, schedule_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduledTask{}
	for rows.Next() {
		t, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateSchedule writes the editable columns of a task: its name, kind and session, payload, trigger,
// missed policy, enablement and next_run_at. The last-firing columns and fire_count are written only by
// the firing paths below, so an update cannot erase what a concurrent firing recorded.
func (s *Store) UpdateSchedule(ctx context.Context, t ScheduledTask) error {
	if err := t.validate(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET
		name = ?, kind = ?, session_id = NULLIF(?, ''), payload_json = ?, trigger_kind = ?, run_at = ?,
		interval_ms = ?, cron_expr = NULLIF(?, ''), timezone = ?, missed_policy = ?, enabled = ?,
		next_run_at = ?, updated_at = ?
		WHERE schedule_id = ?`,
		t.Name, t.Kind, t.SessionID, string(t.Payload), t.TriggerKind, scheduleTimePtr(t.RunAt),
		scheduleIntervalMS(t.Interval), t.Cron, scheduleTimezone(t.Timezone), t.MissedPolicy,
		boolInt(t.Enabled), scheduleTimePtr(t.NextRunAt), scheduleTime(time.Now().UTC()), t.ID)
	if err != nil {
		return scheduleWriteError(err, t)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSchedule removes one task; ErrNotFound when it was already gone.
func (s *Store) DeleteSchedule(ctx context.Context, id string) error {
	if err := checkID("schedule id", id); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM scheduled_tasks WHERE schedule_id = ?`, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DueSchedules returns the enabled tasks whose next_run_at is at or before at, earliest first.
func (s *Store) DueSchedules(ctx context.Context, at time.Time) ([]ScheduledTask, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+scheduleColumns+` FROM scheduled_tasks
		WHERE enabled = 1 AND next_run_at IS NOT NULL AND next_run_at <= ?
		ORDER BY next_run_at, schedule_id`, scheduleTime(at))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScheduledTask{}
	for rows.Next() {
		t, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// EarliestScheduleDue is the next_run_at of the enabled task that comes due first; nil when no enabled
// task waits on a time.
func (s *Store) EarliestScheduleDue(ctx context.Context) (*time.Time, error) {
	var raw sql.NullString
	if err := s.db.QueryRowContext(ctx, `SELECT MIN(next_run_at) FROM scheduled_tasks
		WHERE enabled = 1 AND next_run_at IS NOT NULL`).Scan(&raw); err != nil {
		return nil, err
	}
	return parseScheduleTime(raw), nil
}

// ClaimSchedule takes the occurrence whose next_run_at is from, moving the task to next (nil for a
// one-shot's last firing) and counting the firing. claimed is false when another scheduler (in this
// process or another) already moved the row: the caller must then not run the task.
func (s *Store) ClaimSchedule(ctx context.Context, id string, from time.Time, next *time.Time, firedAt time.Time) (bool, error) {
	if err := checkID("schedule id", id); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET
		next_run_at = ?, last_fired_at = ?, fire_count = fire_count + 1, updated_at = ?
		WHERE schedule_id = ? AND next_run_at IS ? AND enabled = 1`,
		scheduleTimePtr(next), scheduleTime(firedAt), scheduleTime(firedAt), id, scheduleTime(from))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RevertScheduleClaim gives back a claim the task could not use: next_run_at becomes restoreNext (the
// slot it was claimed from when no agent runner is in this process, so the task stays due; a later
// retry time for a one-shot whose session is busy), last_fired_at returns to restoreFiredAt and
// fire_count loses the claim. claimed is the next_run_at the claim wrote. It is false when the row
// moved on.
func (s *Store) RevertScheduleClaim(ctx context.Context, id string, claimed, restoreNext, restoreFiredAt *time.Time) (bool, error) {
	if err := checkID("schedule id", id); err != nil {
		return false, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET
		next_run_at = ?, last_fired_at = ?, fire_count = CASE WHEN fire_count > 0 THEN fire_count - 1 ELSE 0 END,
		updated_at = ?
		WHERE schedule_id = ? AND next_run_at IS ?`,
		scheduleTimePtr(restoreNext), scheduleTimePtr(restoreFiredAt), scheduleTime(time.Now().UTC()),
		id, scheduleTimePtr(claimed))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkScheduleMissed advances a due task past an occurrence no scheduler ran, without firing it:
// next_run_at becomes next (nil for a one-shot, which is disabled too) and last_status becomes
// "missed". It is a compare-and-set on from, so a firing another process claimed first wins.
func (s *Store) MarkScheduleMissed(ctx context.Context, id string, from time.Time, next *time.Time, disable bool) (bool, error) {
	if err := checkID("schedule id", id); err != nil {
		return false, err
	}
	set := `next_run_at = ?, last_status = ?, last_error = NULL, updated_at = ?`
	args := []any{scheduleTimePtr(next), ScheduleStatusMissed, scheduleTime(time.Now().UTC())}
	if disable {
		set = `enabled = 0, ` + set
	}
	args = append(args, id, scheduleTime(from))
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET `+set+
		` WHERE schedule_id = ? AND next_run_at IS ? AND enabled = 1`, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// DisableSchedule stops a task whose stored trigger yields no next occurrence: it clears next_run_at,
// disables the row and records the reason, guarded by the slot the scheduler read. claimed is false when
// the row moved on.
func (s *Store) DisableSchedule(ctx context.Context, id string, from time.Time, status, errText string) (bool, error) {
	if err := checkID("schedule id", id); err != nil {
		return false, err
	}
	if err := checkScheduleStatus(status); err != nil {
		return false, err
	}
	if len(errText) > MaxScheduleErrorBytes {
		errText = errText[:MaxScheduleErrorBytes]
	}
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET
		enabled = 0, next_run_at = NULL, last_status = ?, last_error = NULLIF(?, ''), updated_at = ?
		WHERE schedule_id = ? AND next_run_at IS ?`,
		status, errText, scheduleTime(time.Now().UTC()), id, scheduleTime(from))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RecordScheduleResult writes the outcome of a firing: its status word, the ref of the job or run it
// started, and an error text. A task deleted in the meantime is ErrNotFound, not an error the
// scheduler must retry.
func (s *Store) RecordScheduleResult(ctx context.Context, id, status, ref, errText string) error {
	if err := checkID("schedule id", id); err != nil {
		return err
	}
	if err := checkScheduleStatus(status); err != nil {
		return err
	}
	if len(ref) > MaxScheduleRefBytes {
		return invalid("schedule ref must be at most %d bytes", MaxScheduleRefBytes)
	}
	if len(errText) > MaxScheduleErrorBytes {
		errText = errText[:MaxScheduleErrorBytes]
	}
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET
		last_status = ?, last_ref = NULLIF(?, ''), last_error = NULLIF(?, ''), updated_at = ?
		WHERE schedule_id = ?`, status, ref, errText, scheduleTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordScheduleObserved records the terminal status of the job or run a firing started, when ref is
// still the task's last firing: a later firing's ref is never overwritten by an older observation.
// observed is false when the task moved on or is gone.
func (s *Store) RecordScheduleObserved(ctx context.Context, id, ref, status string) (bool, error) {
	if err := checkID("schedule id", id); err != nil {
		return false, err
	}
	if err := checkScheduleStatus(status); err != nil {
		return false, err
	}
	if ref == "" || len(ref) > MaxScheduleRefBytes {
		return false, invalid("schedule ref must be 1-%d bytes", MaxScheduleRefBytes)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE scheduled_tasks SET last_status = ?, last_error = NULL, updated_at = ?
		WHERE schedule_id = ? AND last_ref = ?`, status, scheduleTime(time.Now().UTC()), id, ref)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
