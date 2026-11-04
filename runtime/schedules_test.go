package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/schedule"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/protocol"
)

// fakeScheduledJobs is the job queue's scheduled-task surface, as the job manager will bind it.
type fakeScheduledJobs struct {
	mu        sync.Mutex
	schedule  []string
	sessions  []string
	commands  []string
	timeouts  []time.Duration
	active    map[string]bool
	submitErr error
}

func (f *fakeScheduledJobs) SubmitScheduled(_ context.Context, scheduleID, sessionID, command string, timeout time.Duration) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.submitErr != nil {
		return "", f.submitErr
	}
	f.schedule = append(f.schedule, scheduleID)
	f.sessions = append(f.sessions, sessionID)
	f.commands = append(f.commands, command)
	f.timeouts = append(f.timeouts, timeout)
	return fmt.Sprintf("job_%d", len(f.commands)), nil
}

func (f *fakeScheduledJobs) JobActive(_ context.Context, jobID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active[jobID], nil
}

func (f *fakeScheduledJobs) submitted() (int, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.commands) == 0 {
		return 0, ""
	}
	return len(f.commands), f.commands[0]
}

// The Engine's schedule API creates, reads, updates and deletes tasks, and announces each change.
func TestEngineScheduleAPI(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	events, unsubscribe := engine.Events(256)
	defer unsubscribe()

	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "watch the build"})
	if err != nil {
		t.Fatal(err)
	}

	created, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "nightly build", Kind: "shell", Command: "make test", TimeoutSeconds: 600,
		Trigger: ScheduleTrigger{Kind: "interval", EverySeconds: 3600},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" || created.Kind != "shell" || created.Command != "make test" || created.TimeoutSeconds != 600 {
		t.Fatalf("created = %+v", created)
	}
	if created.Trigger.Kind != "interval" || created.Trigger.EverySeconds != 3600 {
		t.Fatalf("trigger = %+v", created.Trigger)
	}
	if created.MissedPolicy != "skip" || !created.Enabled || created.NextRunAt == nil {
		t.Fatalf("created defaults = %+v", created)
	}
	ev := waitForEvent(t, events, "schedule.changed")
	if got := string(ev.Data); got != fmt.Sprintf(`{"scheduleId":%q}`, created.ID) {
		t.Fatalf("changed event = %s", got)
	}

	agent, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "check", Kind: "agent", SessionID: handle.ID, Prompt: "check the build",
		Trigger: ScheduleTrigger{Kind: "once", At: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	if agent.Policy != schedule.PolicyReadOnly || agent.SessionID != handle.ID || agent.Trigger.At == "" {
		t.Fatalf("agent task = %+v", agent)
	}

	list, err := engine.Schedules(ctx)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %d tasks, err %v", len(list), err)
	}
	one, err := engine.Schedule(ctx, created.ID)
	if err != nil || one.Name != "nightly build" {
		t.Fatalf("get = %+v, err %v", one, err)
	}

	name := "renamed build"
	disabled := false
	updated, err := engine.UpdateSchedule(ctx, created.ID, SchedulePatch{Name: &name, Enabled: &disabled})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Name != "renamed build" || updated.Enabled || updated.NextRunAt != nil {
		t.Fatalf("updated = %+v", updated)
	}
	waitForEvent(t, events, "schedule.changed")

	enabled := true
	command := "make build"
	reenabled, err := engine.UpdateSchedule(ctx, created.ID, SchedulePatch{Enabled: &enabled, Command: &command})
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if !reenabled.Enabled || reenabled.NextRunAt == nil || reenabled.Command != "make build" {
		t.Fatalf("re-enabled = %+v", reenabled)
	}

	if err := engine.DeleteSchedule(ctx, agent.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	waitForEvent(t, events, "schedule.changed")
	if _, err := engine.Schedule(ctx, agent.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	list, err = engine.Schedules(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list after delete = %d, err %v", len(list), err)
	}

	// A refused request writes nothing.
	if _, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "bad", Kind: "shell", Command: "make",
		Trigger: ScheduleTrigger{Kind: "interval", EverySeconds: 10},
	}); !errors.Is(err, schedule.ErrInvalid) {
		t.Fatalf("short interval: err = %v, want ErrInvalid", err)
	}
	if _, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "bad", Kind: "shell", Command: "make", Trigger: ScheduleTrigger{Kind: "once", At: "tomorrow"},
	}); !errors.Is(err, schedule.ErrInvalid) {
		t.Fatalf("bad time: err = %v, want ErrInvalid", err)
	}
	prompt := "nope"
	if _, err := engine.UpdateSchedule(ctx, created.ID, SchedulePatch{Prompt: &prompt}); !errors.Is(err, schedule.ErrInvalid) {
		t.Fatalf("prompt on a shell task: err = %v, want ErrInvalid", err)
	}
}

// A shell task fires into the Engine's own job queue, bound at Open: the job it starts carries the
// schedule's id and source and really runs the command. A queue bound later (as the fake below) takes
// over with the submitted timeout.
func TestEngineShellScheduleFiresIntoJobQueue(t *testing.T) {
	workspace := t.TempDir()
	engine, err := Open(Options{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	events, unsubscribe := engine.Events(256)
	defer unsubscribe()

	early, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "early", Kind: "shell", Command: "printf fired > fired.txt",
		Trigger: ScheduleTrigger{Kind: "once", At: time.Now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	fired := waitForEvent(t, events, "schedule.fired")
	if want := `"status":"fired"`; !strings.Contains(string(fired.Data), want) {
		t.Fatalf("fired event = %s, want %s", fired.Data, want)
	}
	info, err := engine.Schedule(ctx, early.ID)
	if err != nil {
		t.Fatal(err)
	}
	job, err := engine.Job(ctx, info.LastRef)
	if err != nil {
		t.Fatalf("job %q of the firing: %v", info.LastRef, err)
	}
	if job.ScheduleID != early.ID || job.Source != session.JobSourceSchedule {
		t.Fatalf("job = %+v, want schedule %s, source schedule", job, early.ID)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if raw, err := os.ReadFile(filepath.Join(workspace, "fired.txt")); err == nil && string(raw) == "fired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduled command never ran")
		}
	}

	jobs := &fakeScheduledJobs{active: map[string]bool{}}
	engine.bindScheduledJobs(jobs)
	task, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "bound", Kind: "shell", Command: "make build", TimeoutSeconds: 120,
		Trigger: ScheduleTrigger{Kind: "once", At: time.Now().UTC().Format(time.RFC3339)},
	})
	if err != nil {
		t.Fatalf("create bound: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		count, command := jobs.submitted()
		if count > 0 {
			if command != "make build" {
				t.Fatalf("submitted command = %q", command)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the bound schedule never reached the job queue")
		}
		time.Sleep(5 * time.Millisecond)
	}
	deadline = time.Now().Add(10 * time.Second)
	for {
		info, err := engine.Schedule(ctx, task.ID)
		if err != nil {
			t.Fatal(err)
		}
		if info.LastStatus == "fired" {
			if info.LastRef != "job_1" || info.FireCount != 1 || info.NextRunAt != nil {
				t.Fatalf("info after firing = %+v", info)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status after firing = %q, want fired", info.LastStatus)
		}
		time.Sleep(5 * time.Millisecond)
	}
	jobs.mu.Lock()
	timeout := jobs.timeouts[0]
	jobs.mu.Unlock()
	if timeout != 120*time.Second {
		t.Fatalf("submitted timeout = %v, want 2m", timeout)
	}
}

// An agent task fires only once a RunSupervisor is registered: before that it stays due, and
// unregistering the supervisor keeps it due again.
func TestEngineAgentScheduleNeedsARunner(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()

	if engine.agentRunnerRef() != nil {
		t.Fatal("a runner is registered before NewRunSupervisor")
	}
	if _, err := (scheduleActions{engine: engine}).RunAgent(ctx, schedule.Task{Kind: schedule.KindAgent, Agent: &schedule.AgentPayload{Prompt: "x"}}); !errors.Is(err, schedule.ErrNoRunner) {
		t.Fatalf("RunAgent without a supervisor: %v, want ErrNoRunner", err)
	}

	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	if engine.agentRunnerRef() != supervisor {
		t.Fatal("NewRunSupervisor did not register the supervisor")
	}
	active, err := (scheduleActions{engine: engine}).Active(ctx, schedule.Task{Kind: schedule.KindAgent, SessionID: "session_x"}, "session_x")
	if err != nil || active {
		t.Fatalf("Active with no run: active=%v err=%v", active, err)
	}
	supervisor.Close()
	if engine.agentRunnerRef() != nil {
		t.Fatal("RunSupervisor.Close did not unregister the supervisor")
	}
	if _, err := (scheduleActions{engine: engine}).RunAgent(ctx, schedule.Task{Kind: schedule.KindAgent, Agent: &schedule.AgentPayload{Prompt: "x"}}); !errors.Is(err, schedule.ErrNoRunner) {
		t.Fatalf("RunAgent after Close: %v, want ErrNoRunner", err)
	}
}

// A firing's terminal status reaches the task through ObserveRef, and the scheduler's Close leaves
// nothing running behind the Engine's own Close.
func TestEngineSchedulerStopsWithTheEngine(t *testing.T) {
	dir := t.TempDir()
	engine, err := Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	jobs := &fakeScheduledJobs{active: map[string]bool{}}
	engine.bindScheduledJobs(jobs)
	if _, err := engine.CreateSchedule(context.Background(), ScheduleRequest{
		Name: "long", Kind: "shell", Command: "make",
		Trigger: ScheduleTrigger{Kind: "interval", EverySeconds: 3600},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	// A closed scheduler refuses work: nothing of it can fire after the Engine closed.
	if err := engine.schedules.Start(context.Background()); !errors.Is(err, schedule.ErrClosed) {
		t.Fatalf("start after the Engine closed: %v, want ErrClosed", err)
	}
}

// The wake-up tools create one-shot agent tasks on the calling session, with the run's policy, at
// most three pending, and only that session's wake-ups are reachable.
func TestEngineWakeupsAreScopedToOneSession(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	mine, err := engine.NewSession(ctx, SessionOptions{Goal: "wait for the build"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := engine.NewSession(ctx, SessionOptions{Goal: "someone else"})
	if err != nil {
		t.Fatal(err)
	}

	controller := engine.wakeupsFor(mine.ID, "workspace", nil, nil)
	if controller == nil {
		t.Fatal("no wake-up controller for a run with a session")
	}
	wakeup, err := controller.ScheduleWakeup(ctx, 60, "check whether the build finished")
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if wakeup.ID == "" || wakeup.FireAt == "" {
		t.Fatalf("wakeup = %+v", wakeup)
	}
	fireAt, err := time.Parse(time.RFC3339, wakeup.FireAt)
	if err != nil {
		t.Fatalf("fireAt %q: %v", wakeup.FireAt, err)
	}
	if d := time.Until(fireAt); d < 55*time.Second || d > 65*time.Second {
		t.Fatalf("fireAt = %s, want about a minute from now", wakeup.FireAt)
	}
	info, err := engine.Schedule(ctx, wakeup.ID)
	if err != nil {
		t.Fatalf("get wakeup: %v", err)
	}
	if info.Kind != "agent" || info.SessionID != mine.ID || info.Policy != "workspace" {
		t.Fatalf("wakeup task = %+v", info)
	}
	if info.Prompt != "[runtime] Scheduled wake-up: check whether the build finished" {
		t.Fatalf("prompt = %q", info.Prompt)
	}
	if info.Trigger.Kind != "once" || info.NextRunAt == nil || !info.NextRunAt.Equal(fireAt) {
		t.Fatalf("trigger = %+v, next = %v", info.Trigger, info.NextRunAt)
	}

	// Another session cannot cancel it, and its own controller sees none of it.
	otherController := engine.wakeupsFor(other.ID, "read-only", nil, nil)
	if err := otherController.CancelWakeup(ctx, wakeup.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("cancel from another session: %v, want ErrNotFound", err)
	}
	if _, err := engine.Schedule(ctx, wakeup.ID); err != nil {
		t.Fatalf("the task was deleted by another session: %v", err)
	}

	// Three pending wake-ups are the limit.
	for range 2 {
		if _, err := controller.ScheduleWakeup(ctx, 120, "again"); err != nil {
			t.Fatalf("schedule: %v", err)
		}
	}
	if _, err := controller.ScheduleWakeup(ctx, 120, "one too many"); err == nil {
		t.Fatal("a fourth pending wake-up was accepted")
	}
	if err := controller.CancelWakeup(ctx, wakeup.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := engine.Schedule(ctx, wakeup.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("get after cancel: %v", err)
	}
	// One slot is free again.
	if _, err := controller.ScheduleWakeup(ctx, 120, "after the cancel"); err != nil {
		t.Fatalf("schedule after cancel: %v", err)
	}
}

// wakeup_cancel reaches only wake-ups: a host-created agent schedule of the same session is reported as
// not found and stays (RT-state-17).
func TestWakeupCancelLeavesHostAgentSchedules(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()
	handle := newTestSession(t, engine, "review")
	host, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "daily review", Kind: "agent", SessionID: handle.ID, Prompt: "review the open PRs", Policy: "workspace",
		Trigger: ScheduleTrigger{Kind: "cron", Cron: "0 9 * * *"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.wakeupsFor(handle.ID, "read-only", nil, nil).CancelWakeup(ctx, host.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("wakeup_cancel of a host schedule: %v, want ErrNotFound", err)
	}
	if _, err := engine.Schedule(ctx, host.ID); err != nil {
		t.Fatalf("the host schedule after wakeup_cancel: %v", err)
	}
}

// A read-only run keeps the read-only policy on its wake-up: the ceiling never widens.
func TestEngineWakeupKeepsTheRunPolicy(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	handle, err := engine.NewSession(ctx, SessionOptions{Goal: "look around"})
	if err != nil {
		t.Fatal(err)
	}
	controller := engine.wakeupsFor(handle.ID, "", nil, nil)
	if controller == nil {
		t.Fatal("no wake-up controller")
	}
	wakeup, err := controller.ScheduleWakeup(ctx, 30, "look again")
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	info, err := engine.Schedule(ctx, wakeup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Policy != schedule.PolicyReadOnly {
		t.Fatalf("policy = %q, want %q", info.Policy, schedule.PolicyReadOnly)
	}
}

// The Engine API accepts a cron trigger with its zone and reports both back.
func TestEngineCronSchedule(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()

	info, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "weekday morning", Kind: "shell", Command: "make test",
		Trigger: ScheduleTrigger{Kind: "cron", Cron: "30 9 * * 1-5", Timezone: "America/New_York"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if info.Trigger.Kind != "cron" || info.Trigger.Cron != "30 9 * * 1-5" || info.Trigger.Timezone != "America/New_York" {
		t.Fatalf("trigger = %+v", info.Trigger)
	}
	if info.NextRunAt == nil {
		t.Fatal("a cron task has no next run")
	}
	// 09:30 New York is 13:30 or 14:30 UTC, never another time of day.
	if h := info.NextRunAt.UTC().Hour(); h != 13 && h != 14 {
		t.Fatalf("next run %s is not 09:30 in New York", info.NextRunAt.Format(time.RFC3339))
	}
	if info.Trigger.At != "" || info.Trigger.EverySeconds != 0 {
		t.Fatalf("a cron trigger carries no time or interval: %+v", info.Trigger)
	}

	// A default zone is reported as the UTC the row holds.
	plain, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "every hour", Kind: "shell", Command: "make",
		Trigger: ScheduleTrigger{Kind: "cron", Cron: "0 * * * *"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if plain.Trigger.Timezone != "UTC" {
		t.Fatalf("timezone = %q, want UTC", plain.Trigger.Timezone)
	}

	// Switching an interval task to cron recomputes its next run from the expression.
	interval, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "hourly", Kind: "shell", Command: "make",
		Trigger: ScheduleTrigger{Kind: "interval", EverySeconds: 3600},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	trigger := ScheduleTrigger{Kind: "cron", Cron: "0 3 * * *", Timezone: "UTC"}
	switched, err := engine.UpdateSchedule(ctx, interval.ID, SchedulePatch{Trigger: &trigger})
	if err != nil {
		t.Fatalf("switch to cron: %v", err)
	}
	if switched.Trigger.Kind != "cron" || switched.Trigger.Cron != "0 3 * * *" || switched.NextRunAt == nil {
		t.Fatalf("switched = %+v", switched)
	}
	if h, m := switched.NextRunAt.UTC().Hour(), switched.NextRunAt.UTC().Minute(); h != 3 || m != 0 {
		t.Fatalf("next run = %s, want 03:00 UTC", switched.NextRunAt.Format(time.RFC3339))
	}

	for name, trigger := range map[string]ScheduleTrigger{
		"no expression": {Kind: "cron"},
		"bad zone":      {Kind: "cron", Cron: "0 12 * * *", Timezone: "Nowhere/Land"},
		"bad fields":    {Kind: "cron", Cron: "0 12 * *"},
	} {
		if _, err := engine.CreateSchedule(ctx, ScheduleRequest{
			Name: "bad", Kind: "shell", Command: "make", Trigger: trigger,
		}); !errors.Is(err, schedule.ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// A shell task whose previous job was deleted by job retention fires again: a job that no longer
// exists is not running, so the overlap check lets the next firing through instead of failing every
// later one (RT-state-1).
func TestShellScheduleFiresAfterRetentionDeletedItsLastJob(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()
	events, unsubscribe := engine.Events(256)
	defer unsubscribe()
	info, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "every eight days", Kind: "shell", Command: "true",
		Trigger: ScheduleTrigger{Kind: "interval", EverySeconds: 8 * 24 * 3600},
	})
	if err != nil {
		t.Fatal(err)
	}
	// fireNow makes the task due and waits for the firing to be recorded.
	fireNow := func() ScheduleInfo {
		t.Helper()
		past := time.Now().UTC().Add(-time.Second).Format("2006-01-02T15:04:05.000000000Z")
		seedSQL(t, engine, []any{`UPDATE scheduled_tasks SET next_run_at = ? WHERE schedule_id = ?`, past, info.ID})
		engine.schedules.Wake()
		waitForEvent(t, events, "schedule.fired")
		got, err := engine.Schedule(ctx, info.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	first := fireNow()
	if first.LastStatus != session.ScheduleStatusFired || first.LastRef == "" {
		t.Fatalf("first firing: status=%q ref=%q", first.LastStatus, first.LastRef)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if active, err := engine.JobActive(ctx, first.LastRef); err == nil && !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s never finished", first.LastRef)
		}
	}
	// The retention sweep deletes the finished job row, as it does once JobRetentionDays passed.
	if _, err := engine.store.DeleteFinishedJobsBefore(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.store.GetJob(ctx, first.LastRef); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("job after the sweep: %v, want ErrNotFound", err)
	}
	second := fireNow()
	if second.LastStatus != session.ScheduleStatusFired || second.LastError != "" {
		t.Fatalf("firing after the sweep: status=%q error=%q, want fired", second.LastStatus, second.LastError)
	}
	if second.LastRef == "" || second.LastRef == first.LastRef {
		t.Fatalf("firing after the sweep started no new job: ref=%q (previous %q)", second.LastRef, first.LastRef)
	}
	if n := countRows(t, engine, `SELECT count(*) FROM jobs WHERE schedule_id = ?`, info.ID); n != 1 {
		t.Fatalf("jobs of the task after the sweep = %d, want the new one", n)
	}
}

// wakeupProvider scripts a run that schedules its own wake-up: its first request calls
// wakeup_schedule (1 s), its second waits for hold (nil: no wait) while the run keeps the session, and
// every later request (the woken run) answers at once. It records each request.
type wakeupProvider struct {
	hold     chan struct{}
	mu       sync.Mutex
	requests []protocol.ProviderRequest
}

func (p *wakeupProvider) Model() string { return "wakeup-test" }

func (p *wakeupProvider) Complete(ctx context.Context, request protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	n := len(p.requests)
	p.mu.Unlock()
	switch {
	case n == 1:
		return protocol.ProviderResponse{Message: protocol.Message{ToolCalls: []protocol.ToolCall{
			{ID: "wake", Name: "wakeup_schedule", Arguments: json.RawMessage(`{"delay_sec":1,"message":"resume"}`)},
		}}}, nil
	case n == 2 && p.hold != nil:
		select {
		case <-p.hold:
		case <-ctx.Done():
			return protocol.ProviderResponse{}, ctx.Err()
		}
	}
	return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
}

func (p *wakeupProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// toolNames is the sorted tool list of request i.
func (p *wakeupProvider) toolNames(i int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	names := make([]string, 0, len(p.requests[i].Tools))
	for _, tool := range p.requests[i].Tools {
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	return names
}

// waitRequests waits until the provider received n requests.
func (p *wakeupProvider) waitRequests(t *testing.T, n int, within time.Duration) {
	t.Helper()
	for deadline := time.Now().Add(within); p.count() < n; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the provider got %d requests in %v, want %d", p.count(), within, n)
		}
	}
}

// A wake-up that comes due while the run that scheduled it still holds the session is not lost: it
// stays pending and starts its run once that run ended (RT-state-2).
func TestWakeupDueDuringItsOwnRunFiresAfterTheRunEnds(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()
	handle := newTestSession(t, engine, "wait for the build")
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	provider := &wakeupProvider{hold: make(chan struct{})}
	if _, err := supervisor.startWith(ctx, handle.ID, provider, AgentOptions{Input: "go", Policy: "workspace", MaxSteps: 4}); err != nil {
		t.Fatal(err)
	}
	provider.waitRequests(t, 2, 10*time.Second)
	tasks, err := engine.Schedules(ctx)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("schedules = %+v, %v; want the wake-up", tasks, err)
	}
	wakeup := tasks[0]
	// The wake-up comes due (within 1 s) while the run is still in its second request.
	time.Sleep(2500 * time.Millisecond)
	info, err := engine.Schedule(ctx, wakeup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Enabled || info.NextRunAt == nil || info.LastStatus != "" || info.FireCount != 0 {
		t.Fatalf("wake-up while its run holds the session: enabled=%v next=%v status=%q count=%d, want pending",
			info.Enabled, info.NextRunAt, info.LastStatus, info.FireCount)
	}
	if n := provider.count(); n != 2 {
		t.Fatalf("requests while the run holds the session = %d, want 2", n)
	}

	close(provider.hold)
	provider.waitRequests(t, 3, 15*time.Second)
	waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
	provider.mu.Lock()
	woken := provider.requests[2].Messages
	provider.mu.Unlock()
	if !slices.ContainsFunc(woken, func(m protocol.Message) bool { return strings.Contains(m.Content, wakeupPromptPrefix+"resume") }) {
		t.Fatalf("the woken run's request carries no wake-up prompt: %+v", woken)
	}
	info, err = engine.Schedule(ctx, wakeup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.LastStatus != session.ScheduleStatusFired || info.FireCount != 1 || info.NextRunAt != nil {
		t.Fatalf("wake-up after its run: status=%q count=%d next=%v, want fired once", info.LastStatus, info.FireCount, info.NextRunAt)
	}
}

// answeringProvider answers every request at once and records each, safe across runs.
type answeringProvider struct {
	mu       sync.Mutex
	requests []protocol.ProviderRequest
}

func (p *answeringProvider) Model() string { return "answering-test" }

func (p *answeringProvider) Complete(_ context.Context, request protocol.ProviderRequest) (protocol.ProviderResponse, error) {
	p.mu.Lock()
	p.requests = append(p.requests, request)
	p.mu.Unlock()
	return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}}, nil
}

func (p *answeringProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.requests)
}

// A wake-up that comes due while the Engine runs as many agents as it may is not lost: the refused
// start is busy, as a held session is, so the wake-up stays pending and starts its run once a slot is
// free (RT-state-19).
func TestWakeupDueAtTheAgentLimitFiresWhenASlotFrees(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir(), Budget: Budget{MaxAgents: 1}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	events, unsubscribe := engine.Events(256)
	defer unsubscribe()
	supervisor, err := NewRunSupervisor(engine)
	if err != nil {
		t.Fatal(err)
	}
	defer supervisor.Close()
	waking := newTestSession(t, engine, "wake up later")
	busy := newTestSession(t, engine, "keep the only slot")

	// A first run binds the waking session's model and ends, freeing the only slot.
	provider := &answeringProvider{}
	if _, err := supervisor.startWith(ctx, waking.ID, provider, AgentOptions{Input: "go", Policy: "workspace", MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	waitForRunStopped(t, events, waking.ID)
	// Another session takes the slot and keeps it.
	if _, err := supervisor.startWith(ctx, busy.ID, blockedProvider(), AgentOptions{MaxSteps: 2}); err != nil {
		t.Fatal(err)
	}
	wakeup, err := engine.wakeupsFor(waking.ID, "workspace", nil, nil).ScheduleWakeup(ctx, 1, "resume")
	if err != nil {
		t.Fatal(err)
	}
	// The wake-up comes due (within 1 s) while the slot is taken.
	time.Sleep(2500 * time.Millisecond)
	info, err := engine.Schedule(ctx, wakeup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Enabled || info.NextRunAt == nil || info.LastStatus != "" || info.FireCount != 0 {
		t.Fatalf("wake-up at the agent limit: enabled=%v next=%v status=%q error=%q count=%d, want pending",
			info.Enabled, info.NextRunAt, info.LastStatus, info.LastError, info.FireCount)
	}
	before := provider.count()

	supervisor.Cancel(busy.ID)
	for deadline := time.Now().Add(15 * time.Second); provider.count() == before; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the wake-up never started its run after the slot was freed")
		}
	}
	waitUntilIdle(t, supervisor, waking.ID, 10*time.Second)
	info, err = engine.Schedule(ctx, wakeup.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.LastStatus != session.ScheduleStatusFired || info.FireCount != 1 || info.NextRunAt != nil {
		t.Fatalf("wake-up after the slot freed: status=%q count=%d next=%v, want fired once", info.LastStatus, info.FireCount, info.NextRunAt)
	}
}

// A wake-up keeps the host tool filters of the run that scheduled it: the woken run's first request
// offers exactly the tools the creating run's first request offered, so a denied command tool (by its
// current or its former name) or an allowlist does not come back with the wake-up. The workflow binding
// of the session survives as well (capability U1).
func TestWakeupKeepsTheRunToolFilters(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts AgentOptions
	}{
		{"denylist", AgentOptions{ToolDenylist: []string{"bash"}}},
		{"denylist former name", AgentOptions{ToolDenylist: []string{"shell"}}},
		{"allowlist", AgentOptions{ToolAllowlist: []string{"read_file", "wakeup_schedule"}}},
		{"workflow", AgentOptions{Workflow: json.RawMessage(`{"id":"wf","facts":{"ready":true},"actions":{"work":{"terminal":true,"completeWhen":{"fact":"ready","equals":true},"allowedTools":["read_file","wakeup_schedule"]}}}`)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			engine := openTestEngine(t)
			handle := newTestSession(t, engine, "wake up later")
			supervisor, err := NewRunSupervisor(engine)
			if err != nil {
				t.Fatal(err)
			}
			defer supervisor.Close()
			provider := &wakeupProvider{}
			opts := tc.opts
			opts.Input, opts.Policy, opts.MaxSteps = "go", "full", 4
			if _, err := supervisor.startWith(context.Background(), handle.ID, provider, opts); err != nil {
				t.Fatal(err)
			}
			// Request 1 schedules the wake-up, request 2 ends the run, request 3 is the woken run's first.
			provider.waitRequests(t, 3, 15*time.Second)
			waitUntilIdle(t, supervisor, handle.ID, 10*time.Second)
			creating, woken := provider.toolNames(0), provider.toolNames(2)
			if !slices.Contains(creating, "wakeup_schedule") {
				t.Fatalf("the creating run was not offered wakeup_schedule: %v", creating)
			}
			if !slices.Equal(woken, creating) {
				t.Fatalf("the woken run offers %v, want the creating run's %v", woken, creating)
			}
			if tc.opts.ToolAllowlist != nil || tc.opts.ToolDenylist != nil || tc.opts.Workflow != nil {
				if slices.Contains(woken, "bash") {
					t.Fatalf("the woken run offers bash: %v", woken)
				}
			}
		})
	}
}

// A wake-up stores the run's tool filters by their current names, and a host update of its prompt or
// policy keeps them (capability U1).
func TestWakeupToolFiltersSurviveAHostUpdate(t *testing.T) {
	engine := openTestEngine(t)
	ctx := context.Background()
	handle := newTestSession(t, engine, "wake up later")
	wakeup, err := engine.wakeupsFor(handle.ID, "full", []string{"read_file", "wakeup_schedule"}, []string{"shell"}).
		ScheduleWakeup(ctx, 600, "resume")
	if err != nil {
		t.Fatal(err)
	}
	check := func(when string) {
		t.Helper()
		task, err := engine.schedules.Get(ctx, wakeup.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.Agent == nil || !slices.Equal(task.Agent.ToolAllowlist, []string{"read_file", "wakeup_schedule"}) ||
			!slices.Equal(task.Agent.ToolDenylist, []string{"bash"}) {
			t.Fatalf("%s: payload = %+v, want the run's filters by their current names", when, task.Agent)
		}
	}
	check("created")
	prompt, policy := "resume and report", "read-only"
	if _, err := engine.UpdateSchedule(ctx, wakeup.ID, SchedulePatch{Prompt: &prompt, Policy: &policy}); err != nil {
		t.Fatal(err)
	}
	check("after a host update")
}
