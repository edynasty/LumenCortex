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
