package runtime

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// TestXReviewHubAckedMessageIsNotReplayedByAHandedOffRun pins the delivery bookkeeping across a handoff
// of the same session under another agent: a message the recipient read with worker_inbox is acked, so
// the run the handoff starts does not inject it a second time and does not spend another attempt.
func TestXReviewHubAckedMessageIsNotReplayedByAHandedOffRun(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &inboxReadingProvider{}
	root, worker := workerAndRoot(t, e, p)
	ctx := context.Background()
	if _, err := e.SendWorkerMessage(ctx, root, root, worker, "m1", "ACKED-BEFORE-HANDOFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx, worker, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, worker)
	page, err := e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false)
	if err != nil || len(page.Messages) != 1 {
		t.Fatalf("delivery state %+v %v", page, err)
	}
	if m := page.Messages[0]; m.Delivery != session.WorkerMessageAcked || m.Attempts != 1 {
		t.Fatalf("the inbox read did not ack the pushed message: %+v", m)
	}
	before := len(p.requests())
	if _, err := sup.Handoff(ctx, HubHandoff{Source: worker, Note: "continue after the read"}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, worker)
	requests := p.requests()
	if len(requests) <= before {
		t.Fatalf("the handoff started no run (%d requests)", len(requests))
	}
	// The handed-off run reads the message through the transcript it inherits (the pushed aside and the
	// inbox result), so its first request carries exactly the copies the last request of the pre-handoff
	// run carried; a redelivery would add another one.
	if prior, got := strings.Count(rendered(requests[before-1]), "ACKED-BEFORE-HANDOFF"), strings.Count(rendered(requests[before]), "ACKED-BEFORE-HANDOFF"); got != prior {
		t.Fatalf("the handed-off run's first request carries the message %d times, the transcript before it %d", got, prior)
	}
	after, err := e.store.ReadWorkerMessages(ctx, root, worker, 0, 10, false)
	if err != nil || len(after.Messages) != 1 {
		t.Fatalf("after the handoff %+v %v", after, err)
	}
	if m := after.Messages[0]; m.Attempts != 1 || m.Delivery != session.WorkerMessageAcked {
		t.Fatalf("the handoff redelivered an acked message: %+v", m)
	}
}

// TestXReviewHubCloseDuringAHandoffSettlesEverything pins the shutdown interaction: a handoff in flight
// while the Engine closes settles - no deadlock, no leaked goroutine, no lost run.stopped, and the
// source's Task Lease in a documented state after the reopen (held by the source, which the handoff
// could not hand over, or transferred to the target).
func TestXReviewHubCloseDuringAHandoffSettlesEverything(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	dir := t.TempDir()
	baseline := runtime.NumGoroutine()
	e, err := Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	source := newParentSession(t, e, p)
	target := newParentSession(t, e, p)
	ctx := context.Background()
	if err := e.ConfigureHub(ctx, source, []HubTaskSpec{{ID: "u1", Goal: "HUB:unit"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx, source, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	p.waitRequest(t)
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: source, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	// Hold the source's cleanup just before run.stopped: the handoff is inside stopForHandoff and the
	// Engine.Close that follows has to wait for the run to finish stopping.
	held, release := make(chan struct{}), make(chan struct{})
	testHookBeforeRunStopped = func() { close(held); <-release }
	defer func() { testHookBeforeRunStopped = nil }()
	handoffResult := make(chan error, 1)
	go func() {
		_, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target, Note: "close during handoff"})
		handoffResult <- err
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("the source's cleanup never reached its stop hook")
	}
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	select {
	case <-closed:
		t.Fatal("Engine.Close returned while the source run was still stopping")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(20 * time.Second):
		t.Fatal("Engine.Close did not return after the handoff's source stopped")
	}
	var handoffErr error
	select {
	case handoffErr = <-handoffResult:
	case <-time.After(20 * time.Second):
		t.Fatal("the handoff in flight during Close did not return")
	}
	if handoffErr != nil && !errors.Is(handoffErr, ErrRunSupervisorClosed) && !errors.Is(handoffErr, ErrRunHeld) &&
		!strings.Contains(handoffErr.Error(), "closed") {
		t.Fatalf("handoff during Close: %v", handoffErr)
	}
	// No goroutine of the closed Engine is left behind.
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine(); leaked > baseline+2 {
		t.Fatalf("%d goroutines after Close, %d before the Engine opened", leaked, baseline)
	}
	// The run's stop is durable and the claim is still exactly one row, held by the source or the target.
	e, err = Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	events, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: source, Type: "run.stopped", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(events.Events) != 1 {
		t.Fatalf("run.stopped events after the reopen = %d", len(events.Events))
	}
	leases, err := e.store.TaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || (leases[0].Holder != source && leases[0].Holder != target) {
		t.Fatalf("the claim after a handoff interrupted by Close: %+v", leases)
	}
	if handoffErr != nil && leases[0].Holder != source {
		t.Fatalf("a failed handoff (%v) moved the claim to %s", handoffErr, leases[0].Holder)
	}
}

// TestXReviewHubEventReplayCarriesTheLeaseAndHandoffPaths pins item 7: a cursor taken before a claim, a
// handoff and an ack resumes across Close/reopen and returns those events in the order they were
// produced, without a hole.
func TestXReviewHubEventReplayCarriesTheLeaseAndHandoffPaths(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	dir := t.TempDir()
	e, err := Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	p := &finalProvider{}
	root, worker := workerAndRoot(t, e, p)
	ctx := context.Background()
	if err := e.ConfigureHub(ctx, root, []HubTaskSpec{{ID: "u1", Goal: "HUB:unit"}}); err != nil {
		t.Fatal(err)
	}
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: root, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	cursor := page.HighWatermark
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: root, Unit: "u1", Holder: root, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	message, err := e.SendWorkerMessage(ctx, root, root, worker, "m1", "replay this")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.AckWorkerMessages(ctx, root, worker, message.Seq); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Handoff(ctx, HubHandoff{Source: root, Target: worker, Note: "replay the transfer"}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, worker)
	sup.Close()
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}

	e, err = Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	replay, err := e.ResumeRuntimeEvents(ctx, RuntimeEventQuery{SessionID: root, After: cursor, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	position := map[string]int{}
	var order []string
	for i, event := range replay.Events {
		if event.Cursor <= cursor {
			t.Fatalf("replay returned a cursor at or before the resume point: %d <= %d", event.Cursor, cursor)
		}
		if _, seen := position[event.Type]; !seen {
			position[event.Type] = i
			order = append(order, event.Type)
		}
	}
	for _, want := range []string{"task.lease.claimed", "worker.message.sent", "worker.message.acked", "task.lease.transferred", "agent.handoff"} {
		if _, ok := position[want]; !ok {
			t.Fatalf("%s is missing from the replay after the reopen: %v", want, order)
		}
	}
	if !(position["task.lease.claimed"] < position["task.lease.transferred"] &&
		position["task.lease.transferred"] < position["agent.handoff"]) {
		t.Fatalf("replay order: %v", order)
	}
}
