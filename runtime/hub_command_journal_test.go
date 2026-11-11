package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// TestHubCommandIDHoldsForAResultLargerThanTheJournalRow pins idempotency independent of the result's
// size: a command whose agent projection is larger than a journal row is still journalled, and a retry
// of its id returns that record instead of running the verb a second time.
func TestHubCommandIDHoldsForAResultLargerThanTheJournalRow(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	root := newParentSession(t, e, p)
	ctx := context.Background()
	specs := make([]HubTaskSpec, 0, 100)
	for i := range 100 {
		specs = append(specs, HubTaskSpec{ID: fmt.Sprintf("unit-%03d", i), Goal: fmt.Sprintf("HUB:unit %d", i)})
	}
	if err := e.ConfigureHub(ctx, root, specs); err != nil {
		t.Fatal(err)
	}
	// The root holds every unit, so its own projection carries 100 holdings.
	for _, spec := range specs {
		if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: root, Unit: spec.ID, Holder: root, TTL: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	cmd := HubCommand{SessionID: root, CommandID: "start-large", Verb: HubVerbStart, Args: json.RawMessage(`{"input":"go","maxSteps":1}`)}
	first, err := sup.Command(ctx, cmd)
	if err != nil || first.Status != HubAccepted {
		t.Fatalf("first %+v %v", first, err)
	}
	if raw, _ := json.Marshal(first); len(raw) <= session.MaxHubCommandResultBytes {
		t.Fatalf("the fixture's result is %d bytes; it must exceed a journal row (%d)", len(raw), session.MaxHubCommandResultBytes)
	}
	p.waitRequest(t)
	p.release()
	waitInactive(t, sup, root)
	before := p.count()
	again, err := sup.Command(ctx, cmd)
	waitInactive(t, sup, root)
	if err != nil || again.Status != HubDuplicate {
		t.Errorf("retry of the command id: %+v %v", again.Status, err)
	}
	if p.count() != before {
		t.Errorf("a retried command id ran the verb again: %d -> %d model requests", before, p.count())
	}
}

// TestHubCommandIDInFlightIsNotRunAgain pins the journal under concurrency: while the first execution of a
// command id has no recorded outcome, every concurrent retry of the id is answered pending without
// running, and once the outcome is recorded a retry is a duplicate.
func TestHubCommandIDInFlightIsNotRunAgain(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	cmd := HubCommand{SessionID: id, CommandID: "start-once", Verb: HubVerbStart, Args: json.RawMessage(`{"input":"go","maxSteps":1}`)}
	const retries = 4
	results := make([]HubCommandResult, retries)
	errs := make([]error, retries)
	testHookHubCommandApplied = func(HubCommand) {
		var wg sync.WaitGroup
		for i := range retries {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i], errs[i] = sup.Command(ctx, cmd)
			}()
		}
		wg.Wait()
	}
	first, err := sup.Command(ctx, cmd)
	testHookHubCommandApplied = nil
	if err != nil || first.Status != HubAccepted {
		t.Fatalf("first %+v %v", first, err)
	}
	for i := range retries {
		if !errors.Is(errs[i], ErrHubCommandPending) || results[i].Status != HubPending {
			t.Errorf("retry %d while the first execution is unrecorded: %+v %v", i, results[i], errs[i])
		}
	}
	p.waitRequest(t)
	p.release()
	waitInactive(t, sup, id)
	again, err := sup.Command(ctx, cmd)
	waitInactive(t, sup, id)
	if err != nil || again.Status != HubDuplicate || again.Agent.RunID != first.Agent.RunID {
		t.Errorf("retry after the outcome was recorded: %+v %v", again, err)
	}
	if p.count() != 1 {
		t.Errorf("the command id ran %d model requests, want the first run's 1", p.count())
	}
}

// TestHubCommandWithoutARecordedOutcomeIsNotRunAfterRestart pins the crash boundary: a command id a dead
// process journalled without an outcome stays pending after a restart, and its retry runs nothing.
func TestHubCommandWithoutARecordedOutcomeIsNotRunAfterRestart(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	dir := t.TempDir()
	e, err := Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	p := &handoffProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	cmd := HubCommand{SessionID: id, CommandID: "start-before-crash", Verb: HubVerbStart, Args: json.RawMessage(`{"input":"go","maxSteps":1}`)}
	// What Command journals before it runs a verb, left behind by a process that ended before the outcome.
	provisional, err := json.Marshal(HubCommandResult{Status: HubPending, Verb: cmd.Verb, CommandID: cmd.CommandID})
	if err != nil {
		t.Fatal(err)
	}
	if _, reserved, err := e.store.ReserveHubCommand(ctx, session.HubCommandRecord{CommandID: cmd.CommandID, SessionID: id, Verb: cmd.Verb, Result: provisional}); err != nil || !reserved {
		t.Fatalf("reserve %v %v", reserved, err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	e, err = Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	result, err := sup.Command(ctx, cmd)
	if !errors.Is(err, ErrHubCommandPending) || result.Status != HubPending {
		t.Fatalf("retry after restart: %+v %v", result, err)
	}
	if sup.Active(id) || p.count() != 0 {
		t.Fatalf("a command without a recorded outcome ran again (active=%v, requests=%d)", sup.Active(id), p.count())
	}
}

// TestHubCommandOutcomeThatCannotBeJournalledIsReported pins that a lost outcome is never silent: the
// command returns its result together with ErrHubCommandUnrecorded.
func TestHubCommandOutcomeThatCannotBeJournalledIsReported(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	id := newParentSession(t, e, &finalProvider{})
	ctx := context.Background()
	// The session goes away between the verb and its record, taking the reservation with it.
	testHookHubCommandApplied = func(HubCommand) {
		if err := e.store.Delete(ctx, id); err != nil {
			t.Error(err)
		}
	}
	defer func() { testHookHubCommandApplied = nil }()
	result, err := sup.Command(ctx, HubCommand{SessionID: id, CommandID: "attach-unrecorded", Verb: HubVerbAttach})
	if !errors.Is(err, ErrHubCommandUnrecorded) || result.Status != HubAccepted {
		t.Fatalf("an outcome that could not be journalled: %+v %v", result, err)
	}
}
