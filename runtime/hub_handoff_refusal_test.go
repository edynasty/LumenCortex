package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestHubRefusedHandoffLeavesTheLiveSourceRunning pins that a handoff refused because of the target's state
// never stops the source: a busy target is refused before the source is cancelled, so the source keeps its
// run and its claims.
func TestHubRefusedHandoffLeavesTheLiveSourceRunning(t *testing.T) {
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
	ctx := context.Background()
	if err := e.ConfigureHub(ctx, source, []HubTaskSpec{{ID: "u1", Goal: "HUB:unit"}}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{source, target} {
		if _, err := sup.Start(ctx, id, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
			t.Fatal(err)
		}
		p.waitCount(t, i+1)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: source, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target}); !errors.Is(err, ErrRunHeld) {
		t.Fatalf("handoff to a busy target: %v", err)
	}
	if !sup.Active(source) {
		t.Error("a refused handoff stopped the live source")
	}
	leases, err := e.HubTaskLeases(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Holder != source {
		t.Errorf("leases after a refused handoff: %+v (want u1 held by the source)", leases)
	}
	sup.Cancel(source)
	sup.Cancel(target)
	waitInactive(t, sup, source, target)
}

// TestHubHandoffRefusesALiveWorktreeOwnerWithoutStoppingIt pins the worktree owner rule for a running
// source: the refusal comes before the source is cancelled.
func TestHubHandoffRefusesALiveWorktreeOwnerWithoutStoppingIt(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	root, _ := taskRepo(t)
	e, sup := openTaskEngine(t, root)
	defer e.Close()
	defer sup.Close()
	p := &handoffProvider{hold: true, gate: make(chan struct{})}
	owner := newParentSession(t, e, p)
	other := newParentSession(t, e, p)
	ctx := context.Background()
	if _, err := e.AttachWorktree(ctx, owner, "HEAD"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Start(ctx, owner, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 3}); err != nil {
		t.Fatal(err)
	}
	p.waitRequest(t)
	if _, err := sup.Handoff(ctx, HubHandoff{Source: owner, Target: other}); !errors.Is(err, ErrHubWorktreeOwned) {
		t.Fatalf("cross-session worktree handoff: %v", err)
	}
	if !sup.Active(owner) {
		t.Error("a refused handoff stopped the live worktree owner")
	}
	sup.Cancel(owner)
	waitInactive(t, sup, owner)
}
