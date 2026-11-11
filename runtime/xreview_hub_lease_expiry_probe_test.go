package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestXReviewHubTransferredLeaseGetsTheReceiversLeaseLifetime pins that the target takes the unit over as a live
// claim, not with whatever was left of the source's expiry: the claim's lifetime is the target's, whose
// first step boundary is a whole model request away.
func TestXReviewHubTransferredLeaseGetsTheReceiversLeaseLifetime(t *testing.T) {
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
	// A short claim: the source is about to hand the unit over, so only a moment of its TTL is left.
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: source, TTL: 500 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target}); err != nil {
		t.Fatal(err)
	}
	p.waitCount(t, 2) // the target is inside its first model request
	// The target's first step boundary is still ahead; the claim must not lapse meanwhile.
	time.Sleep(700 * time.Millisecond)
	leases, err := e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != target {
		t.Fatalf("the target lost the unit while it was working it: %+v", leases)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: third, TTL: time.Minute}); !errors.Is(err, ErrHubLeaseHeld) {
		t.Fatalf("a third worker claimed the unit the target is working: %v", err)
	}
	p.release()
	waitInactive(t, sup, source, target)
}

// TestXReviewHubSecondHandoffOfTheSameSourceStartsAnUnownedTarget records the documented gap of a
// handoff that names a source whose work already moved: the first handoff transferred the unit to T1, and
// a second handoff of the same (now idle) source starts T2 with no claim at all while T1 still owns the
// unit. This is the Hub's ownership model, not a handoff regression: refusing or re-claiming is a
// product rule the slice does not decide.
func TestXReviewHubSecondHandoffOfTheSameSourceStartsAnUnownedTarget(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	source := newParentSession(t, e, p)
	first := newParentSession(t, e, p)
	second := newParentSession(t, e, p)
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
	if _, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: first}); err != nil {
		t.Fatal(err)
	}
	p.waitCount(t, 2)
	if _, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: second}); err != nil {
		t.Fatalf("the second handoff of an idle source: %v", err)
	}
	p.waitCount(t, 3)
	leases, err := e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != first {
		t.Fatalf("the unit's owner after two handoffs: %+v", leases)
	}
	// The second target runs the same work with no claim of it.
	held, err := e.store.TaskLeasesHeldBy(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 0 {
		t.Fatalf("the second target unexpectedly owns %+v", held)
	}
	if !sup.Active(second) || !sup.Active(first) {
		t.Fatalf("both handoff targets should be running: first=%v second=%v", sup.Active(first), sup.Active(second))
	}
	p.release()
	sup.Cancel(first)
	sup.Cancel(second)
	waitInactive(t, sup, source, first, second)
}
