package session

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newScheduleFixture opens a store with one session, for tasks that need one.
func newScheduleFixture(t *testing.T) (*Store, string) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), ".lumencortex"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const id = "session_sched"
	if err := store.Create(context.Background(), Session{ID: id, Goal: "scheduled work"}); err != nil {
		t.Fatal(err)
	}
	return store, id
}

func intervalTask(id, name string, every time.Duration, next time.Time) ScheduledTask {
	return ScheduledTask{
		ID: id, Name: name, Kind: ScheduleKindShell,
		Payload:     []byte(`{"command":"make test"}`),
		TriggerKind: ScheduleInterval, Interval: every, MissedPolicy: ScheduleSkip,
		Enabled: true, NextRunAt: &next,
	}
}

func TestScheduleRowsRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, sessionID := newScheduleFixture(t)
	next := time.Date(2026, 11, 1, 9, 0, 0, 123456000, time.UTC)
	at := next.Add(time.Hour)
	task := ScheduledTask{
		ID: "schedule_once", Name: "one shot", Kind: ScheduleKindAgent, SessionID: sessionID,
		Payload:     []byte(`{"prompt":"check the build","policy":"read-only"}`),
		TriggerKind: ScheduleOnce, RunAt: &at, MissedPolicy: ScheduleRunOnce, Enabled: true, NextRunAt: &at,
	}
	if err := store.CreateSchedule(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := store.GetSchedule(ctx, task.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != task.Name || got.Kind != task.Kind || got.SessionID != sessionID {
		t.Fatalf("row = %+v", got)
	}
	if got.TriggerKind != ScheduleOnce || got.RunAt == nil || !got.RunAt.Equal(at) {
		t.Fatalf("trigger: kind=%q run_at=%v", got.TriggerKind, got.RunAt)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(at) {
		t.Fatalf("next run = %v, want %v", got.NextRunAt, at)
	}
	if got.Interval != 0 || got.FireCount != 0 || got.LastStatus != "" || got.MissedPolicy != ScheduleRunOnce {
		t.Fatalf("row = %+v", got)
	}

	// A second task: list is oldest first and the due query honours its bound.
	earlier := next
	if err := store.CreateSchedule(ctx, intervalTask("schedule_interval", "every hour", time.Hour, earlier)); err != nil {
		t.Fatalf("create interval: %v", err)
	}
	all, err := store.ListSchedules(ctx)
	if err != nil || len(all) != 2 {
		t.Fatalf("list = %d rows, err %v", len(all), err)
	}
	due, err := store.DueSchedules(ctx, next.Add(-time.Second))
	if err != nil || len(due) != 0 {
		t.Fatalf("due before the slot = %d, err %v", len(due), err)
	}
	due, err = store.DueSchedules(ctx, next)
	if err != nil || len(due) != 1 || due[0].ID != "schedule_interval" {
		t.Fatalf("due at the interval slot = %+v, err %v", due, err)
	}
	due, err = store.DueSchedules(ctx, at)
	if err != nil || len(due) != 2 || due[0].ID != "schedule_interval" || due[1].ID != "schedule_once" {
		t.Fatalf("due at the later slot = %+v, err %v", due, err)
	}
	earliest, err := store.EarliestScheduleDue(ctx)
	if err != nil || earliest == nil || !earliest.Equal(next) {
		t.Fatalf("earliest = %v, err %v", earliest, err)
	}

	// A firing counts the occurrence and records its ref; an update afterwards changes the editable
	// columns only, so it cannot erase what the firing wrote.
	later := next.Add(time.Hour)
	if ok, err := store.ClaimSchedule(ctx, "schedule_interval", next, &later, next); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	if err := store.RecordScheduleResult(ctx, "schedule_interval", ScheduleStatusFired, "job_1", ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	disabled := false
	row := mustRow(t, store, "schedule_interval")
	read := row.NextRunAt
	row.Name = "renamed"
	row.Enabled = disabled
	row.NextRunAt = nil
	// An update is a compare-and-set on the next_run_at it read: a stale one writes nothing.
	if ok, err := store.UpdateSchedule(ctx, row, &next); err != nil || ok {
		t.Fatalf("update with a stale next_run_at: ok=%v err=%v", ok, err)
	}
	if got := mustRow(t, store, "schedule_interval"); got.Name == "renamed" || got.NextRunAt == nil || !got.NextRunAt.Equal(later) {
		t.Fatalf("a stale update wrote the row: %+v", got)
	}
	if ok, err := store.UpdateSchedule(ctx, row, read); err != nil || !ok {
		t.Fatalf("update: ok=%v err=%v", ok, err)
	}
	missing := row
	missing.ID = "schedule_missing"
	if _, err := store.UpdateSchedule(ctx, missing, read); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing task: %v, want ErrNotFound", err)
	}
	got = mustRow(t, store, "schedule_interval")
	if got.Name != "renamed" || got.Enabled {
		t.Fatalf("row after update = %+v", got)
	}
	if got.FireCount != 1 || got.LastStatus != ScheduleStatusFired || got.LastRef != "job_1" {
		t.Fatalf("update erased firing columns: %+v", got)
	}
	if got.LastFiredAt == nil || !got.LastFiredAt.Equal(next) {
		t.Fatalf("last fired at = %v, want %v", got.LastFiredAt, next)
	}
	if earliest, err = store.EarliestScheduleDue(ctx); err != nil || earliest == nil || !earliest.Equal(at) {
		t.Fatalf("earliest after disabling = %v, err %v", earliest, err)
	}

	if err := store.DeleteSchedule(ctx, "schedule_interval"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.GetSchedule(ctx, "schedule_interval"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	if err := store.DeleteSchedule(ctx, "schedule_interval"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func mustRow(t *testing.T, store *Store, id string) ScheduledTask {
	t.Helper()
	row, err := store.GetSchedule(context.Background(), id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return row
}

func TestScheduleRefusesInvalidRows(t *testing.T) {
	ctx := context.Background()
	store, sessionID := newScheduleFixture(t)
	next := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	cases := map[string]ScheduledTask{
		"empty name": func() ScheduledTask { r := intervalTask("schedule_a", "", time.Minute, next); return r }(),
		"bad kind": func() ScheduledTask {
			r := intervalTask("schedule_a", "n", time.Minute, next)
			r.Kind = "cron"
			return r
		}(),
		"agent no session": func() ScheduledTask {
			r := intervalTask("schedule_a", "n", time.Minute, next)
			r.Kind = ScheduleKindAgent
			return r
		}(),
		"payload not object": func() ScheduledTask {
			r := intervalTask("schedule_a", "n", time.Minute, next)
			r.Payload = []byte(`[]`)
			return r
		}(),
		"short interval": func() ScheduledTask { r := intervalTask("schedule_a", "n", time.Millisecond, next); return r }(),
		"once with interval": func() ScheduledTask {
			r := intervalTask("schedule_a", "n", time.Minute, next)
			r.TriggerKind = ScheduleOnce
			r.RunAt = &next
			return r
		}(),
		"bad missed policy": func() ScheduledTask {
			r := intervalTask("schedule_a", "n", time.Minute, next)
			r.MissedPolicy = "later"
			return r
		}(),
		"bad status": func() ScheduledTask {
			r := intervalTask("schedule_a", "n", time.Minute, next)
			r.LastStatus = "Completed!"
			return r
		}(),
	}
	for name, task := range cases {
		if err := store.CreateSchedule(ctx, task); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}

	// A duplicate id and a vanished session are named for the caller, not left as driver text.
	task := intervalTask("schedule_dup", "n", time.Minute, next)
	if err := store.CreateSchedule(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.CreateSchedule(ctx, task); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate: err = %v, want ErrExists", err)
	}
	orphan := intervalTask("schedule_orphan", "n", time.Minute, next)
	orphan.Kind, orphan.SessionID = ScheduleKindAgent, "session_missing"
	orphan.Payload = []byte(`{"prompt":"x","policy":"read-only"}`)
	if err := store.CreateSchedule(ctx, orphan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan: err = %v, want ErrNotFound", err)
	}
	_ = sessionID
}

// Two connections to one database claim the same occurrence once: the compare-and-set decides, and the
// loser runs nothing. This is the cross-process guarantee at the SQL layer.
func TestClaimScheduleIsCompareAndSet(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".lumencortex")
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	slot := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err := first.CreateSchedule(ctx, intervalTask("schedule_race", "race", time.Minute, slot)); err != nil {
		t.Fatal(err)
	}
	next := slot.Add(time.Minute)
	const racers = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed int
	)
	for i := range racers {
		store := first
		if i%2 == 1 {
			store = second
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.ClaimSchedule(ctx, "schedule_race", slot, &next, slot)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if ok {
				mu.Lock()
				claimed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if claimed != 1 {
		t.Fatalf("%d claims succeeded, want exactly 1", claimed)
	}
	row := mustRow(t, first, "schedule_race")
	if row.FireCount != 1 || row.NextRunAt == nil || !row.NextRunAt.Equal(next) {
		t.Fatalf("row after the race: count=%d next=%v", row.FireCount, row.NextRunAt)
	}

	// A stale claim (the slot the row no longer has) is refused, and a disabled task cannot be claimed.
	if ok, err := second.ClaimSchedule(ctx, "schedule_race", slot, &next, slot); err != nil || ok {
		t.Fatalf("stale claim: ok=%v err=%v", ok, err)
	}
	row = mustRow(t, first, "schedule_race")
	row.Enabled = false
	row.NextRunAt = &next
	if ok, err := first.UpdateSchedule(ctx, row, &next); err != nil || !ok {
		t.Fatalf("update: ok=%v err=%v", ok, err)
	}
	later := next.Add(time.Minute)
	if ok, err := second.ClaimSchedule(ctx, "schedule_race", next, &later, next); err != nil || ok {
		t.Fatalf("claim of a disabled task: ok=%v err=%v", ok, err)
	}
}

// A claim given back (no runner) restores the slot and the counter, so the task stays due.
func TestRevertScheduleClaimKeepsTaskDue(t *testing.T) {
	ctx := context.Background()
	store, _ := newScheduleFixture(t)
	slot := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSchedule(ctx, intervalTask("schedule_revert", "revert", time.Minute, slot)); err != nil {
		t.Fatal(err)
	}
	next := slot.Add(time.Minute)
	ok, err := store.ClaimSchedule(ctx, "schedule_revert", slot, &next, slot)
	if err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	ok, err = store.RevertScheduleClaim(ctx, "schedule_revert", &next, &slot, nil)
	if err != nil || !ok {
		t.Fatalf("revert: ok=%v err=%v", ok, err)
	}
	row := mustRow(t, store, "schedule_revert")
	if row.FireCount != 0 || row.LastFiredAt != nil {
		t.Fatalf("row after revert: count=%d fired=%v", row.FireCount, row.LastFiredAt)
	}
	if row.NextRunAt == nil || !row.NextRunAt.Equal(slot) {
		t.Fatalf("next run after revert = %v, want %v", row.NextRunAt, slot)
	}
	due, err := store.DueSchedules(ctx, slot)
	if err != nil || len(due) != 1 {
		t.Fatalf("due after revert = %d, err %v", len(due), err)
	}
}

func TestScheduleRecordsResults(t *testing.T) {
	ctx := context.Background()
	store, _ := newScheduleFixture(t)
	slot := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	if err := store.CreateSchedule(ctx, intervalTask("schedule_result", "result", time.Minute, slot)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordScheduleResult(ctx, "schedule_result", ScheduleStatusFired, "job_7", ""); err != nil {
		t.Fatalf("record: %v", err)
	}
	row := mustRow(t, store, "schedule_result")
	if row.LastStatus != ScheduleStatusFired || row.LastRef != "job_7" || row.LastError != "" {
		t.Fatalf("row = %+v", row)
	}

	// An observation of the current ref lands; one of an older ref does not.
	if ok, err := store.RecordScheduleObserved(ctx, "schedule_result", "job_7", "completed"); err != nil || !ok {
		t.Fatalf("observe: ok=%v err=%v", ok, err)
	}
	if row = mustRow(t, store, "schedule_result"); row.LastStatus != "completed" {
		t.Fatalf("status = %q, want completed", row.LastStatus)
	}
	if ok, err := store.RecordScheduleObserved(ctx, "schedule_result", "job_6", "failed"); err != nil || ok {
		t.Fatalf("stale observe: ok=%v err=%v", ok, err)
	}
	if row = mustRow(t, store, "schedule_result"); row.LastStatus != "completed" {
		t.Fatalf("a stale observation overwrote the status: %q", row.LastStatus)
	}

	// A missed occurrence advances the slot without counting a firing; a one-shot is disabled.
	if ok, err := store.MarkScheduleMissed(ctx, "schedule_result", slot, nil, true); err != nil || !ok {
		t.Fatalf("mark missed: ok=%v err=%v", ok, err)
	}
	row = mustRow(t, store, "schedule_result")
	if row.LastStatus != ScheduleStatusMissed || row.Enabled || row.NextRunAt != nil || row.FireCount != 0 {
		t.Fatalf("row after missed = %+v", row)
	}
	if ok, err := store.MarkScheduleMissed(ctx, "schedule_result", slot, nil, true); err != nil || ok {
		t.Fatalf("second mark missed: ok=%v err=%v (the slot is gone)", ok, err)
	}

	// Recording on a vanished row is ErrNotFound, not a silent success.
	if err := store.RecordScheduleResult(ctx, "schedule_gone", ScheduleStatusFired, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("record on a missing row: %v", err)
	}
}

// Deleting a session deletes its scheduled tasks: the payload is content that belongs to the session.
func TestSessionDeletionRemovesScheduledTasks(t *testing.T) {
	ctx := context.Background()
	store, sessionID := newScheduleFixture(t)
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	task := ScheduledTask{
		ID: "schedule_cascade", Name: "cascade", Kind: ScheduleKindAgent, SessionID: sessionID,
		Payload:     []byte(`{"prompt":"check","policy":"read-only"}`),
		TriggerKind: ScheduleOnce, RunAt: &at, MissedPolicy: ScheduleSkip, Enabled: true, NextRunAt: &at,
	}
	if err := store.CreateSchedule(ctx, task); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, sessionID); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := store.GetSchedule(ctx, task.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the session's task survived: %v", err)
	}
}

// A cron task keeps its expression and zone, and the schema refuses a row that carries the wrong
// combination of trigger columns.
func TestScheduleCronRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, _ := newScheduleFixture(t)
	next := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)
	task := ScheduledTask{
		ID: "schedule_cron", Name: "noon in Berlin", Kind: ScheduleKindShell,
		Payload:     []byte(`{"command":"make"}`),
		TriggerKind: ScheduleCron, Cron: "0 12 * * *", Timezone: "Europe/Berlin",
		MissedPolicy: ScheduleSkip, Enabled: true, NextRunAt: &next,
	}
	if err := store.CreateSchedule(ctx, task); err != nil {
		t.Fatalf("create: %v", err)
	}
	got := mustRow(t, store, task.ID)
	if got.Cron != "0 12 * * *" || got.Timezone != "Europe/Berlin" || got.Interval != 0 || got.RunAt != nil {
		t.Fatalf("row = %+v", got)
	}
	if got.NextRunAt == nil || !got.NextRunAt.Equal(next) {
		t.Fatalf("next run = %v", got.NextRunAt)
	}

	// A task that names no zone is UTC, which is the column's default.
	plain := intervalTask("schedule_utc", "utc", time.Minute, next)
	plain.Timezone = ""
	if err := store.CreateSchedule(ctx, plain); err != nil {
		t.Fatalf("create without a zone: %v", err)
	}
	if got = mustRow(t, store, "schedule_utc"); got.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want UTC", got.Timezone)
	}

	// The trigger columns are consistent or the row is refused, in Go and in SQL.
	for name, broken := range map[string]ScheduledTask{
		"cron without an expression": func() ScheduledTask {
			r := intervalTask("schedule_bad", "bad", time.Minute, next)
			r.TriggerKind = ScheduleCron
			r.Interval = 0
			return r
		}(),
		"cron with an interval": func() ScheduledTask {
			r := intervalTask("schedule_bad", "bad", time.Minute, next)
			r.TriggerKind = ScheduleCron
			r.Cron = "0 12 * * *"
			return r
		}(),
		"interval with an expression": func() ScheduledTask {
			r := intervalTask("schedule_bad", "bad", time.Minute, next)
			r.Cron = "0 12 * * *"
			return r
		}(),
	} {
		if err := store.CreateSchedule(ctx, broken); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO scheduled_tasks(schedule_id, name, kind, payload_json,
		trigger_kind, interval_ms, cron_expr, next_run_at, created_at, updated_at)
		VALUES('schedule_sql', 'n', 'shell', '{"command":"make"}', 'cron', 60000, '0 12 * * *', NULL, 't', 't')`); err == nil {
		t.Fatal("the schema accepted a cron row that also carries an interval")
	}
}

// A schedule journal row carries the task's id, kind and status, and never its command or prompt: the
// payload stays in scheduled_tasks.
func TestScheduleJournalIsContentFree(t *testing.T) {
	store, journal := journalTestStore(t)
	ctx := context.Background()
	if err := store.Journal(ctx, "schedule.fired", map[string]any{
		"scheduleId": "schedule_1", "kind": "shell", "status": "fired", "ref": "job_1",
		"command": "curl -H 'authorization: bearer secret' https://example.test",
	}); err != nil {
		t.Fatal(err)
	}
	payload := journal.last()
	if payload["scheduleId"] != "schedule_1" || payload["kind"] != "shell" || payload["status"] != "fired" {
		t.Fatalf("payload = %v", payload)
	}
	if _, ok := payload["command"]; ok {
		t.Fatalf("the journal kept the command: %v", payload)
	}
}
