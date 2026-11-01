package schedule

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/edynasty/LumenCortex/internal/session"
)

// fakeClock is the scheduler's injectable clock. Tests advance it by hand, so a timer fires when the
// test says so and never because wall time passed.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(t time.Time) *fakeClock { return &fakeClock{t: t} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type firedEvent struct {
	event string
	task  Task
}

// fakeActions records firings and answers the overlap question from a set of refs it was told are
// still active.
type fakeActions struct {
	mu        sync.Mutex
	shells    []string
	agents    []string
	refs      []string
	active    map[string]bool
	activeErr error
	runErr    error
	noRunner  bool
	events    chan firedEvent
}

func newFakeActions() *fakeActions {
	return &fakeActions{active: map[string]bool{}, events: make(chan firedEvent, 64)}
}

func (a *fakeActions) RunShell(_ context.Context, t Task) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.shells = append(a.shells, t.Shell.Command)
	return a.refLocked(t)
}

func (a *fakeActions) RunAgent(_ context.Context, t Task) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.agents = append(a.agents, t.Agent.Prompt)
	return a.refLocked(t)
}

func (a *fakeActions) refLocked(t Task) (string, error) {
	if a.noRunner {
		return "", ErrNoRunner
	}
	if a.runErr != nil {
		return "", a.runErr
	}
	ref := fmt.Sprintf("ref-%d", len(a.refs)+1)
	a.refs = append(a.refs, ref)
	return ref, nil
}

func (a *fakeActions) Active(_ context.Context, _ Task, ref string) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.activeErr != nil {
		return false, a.activeErr
	}
	return a.active[ref], nil
}

func (a *fakeActions) count() (shells, agents int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.shells), len(a.agents)
}

func (a *fakeActions) setActive(ref string, active bool) {
	a.mu.Lock()
	a.active[ref] = active
	a.mu.Unlock()
}

// notify forwards the scheduler's events to the test's channel without blocking the scheduler.
func (a *fakeActions) notify(event string, t Task) {
	select {
	case a.events <- firedEvent{event: event, task: t}:
	default:
	}
}

// openScheduleStore opens the real store under dir, running the real migrations.
func openScheduleStore(t *testing.T, dir string) *session.Store {
	t.Helper()
	store, err := session.Open(dir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func newTestScheduler(t *testing.T, store Store, actions Actions, clock *fakeClock, notify func(string, Task)) *Scheduler {
	t.Helper()
	if notify == nil {
		if fake, ok := actions.(*fakeActions); ok {
			notify = fake.notify
		}
	}
	s, err := New(Options{
		Store:       store,
		Actions:     actions,
		Now:         clock.Now,
		MinInterval: time.Second,
		Notify:      notify,
	})
	if err != nil {
		t.Fatalf("new scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func waitEvent(t *testing.T, events <-chan firedEvent, want string) firedEvent {
	t.Helper()
	select {
	case ev := <-events:
		if ev.event != want {
			t.Fatalf("event=%q want %q", ev.event, want)
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatalf("no %q event", want)
	}
	return firedEvent{}
}

func noEvent(t *testing.T, events <-chan firedEvent) {
	t.Helper()
	select {
	case ev := <-events:
		t.Fatalf("unexpected %q event for %s (status %q)", ev.event, ev.task.ID, ev.task.LastStatus)
	case <-time.After(200 * time.Millisecond):
	}
}

// waitFor polls until cond holds, so a test never depends on how fast the scheduler goroutine runs.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mustTask(t *testing.T, store Store, id string) session.ScheduledTask {
	t.Helper()
	row, err := store.GetSchedule(context.Background(), id)
	if err != nil {
		t.Fatalf("get schedule: %v", err)
	}
	return row
}

var testBase = time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)

func shellSpec(name, command string, trigger Trigger) Spec {
	return Spec{Name: name, Kind: KindShell, Shell: &ShellPayload{Command: command}, Trigger: trigger}
}

// A once task fires exactly once: its occurrence is taken, next_run_at becomes NULL and no later
// advance of the clock fires it again.
func TestOnceFiresExactlyOnce(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	task, err := s.Create(ctx, shellSpec("nightly", "make test", Trigger{Kind: TriggerOnce, At: testBase.Add(time.Minute)}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.NextRunAt == nil || !task.NextRunAt.Equal(testBase.Add(time.Minute)) {
		t.Fatalf("next run = %v, want %v", task.NextRunAt, testBase.Add(time.Minute))
	}
	noEvent(t, actions.events)

	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")

	row := mustTask(t, store, task.ID)
	if row.NextRunAt != nil {
		t.Fatalf("a fired once task still has next_run_at %v", row.NextRunAt)
	}
	if row.FireCount != 1 || row.LastStatus != session.ScheduleStatusFired || row.LastRef != "ref-1" {
		t.Fatalf("row after firing: count=%d status=%q ref=%q", row.FireCount, row.LastStatus, row.LastRef)
	}
	if row.LastFiredAt == nil || !row.LastFiredAt.Equal(testBase.Add(time.Minute)) {
		t.Fatalf("last fired at %v", row.LastFiredAt)
	}
	if shells, _ := actions.count(); shells != 1 {
		t.Fatalf("shell ran %d times", shells)
	}

	clock.Advance(24 * time.Hour)
	s.Wake()
	noEvent(t, actions.events)
	if shells, _ := actions.count(); shells != 1 {
		t.Fatalf("shell ran %d times after the one occurrence", shells)
	}
}

// An interval task fires once per slot, its next time is the previous slot plus the interval, and a
// long absence advances past the missed slots without a burst.
func TestIntervalFiresWithoutCatchUpBurst(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}

	task, err := s.Create(ctx, shellSpec("every minute", "date", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if task.NextRunAt == nil || !task.NextRunAt.Equal(testBase.Add(time.Minute)) {
		t.Fatalf("next run = %v, want %v", task.NextRunAt, testBase.Add(time.Minute))
	}

	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	row := mustTask(t, store, task.ID)
	if row.NextRunAt == nil || !row.NextRunAt.Equal(testBase.Add(2*time.Minute)) {
		t.Fatalf("next run after first firing = %v, want %v", row.NextRunAt, testBase.Add(2*time.Minute))
	}

	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	row = mustTask(t, store, task.ID)
	if row.NextRunAt == nil || !row.NextRunAt.Equal(testBase.Add(3*time.Minute)) {
		t.Fatalf("next run after second firing = %v, want %v", row.NextRunAt, testBase.Add(3*time.Minute))
	}
	if row.FireCount != 2 {
		t.Fatalf("fire count = %d, want 2", row.FireCount)
	}

	// Ten slots pass with no scheduler tick: one firing, and the next slot is the first one after now.
	clock.Advance(10 * time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	noEvent(t, actions.events)
	row = mustTask(t, store, task.ID)
	// The first slot strictly after now: base+12m plus the remainder of the current interval.
	if row.NextRunAt == nil || !row.NextRunAt.Equal(testBase.Add(13*time.Minute)) {
		t.Fatalf("next run after the gap = %v, want %v", row.NextRunAt, testBase.Add(13*time.Minute))
	}
	if shells, _ := actions.count(); shells != 3 {
		t.Fatalf("shell ran %d times, want 3", shells)
	}
}

// Disabling stops the firings and clears next_run_at; enabling recomputes it from the current time.
func TestDisableStopsAndEnableRecomputes(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("toggle", "date", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	disabled := false
	if _, err := s.Update(ctx, task.ID, Patch{Enabled: &disabled}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	row := mustTask(t, store, task.ID)
	if row.Enabled || row.NextRunAt != nil {
		t.Fatalf("disabled row: enabled=%v next=%v", row.Enabled, row.NextRunAt)
	}
	clock.Advance(5 * time.Minute)
	s.Wake()
	noEvent(t, actions.events)

	clock.Advance(10 * time.Minute) // now = base + 15m
	enabled := true
	updated, err := s.Update(ctx, task.ID, Patch{Enabled: &enabled})
	if err != nil {
		t.Fatalf("enable: %v", err)
	}
	if updated.NextRunAt == nil || !updated.NextRunAt.Equal(testBase.Add(16*time.Minute)) {
		t.Fatalf("next run after enabling = %v, want %v", updated.NextRunAt, testBase.Add(16*time.Minute))
	}
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	if shells, _ := actions.count(); shells != 1 {
		t.Fatalf("shell ran %d times, want 1", shells)
	}
}

// Deleting a task stops it: nothing fires afterwards and the row is gone.
func TestDeleteStopsFiring(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("gone", "date", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Delete(ctx, task.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, task.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("get after delete: %v", err)
	}
	clock.Advance(time.Hour)
	s.Wake()
	noEvent(t, actions.events)
	if shells, _ := actions.count(); shells != 0 {
		t.Fatalf("shell ran %d times after delete", shells)
	}
}

// A task that came due while no scheduler ran follows its missed policy at the next start: skip
// advances without firing, run_once fires exactly once, both without a catch-up burst.
func TestMissedPolicyAcrossRestart(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		trigger Trigger
		policy  MissedPolicy
		// wantFires is how many firings the restart performs.
		wantFires int
		// wantEnabled and wantNext are the row after the restart (next is relative to now).
		wantEnabled bool
		wantNext    *time.Duration
	}{
		{
			name:        "interval skip",
			trigger:     Trigger{Kind: TriggerInterval, Every: time.Minute},
			policy:      MissedSkip,
			wantFires:   0,
			wantEnabled: true,
			wantNext:    durationPtr(6 * time.Minute),
		},
		{
			name:        "interval run_once",
			trigger:     Trigger{Kind: TriggerInterval, Every: time.Minute},
			policy:      MissedRunOnce,
			wantFires:   1,
			wantEnabled: true,
			wantNext:    durationPtr(6 * time.Minute),
		},
		{
			name:        "once skip",
			trigger:     Trigger{Kind: TriggerOnce, At: testBase.Add(time.Minute)},
			policy:      MissedSkip,
			wantFires:   0,
			wantEnabled: false,
			wantNext:    nil,
		},
		{
			name:        "once run_once",
			trigger:     Trigger{Kind: TriggerOnce, At: testBase.Add(time.Minute)},
			policy:      MissedRunOnce,
			wantFires:   1,
			wantEnabled: true,
			wantNext:    nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".lumencortex")
			clock := newFakeClock(testBase)
			store := openScheduleStore(t, dir)
			actions := newFakeActions()
			first := newTestScheduler(t, store, actions, clock, nil)
			task, err := first.Create(ctx, Spec{
				Name: "missed", Kind: KindShell, Shell: &ShellPayload{Command: "make"},
				Trigger: tc.trigger, MissedPolicy: tc.policy,
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			// The process stops before the task comes due; five minutes pass with nothing running.
			if err := first.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatalf("close store: %v", err)
			}
			clock.Advance(5 * time.Minute)

			store = openScheduleStore(t, dir)
			defer store.Close()
			second := newTestScheduler(t, store, actions, clock, nil)
			if err := second.Start(ctx); err != nil {
				t.Fatalf("start after restart: %v", err)
			}
			if tc.wantFires == 1 {
				waitEvent(t, actions.events, "fired")
				noEvent(t, actions.events)
			} else {
				// The skipped occurrence is announced too, without a firing.
				waitEvent(t, actions.events, "missed")
				noEvent(t, actions.events)
			}
			if shells, _ := actions.count(); shells != tc.wantFires {
				t.Fatalf("shell ran %d times, want %d", shells, tc.wantFires)
			}
			row := mustTask(t, store, task.ID)
			if row.Enabled != tc.wantEnabled {
				t.Fatalf("enabled = %v, want %v", row.Enabled, tc.wantEnabled)
			}
			if tc.wantNext == nil {
				if row.NextRunAt != nil {
					t.Fatalf("next run = %v, want none", row.NextRunAt)
				}
			} else {
				want := testBase.Add(*tc.wantNext)
				if row.NextRunAt == nil || !row.NextRunAt.Equal(want) {
					t.Fatalf("next run = %v, want %v", row.NextRunAt, want)
				}
			}
			if tc.wantFires == 0 {
				if row.LastStatus != session.ScheduleStatusMissed {
					t.Fatalf("last status = %q, want %q", row.LastStatus, session.ScheduleStatusMissed)
				}
				if row.FireCount != 0 {
					t.Fatalf("fire count = %d, want 0", row.FireCount)
				}
			} else if row.FireCount != 1 || row.LastStatus != session.ScheduleStatusFired {
				t.Fatalf("fire count = %d status = %q, want 1 fired", row.FireCount, row.LastStatus)
			}
		})
	}
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// While the previous firing's job or run is still active, the next occurrence is skipped, never run
// concurrently, and the skip is recorded.
func TestOverlapIsSkipped(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("slow", "sleep 3600", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")

	actions.setActive("ref-1", true)
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	row := mustTask(t, store, task.ID)
	if row.LastStatus != session.ScheduleStatusSkippedOverlap || row.LastRef != "ref-1" {
		t.Fatalf("status = %q ref = %q, want %q ref-1", row.LastStatus, row.LastRef, session.ScheduleStatusSkippedOverlap)
	}
	if shells, _ := actions.count(); shells != 1 {
		t.Fatalf("shell ran %d times, want 1 (the second occurrence was skipped)", shells)
	}
	if row.NextRunAt == nil || !row.NextRunAt.Equal(testBase.Add(3*time.Minute)) {
		t.Fatalf("next run = %v, want %v", row.NextRunAt, testBase.Add(3*time.Minute))
	}

	// The job finished: the following occurrence runs.
	actions.setActive("ref-1", false)
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	if shells, _ := actions.count(); shells != 2 {
		t.Fatalf("shell ran %d times, want 2", shells)
	}
}

// Two schedulers on one database (two store handles, as two processes would be) fire one occurrence
// exactly once: the compare-and-set claim decides.
func TestTwoSchedulersFireOneOccurrence(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".lumencortex")
	clock := newFakeClock(testBase)
	storeA := openScheduleStore(t, dir)
	defer storeA.Close()
	storeB := openScheduleStore(t, dir)
	defer storeB.Close()

	actions := newFakeActions()
	a := newTestScheduler(t, storeA, actions, clock, nil)
	b := newTestScheduler(t, storeB, actions, clock, nil)
	if err := a.Start(ctx); err != nil {
		t.Fatalf("start a: %v", err)
	}
	if err := b.Start(ctx); err != nil {
		t.Fatalf("start b: %v", err)
	}
	task, err := a.Create(ctx, shellSpec("shared", "make", Trigger{Kind: TriggerOnce, At: testBase.Add(time.Minute)}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	clock.Advance(time.Minute)
	a.Wake()
	b.Wake()
	waitEvent(t, actions.events, "fired")
	noEvent(t, actions.events)
	if shells, _ := actions.count(); shells != 1 {
		t.Fatalf("shell ran %d times, want 1", shells)
	}
	row := mustTask(t, storeA, task.ID)
	if row.FireCount != 1 {
		t.Fatalf("fire count = %d, want 1", row.FireCount)
	}
}

// A failing action is recorded with its error and does not stop the schedule.
func TestActionFailureIsRecorded(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	actions.runErr = errors.New("queue is full")
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("broken", "make", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clock.Advance(time.Minute)
	s.Wake()
	ev := waitEvent(t, actions.events, "fired")
	if ev.task.LastStatus != session.ScheduleStatusFailed {
		t.Fatalf("event status = %q", ev.task.LastStatus)
	}
	row := mustTask(t, store, task.ID)
	if row.LastStatus != session.ScheduleStatusFailed || row.LastError != "queue is full" {
		t.Fatalf("row: status=%q error=%q", row.LastStatus, row.LastError)
	}
	if row.NextRunAt == nil || !row.NextRunAt.Equal(testBase.Add(2*time.Minute)) {
		t.Fatalf("next run = %v", row.NextRunAt)
	}
}

// Without an agent runner the task stays due: the claim is given back and nothing is marked fired.
func TestNoRunnerKeepsTaskDue(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	if err := store.Create(ctx, session.Session{ID: "session_agent", Goal: "check the build"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	actions := newFakeActions()
	actions.noRunner = true
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, Spec{
		Name: "agent check", Kind: KindAgent, SessionID: "session_agent",
		Agent:   &AgentPayload{Prompt: "check the build", Policy: PolicyReadOnly},
		Trigger: Trigger{Kind: TriggerOnce, At: testBase.Add(time.Minute)},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clock.Advance(time.Minute)
	s.Wake()
	// The unclaimed row already matches the given-back one, so first wait for the attempt; after it the
	// row is either still claimed (fire_count 1) or given back.
	waitFor(t, "the agent attempt", func() bool {
		actions.mu.Lock()
		defer actions.mu.Unlock()
		return len(actions.agents) == 1
	})
	waitFor(t, "the claim to be given back", func() bool {
		row := mustTask(t, store, task.ID)
		return row.NextRunAt != nil && row.FireCount == 0 && row.LastStatus == ""
	})
	row := mustTask(t, store, task.ID)
	if row.LastFiredAt != nil {
		t.Fatalf("last fired at = %v, want none", row.LastFiredAt)
	}
	noEvent(t, actions.events)

	// A runner registers: the task fires on the next Wake.
	actions.mu.Lock()
	actions.noRunner = false
	actions.mu.Unlock()
	s.Wake()
	waitEvent(t, actions.events, "fired")
	row = mustTask(t, store, task.ID)
	if row.FireCount != 1 || row.LastStatus != session.ScheduleStatusFired || row.LastRef != "ref-1" {
		t.Fatalf("row after the retry: count=%d status=%q ref=%q", row.FireCount, row.LastStatus, row.LastRef)
	}
}

// A one-shot task whose session is busy when it comes due (ErrBusy: the run that scheduled a wake-up
// still holds the session) is not consumed: it stays pending, is tried again busyRetry later and fires
// once the session is free. A recurring task records the skip and waits for its next slot (RT-state-2).
func TestBusyOnceTaskFiresWhenTheSessionIsFree(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	if err := store.Create(ctx, session.Session{ID: "session_agent", Goal: "wait for the build"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	actions := newFakeActions()
	actions.runErr = ErrBusy
	s := newTestScheduler(t, store, actions, clock, nil)
	agentSpec := func(name string, trigger Trigger) Spec {
		return Spec{Name: name, Kind: KindAgent, SessionID: "session_agent",
			Agent: &AgentPayload{Prompt: "resume", Policy: PolicyReadOnly}, Trigger: trigger}
	}
	once, err := s.Create(ctx, agentSpec("wake-up", Trigger{Kind: TriggerOnce, At: testBase.Add(time.Minute)}))
	if err != nil {
		t.Fatalf("create once: %v", err)
	}
	every, err := s.Create(ctx, agentSpec("every minute", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create interval: %v", err)
	}
	pendingAt := func(want time.Time) {
		t.Helper()
		row := mustTask(t, store, once.ID)
		if !row.Enabled || row.NextRunAt == nil || !row.NextRunAt.Equal(want) {
			t.Fatalf("busy one-shot: enabled=%v next=%v status=%q, want pending at %v", row.Enabled, row.NextRunAt, row.LastStatus, want)
		}
		if row.FireCount != 0 || row.LastFiredAt != nil || row.LastStatus != "" {
			t.Fatalf("busy one-shot recorded a firing: count=%d fired=%v status=%q", row.FireCount, row.LastFiredAt, row.LastStatus)
		}
	}

	clock.Advance(time.Minute)
	s.tick(ctx)
	pendingAt(clock.Now().Add(busyRetry))
	rec := mustTask(t, store, every.ID)
	if rec.LastStatus != session.ScheduleStatusSkippedOverlap || rec.NextRunAt == nil || !rec.NextRunAt.Equal(testBase.Add(2*time.Minute)) {
		t.Fatalf("busy interval task: status=%q next=%v, want %q and its next slot", rec.LastStatus, rec.NextRunAt, session.ScheduleStatusSkippedOverlap)
	}

	// Still busy at the retry: deferred again, not before the retry comes due.
	s.tick(ctx)
	pendingAt(clock.Now().Add(busyRetry))
	clock.Advance(busyRetry)
	s.tick(ctx)
	pendingAt(clock.Now().Add(busyRetry))

	// The session is free: the wake-up fires once.
	actions.mu.Lock()
	actions.runErr = nil
	actions.mu.Unlock()
	clock.Advance(busyRetry)
	s.tick(ctx)
	row := mustTask(t, store, once.ID)
	if row.FireCount != 1 || row.LastStatus != session.ScheduleStatusFired || row.LastRef != "ref-1" || row.NextRunAt != nil {
		t.Fatalf("one-shot after the session freed: count=%d status=%q ref=%q next=%v", row.FireCount, row.LastStatus, row.LastRef, row.NextRunAt)
	}
	clock.Advance(busyRetry)
	s.tick(ctx)
	if row := mustTask(t, store, once.ID); row.FireCount != 1 {
		t.Fatalf("one-shot fired %d times, want 1", row.FireCount)
	}
}

// Close stops the goroutine and the timer: nothing fires after it returns, and a second Close is a
// no-op.
func TestCloseStopsTheLoop(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := s.Create(ctx, shellSpec("stop me", "make", Trigger{Kind: TriggerInterval, Every: time.Minute})); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-s.done:
	default:
		t.Fatal("the scheduler goroutine is still running after Close")
	}
	clock.Advance(time.Hour)
	noEvent(t, actions.events)
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := s.Start(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after close: %v, want ErrClosed", err)
	}
}

// ObserveRef records the terminal status of the firing, and a stale ref never overwrites a newer one.
func TestObserveRefRecordsTerminalStatus(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("observed", "make", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")

	if err := s.ObserveRef(ctx, task.ID, "ref-1", "completed"); err != nil {
		t.Fatalf("observe: %v", err)
	}
	row := mustTask(t, store, task.ID)
	if row.LastStatus != "completed" {
		t.Fatalf("status = %q, want completed", row.LastStatus)
	}
	if err := s.ObserveRef(ctx, task.ID, "ref-0", "failed"); err != nil {
		t.Fatalf("stale observe: %v", err)
	}
	if row = mustTask(t, store, task.ID); row.LastStatus != "completed" {
		t.Fatalf("a stale observation overwrote the status: %q", row.LastStatus)
	}
}

// Create refuses a spec the table or the contract cannot hold, before writing anything.
func TestCreateValidates(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	longCommand := make([]byte, MaxCommandBytes+1)
	for i := range longCommand {
		longCommand[i] = 'x'
	}
	cases := map[string]Spec{
		"no name":       {Kind: KindShell, Shell: &ShellPayload{Command: "make"}, Trigger: Trigger{Kind: TriggerInterval, Every: time.Minute}},
		"no command":    shellSpec("n", "", Trigger{Kind: TriggerInterval, Every: time.Minute}),
		"long command":  shellSpec("n", string(longCommand), Trigger{Kind: TriggerInterval, Every: time.Minute}),
		"short period":  shellSpec("n", "make", Trigger{Kind: TriggerInterval, Every: 10 * time.Millisecond}),
		"past once":     shellSpec("n", "make", Trigger{Kind: TriggerOnce, At: testBase.Add(-time.Hour)}),
		"both payloads": {Name: "n", Kind: KindShell, Shell: &ShellPayload{Command: "make"}, Agent: &AgentPayload{Prompt: "x"}, Trigger: Trigger{Kind: TriggerInterval, Every: time.Minute}},
		"agent no session": {
			Name: "n", Kind: KindAgent, Agent: &AgentPayload{Prompt: "x", Policy: PolicyReadOnly},
			Trigger: Trigger{Kind: TriggerInterval, Every: time.Minute},
		},
		"bad policy": {
			Name: "n", Kind: KindAgent, SessionID: "session_agent", Agent: &AgentPayload{Prompt: "x", Policy: "root"},
			Trigger: Trigger{Kind: TriggerInterval, Every: time.Minute},
		},
	}
	for name, spec := range cases {
		if _, err := s.Create(ctx, spec); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if list, err := s.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("refused specs wrote rows: %v %v", list, err)
	}
}

// An agent task defaults to the read-only policy and a shell task keeps its session empty.
func TestCreateDefaults(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	if err := store.Create(ctx, session.Session{ID: "session_agent", Goal: "check"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	agent, err := s.Create(ctx, Spec{
		Name: "agent", Kind: KindAgent, SessionID: "session_agent", Agent: &AgentPayload{Prompt: "check"},
		Trigger: Trigger{Kind: TriggerOnce, At: testBase.Add(time.Hour)},
	})
	if err != nil {
		t.Fatalf("create agent task: %v", err)
	}
	if agent.Agent == nil || agent.Agent.Policy != PolicyReadOnly {
		t.Fatalf("policy = %v, want %s", agent.Agent, PolicyReadOnly)
	}
	if agent.MissedPolicy != MissedSkip {
		t.Fatalf("missed policy = %q, want %q", agent.MissedPolicy, MissedSkip)
	}
	row := mustTask(t, store, agent.ID)
	if row.Kind != session.ScheduleKindAgent || row.SessionID != "session_agent" {
		t.Fatalf("row: kind=%q session=%q", row.Kind, row.SessionID)
	}
}

// A shell task has no session, and the store's foreign key refuses an agent task whose session is
// gone: the scheduler reports ErrNotFound, not a raw constraint failure.
func TestCreateAgentTaskNeedsAnExistingSession(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	_, err := s.Create(ctx, Spec{
		Name: "orphan", Kind: KindAgent, SessionID: "session_missing", Agent: &AgentPayload{Prompt: "x", Policy: PolicyReadOnly},
		Trigger: Trigger{Kind: TriggerOnce, At: testBase.Add(time.Hour)},
	})
	if !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A cron task fires at the match its expression names in its zone, and its next slot is the following
// match; the expression survives the round trip through the store.
func TestCronTaskFiresAtItsZoneTime(t *testing.T) {
	ctx := context.Background()
	// 09:00 UTC is 10:00 in Berlin (CET, +01:00) on 1 November 2026.
	clock := newFakeClock(time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC))
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, Spec{
		Name: "noon in Berlin", Kind: KindShell, Shell: &ShellPayload{Command: "make"},
		Trigger: Trigger{Kind: TriggerCron, Cron: "0 12 * * *", Timezone: "Europe/Berlin"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	want := time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC) // 12:00 CET
	if task.NextRunAt == nil || !task.NextRunAt.Equal(want) {
		t.Fatalf("next run = %v, want %s", task.NextRunAt, want.Format(time.RFC3339))
	}
	noEvent(t, actions.events)

	clock.Advance(2 * time.Hour)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	row := mustTask(t, store, task.ID)
	if row.Cron != "0 12 * * *" || row.Timezone != "Europe/Berlin" {
		t.Fatalf("row: cron=%q timezone=%q", row.Cron, row.Timezone)
	}
	if row.NextRunAt == nil || !row.NextRunAt.Equal(want.Add(24*time.Hour)) {
		t.Fatalf("next run = %v, want %s", row.NextRunAt, want.Add(24*time.Hour).Format(time.RFC3339))
	}
	if row.FireCount != 1 {
		t.Fatalf("fire count = %d, want 1", row.FireCount)
	}
}

// A cron task that came due while no scheduler ran follows its missed policy across a restart.
func TestCronMissedPolicyAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), ".lumencortex")
	clock := newFakeClock(time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC))
	store := openScheduleStore(t, dir)
	actions := newFakeActions()
	first := newTestScheduler(t, store, actions, clock, nil)
	task, err := first.Create(ctx, Spec{
		Name: "hourly", Kind: KindShell, Shell: &ShellPayload{Command: "make"},
		Trigger: Trigger{Kind: TriggerCron, Cron: "0 * * * *", Timezone: "UTC"}, MissedPolicy: MissedSkip,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	clock.Advance(5 * time.Hour) // 13:00: five hourly slots passed unobserved

	store = openScheduleStore(t, dir)
	defer store.Close()
	second := newTestScheduler(t, store, actions, clock, nil)
	if err := second.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitEvent(t, actions.events, "missed")
	noEvent(t, actions.events)
	row := mustTask(t, store, task.ID)
	if row.LastStatus != session.ScheduleStatusMissed || row.FireCount != 0 {
		t.Fatalf("row after the restart: status=%q count=%d", row.LastStatus, row.FireCount)
	}
	if want := time.Date(2026, 11, 1, 14, 0, 0, 0, time.UTC); row.NextRunAt == nil || !row.NextRunAt.Equal(want) {
		t.Fatalf("next run = %v, want %s", row.NextRunAt, want.Format(time.RFC3339))
	}
}

// A bad cron expression, an unknown zone and an expression that never matches are refused at create.
func TestCreateRefusesBadCron(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	for name, trigger := range map[string]Trigger{
		"four fields":   {Kind: TriggerCron, Cron: "0 12 * *"},
		"out of range":  {Kind: TriggerCron, Cron: "0 25 * * *"},
		"never matches": {Kind: TriggerCron, Cron: "0 0 30 2 *"},
		"unknown zone":  {Kind: TriggerCron, Cron: "0 12 * * *", Timezone: "Mars/Olympus"},
	} {
		if _, err := s.Create(ctx, shellSpec("bad cron", "make", trigger)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// A stored trigger that yields no further slot (a corrupted expression, written past the API) is
// disabled with its reason on the first tick, instead of staying due and failing on every tick.
func TestUnschedulableTaskIsDisabled(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	dir := filepath.Join(t.TempDir(), ".lumencortex")
	store := openScheduleStore(t, dir)
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("hourly", "make", Trigger{Kind: TriggerCron, Cron: "0 * * * *"}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Corrupt the row behind the scheduler's back, as a hand edit of the database would.
	db, err := sql.Open("sqlite3", store.Path())
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `UPDATE scheduled_tasks SET cron_expr = 'not a cron expression' WHERE schedule_id = ?`, task.ID); err != nil {
		t.Fatalf("corrupt the row: %v", err)
	}

	clock.Advance(time.Hour)
	s.Wake()
	ev := waitEvent(t, actions.events, "fired")
	if ev.task.LastStatus != session.ScheduleStatusFailed || ev.task.Enabled {
		t.Fatalf("event = %+v", ev.task)
	}
	row := mustTask(t, store, task.ID)
	if row.Enabled || row.NextRunAt != nil {
		t.Fatalf("row after the failure: enabled=%v next=%v", row.Enabled, row.NextRunAt)
	}
	if row.LastStatus != session.ScheduleStatusFailed || row.LastError == "" {
		t.Fatalf("row after the failure: status=%q error=%q", row.LastStatus, row.LastError)
	}
	if shells, _ := actions.count(); shells != 0 {
		t.Fatalf("an unschedulable task ran %d times", shells)
	}
	// It stays quiet: no further event on later ticks.
	clock.Advance(time.Hour)
	s.Wake()
	noEvent(t, actions.events)
}

// A task whose session is deleted disappears with it (the schema cascades), and the scheduler does not
// fire or fail on the row it can no longer see.
func TestVanishedTaskIsTolerated(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	if err := store.Create(ctx, session.Session{ID: "session_gone", Goal: "temporary"}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, Spec{
		Name: "agent check", Kind: KindAgent, SessionID: "session_gone",
		Agent:   &AgentPayload{Prompt: "check", Policy: PolicyReadOnly},
		Trigger: Trigger{Kind: TriggerInterval, Every: time.Minute},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// The session is deleted out from under the scheduler, as a privacy deletion in another process
	// would: its task goes with it through the schema's cascade.
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(store.Path())+"?_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		t.Fatalf("open the database: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `DELETE FROM sessions WHERE id = 'session_gone'`); err != nil {
		t.Fatalf("delete session: %v", err)
	}
	if _, err := store.GetSchedule(ctx, task.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("the task survived its session: %v", err)
	}
	clock.Advance(2 * time.Minute)
	s.Wake()
	noEvent(t, actions.events)
	if _, agents := actions.count(); agents != 0 {
		t.Fatalf("an agent run started for a deleted session (%d times)", agents)
	}
	// The scheduler is still healthy afterwards: a new task fires normally.
	if _, err := s.Create(ctx, shellSpec("after", "make", Trigger{Kind: TriggerOnce, At: clock.Now().Add(time.Minute)})); err != nil {
		t.Fatalf("create after the deletion: %v", err)
	}
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
}

// A task deleted between the claim and the result write is not an error: the firing already happened,
// and there is no row left to record it on.
func TestTaskDeletedDuringFiringIsTolerated(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := &deletingActions{store: store, events: make(chan firedEvent, 8)}
	s, err := New(Options{Store: store, Actions: actions, Now: clock.Now, MinInterval: time.Second, Notify: actions.notify})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer s.Close()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("self-deleting", "make", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	actions.id = task.ID
	clock.Advance(time.Minute)
	s.Wake()
	waitFor(t, "the task to be deleted", func() bool {
		_, err := store.GetSchedule(ctx, task.ID)
		return errors.Is(err, session.ErrNotFound)
	})
	noEvent(t, actions.events)
	// The loop survives a result it could not record.
	if _, err := s.Create(ctx, shellSpec("next", "make", Trigger{Kind: TriggerOnce, At: clock.Now().Add(time.Minute)})); err != nil {
		t.Fatalf("create after the deletion: %v", err)
	}
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
}

// deletingActions removes the task's row when it fires, as another process deleting the session would.
type deletingActions struct {
	store  Store
	id     string
	events chan firedEvent
	mu     sync.Mutex
	ran    int
}

func (d *deletingActions) RunShell(ctx context.Context, _ Task) (string, error) {
	d.mu.Lock()
	d.ran++
	d.mu.Unlock()
	if err := d.store.DeleteSchedule(ctx, d.id); err != nil {
		return "", err
	}
	return "ref-deleted", nil
}

func (d *deletingActions) RunAgent(context.Context, Task) (string, error) { return "", ErrNoRunner }

func (d *deletingActions) Active(context.Context, Task, string) (bool, error) { return false, nil }

func (d *deletingActions) notify(event string, t Task) {
	select {
	case d.events <- firedEvent{event: event, task: t}:
	default:
	}
}

// A firing whose action reports that the previous one is still running is recorded as an overlap skip,
// not as a failure: the run lock of another process is not an error of this scheduler.
func TestBusyActionIsSkippedAsOverlap(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock(testBase)
	store := openScheduleStore(t, filepath.Join(t.TempDir(), ".lumencortex"))
	defer store.Close()
	actions := newFakeActions()
	s := newTestScheduler(t, store, actions, clock, nil)
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	task, err := s.Create(ctx, shellSpec("busy", "make", Trigger{Kind: TriggerInterval, Every: time.Minute}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	actions.mu.Lock()
	actions.runErr = ErrBusy
	actions.mu.Unlock()
	clock.Advance(time.Minute)
	s.Wake()
	waitEvent(t, actions.events, "fired")
	row := mustTask(t, store, task.ID)
	if row.LastStatus != session.ScheduleStatusSkippedOverlap {
		t.Fatalf("status = %q, want %q", row.LastStatus, session.ScheduleStatusSkippedOverlap)
	}
	if row.LastError != "" {
		t.Fatalf("a skip recorded an error: %q", row.LastError)
	}
}
