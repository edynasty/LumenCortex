package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/schedule"
	"github.com/edynasty/LumenCortex/internal/session"
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

// A shell task fires into the job queue once it is bound; before that it fails with a clear error
// rather than pretending to have run.
func TestEngineShellScheduleFiresIntoJobQueue(t *testing.T) {
	engine, err := Open(Options{Workspace: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	ctx := context.Background()
	events, unsubscribe := engine.Events(256)
	defer unsubscribe()

	// Unbound: the firing is recorded as failed with the queue's absence named.
	if _, err := engine.CreateSchedule(ctx, ScheduleRequest{
		Name: "early", Kind: "shell", Command: "make test",
		Trigger: ScheduleTrigger{Kind: "once", At: time.Now().UTC().Format(time.RFC3339)},
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	fired := waitForEvent(t, events, "schedule.fired")
	if want := `"status":"failed"`; !strings.Contains(string(fired.Data), want) {
		t.Fatalf("fired event = %s, want %s", fired.Data, want)
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

	controller := engine.wakeupsFor(mine.ID, "workspace")
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
	otherController := engine.wakeupsFor(other.ID, "read-only")
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
	controller := engine.wakeupsFor(handle.ID, "")
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
