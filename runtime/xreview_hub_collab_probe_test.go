package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/internal/toolset"
)

// This file holds the cross-review probes of the Hub slice: the interactions between the handoff, the
// Task Lease, the shared-workspace write lease, the host command journal and the shutdown paths. A probe
// states the contract it checks; the ones that found a defect keep their regression name.

// TestXReviewHubHandoffReleasesTheStoppedSourcesWriteLease pins that a cross-session handoff leaves no shared
// workspace claim behind: the source stopped writing when it was stopped, so its write lease must not
// refuse the target's own writes for the rest of the lease's lifetime.
func TestXReviewHubHandoffReleasesTheStoppedSourcesWriteLease(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e, root := gitEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
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
	if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "shared.txt", Holder: source}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target, Note: "own the workspace"}); err != nil {
		t.Fatal(err)
	}
	leases, err := e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != target {
		t.Fatalf("task leases after the handoff: %+v (want u1 held by %s)", leases, target)
	}
	// The target does the work from here, so its write of the shared path must not be refused by the
	// stopped source's claim.
	written, err := toolset.New(toolset.Options{Workspace: root, Policy: toolset.PolicyFull,
		SessionID: target, Editing: e.editing})
	if err != nil {
		t.Fatal(err)
	}
	result, execErr := written.Execute(ctx, "write_file", map[string]any{"path": "shared.txt", "content": "handed over\n"}, nil)
	if execErr != nil {
		t.Fatal(execErr)
	}
	if !result.OK {
		t.Fatalf("the handoff target cannot write a path the stopped source still leases: %s\nwrite leases: %+v", result.Content, leaseList(t, e))
	}
	sup.Cancel(target)
	waitInactive(t, sup, source, target)
}

func leaseList(t *testing.T, e *Engine) []WriteLease {
	t.Helper()
	leases, err := e.WriteLeases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return leases
}

// TestXReviewHubHandoffRefusedAfterTheStopKeepsTheClaimsWithTheSource pins the documented policy of a
// handoff that is refused only once the source has already stopped (the target started between the
// pre-check and the stop): the source is stopped, its Task Leases stay with it, no success event is
// published, and nothing claims that the work moved.
func TestXReviewHubHandoffRefusedAfterTheStopKeepsTheClaimsWithTheSource(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	source := newParentSession(t, e, p)
	target := newParentSession(t, e, p)
	third := newParentSession(t, e, p)
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
	// Hold the source's cleanup at its handoff lease decision: the target is started inside that window,
	// i.e. after the pre-check and before the post-stop re-check.
	entered, release := make(chan struct{}), make(chan struct{})
	testHookAfterHandoffLeaseDecision = func() { close(entered); <-release }
	defer func() { testHookAfterHandoffLeaseDecision = nil }()
	result := make(chan error, 1)
	go func() {
		_, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the source's cleanup never reached its handoff lease decision")
	}
	if _, err := sup.Start(ctx, target, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	p.waitCount(t, 2)
	close(release)
	select {
	case err := <-result:
		if !errors.Is(err, ErrRunHeld) {
			t.Fatalf("handoff refused after the stop: %v (want ErrRunHeld)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refused handoff did not return")
	}
	waitInactive(t, sup, source)
	// The documented policy: once the source was stopped, a later refusal keeps its claims with it.
	leases, err := e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != source {
		t.Fatalf("claims after a handoff refused post-stop: %+v (documented: they stay with the source)", leases)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: third, TTL: time.Minute}); !errors.Is(err, ErrHubLeaseHeld) {
		t.Fatalf("a third worker claimed the stopped source's unit: %v", err)
	}
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: source, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range page.Events {
		if event.Type == "task.lease.transferred" || event.Type == "agent.handoff" {
			t.Fatalf("a refused handoff published %s", event.Type)
		}
	}
	sup.Cancel(target)
	waitInactive(t, sup, target)
}

// TestXReviewHubExpiredLeaseFormerOwnerIsNotTheOwner pins both lease kinds after expiry: once another
// worker claims the unit, the former owner cannot renew, release or claim as the owner, and its writes
// are refused; the same holds for the shared-workspace write lease.
func TestXReviewHubExpiredLeaseFormerOwnerIsNotTheOwner(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	ctx := context.Background()
	p := &hubScriptProvider{}
	root := newParentSession(t, e, p)
	former := newParentSession(t, e, p)
	next := newParentSession(t, e, p)
	if err := e.ConfigureHub(ctx, root, []HubTaskSpec{{ID: "u1", Goal: "HUB:unit"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: root, Unit: "u1", Holder: former, TTL: 50 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: root, Unit: "u1", Holder: next, TTL: time.Minute}); err != nil {
		t.Fatalf("the next worker could not claim an expired unit: %v", err)
	}
	// The expired former owner keeps none: renewal finds nothing, release finds nothing, a claim is refused.
	renewed, err := e.RenewHubTaskLeases(ctx, former)
	if err != nil || len(renewed) != 0 {
		t.Fatalf("the expired former owner renewed %+v (%v)", renewed, err)
	}
	if err := e.ReleaseHubTaskLease(ctx, root, "u1", former); !errors.Is(err, ErrHubLeaseNotHeld) {
		t.Fatalf("the expired former owner released the new owner's lease: %v", err)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: root, Unit: "u1", Holder: former, TTL: time.Minute}); !errors.Is(err, ErrHubLeaseHeld) {
		t.Fatalf("the expired former owner claimed a live unit: %v", err)
	}
	held, err := e.store.TaskLeasesHeldBy(ctx, next)
	if err != nil || len(held) != 1 || held[0].Holder != next {
		t.Fatalf("the unit's owner after recovery: %+v %v", held, err)
	}

	// The shared-workspace write lease follows the same rule.
	if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "shared.txt", Holder: former, TTLSeconds: 1}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := e.AcquireWriteLease(ctx, WriteLeaseRequest{Path: "shared.txt", Holder: next, TTLSeconds: 60}); err != nil {
		t.Fatalf("the next holder could not take an expired write lease: %v", err)
	}
	if _, err := e.RenewWriteLease(ctx, WriteLeaseRequest{Path: "shared.txt", Holder: former}); err == nil {
		t.Fatal("the expired former holder renewed another holder's write lease")
	}
	if err := e.ReleaseWriteLease(ctx, WriteLeaseRequest{Path: "shared.txt", Holder: former}); err != nil {
		t.Fatal(err)
	}
	remaining := leaseList(t, e)
	if len(remaining) != 1 || remaining[0].Holder != next {
		t.Fatalf("the former holder's release removed the new holder's lease: %+v", remaining)
	}
}

// TestXReviewHubDuplicateCommandIDFromConcurrentCallersRunsTheVerbOnce pins the journal under a real
// race: concurrent callers of one command id, none of which is pending yet, produce exactly one
// execution of the verb.
func TestXReviewHubDuplicateCommandIDFromConcurrentCallersRunsTheVerbOnce(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	const callers = 8
	start := make(chan struct{})
	results := make([]HubCommandResult, callers)
	errs := make([]error, callers)
	done := make(chan struct{}, callers)
	for i := range callers {
		go func(i int) {
			<-start
			results[i], errs[i] = sup.Command(ctx, HubCommand{SessionID: id, CommandID: "steer-once", Verb: HubVerbSteer,
				Args: []byte(`{"text":"STEER-ONCE"}`)})
			done <- struct{}{}
		}(i)
	}
	close(start)
	for range callers {
		<-done
	}
	stored, err := e.store.SteerMessages(ctx, id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Body != "STEER-ONCE" {
		t.Fatalf("a duplicate command id queued %d steer messages: %+v", len(stored), stored)
	}
	accepted := 0
	for i := range callers {
		switch {
		case errs[i] == nil && results[i].Status == HubAccepted:
			accepted++
		case errors.Is(errs[i], ErrHubCommandPending) && results[i].Status == HubPending:
		default:
			t.Fatalf("caller %d: %+v %v", i, results[i], errs[i])
		}
	}
	if accepted != 1 {
		t.Fatalf("%d of %d concurrent callers executed the verb", accepted, callers)
	}
}
