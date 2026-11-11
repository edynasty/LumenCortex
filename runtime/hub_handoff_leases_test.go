package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestHubHandoffOfALiveSourceKeepsItsTaskLeases pins ownership through the handoff of a running source:
// stopping the source for the handoff does not release its claims, so the target - another session, or
// the same session under another agent - owns the unit when the handoff returns, and no third worker can
// claim it in between.
func TestHubHandoffOfALiveSourceKeepsItsTaskLeases(t *testing.T) {
	for _, sameSession := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameSession=%v", sameSession), func(t *testing.T) {
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
			other := newParentSession(t, e, p)
			if sameSession {
				target = source
			}
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
			h := HubHandoff{Source: source, Note: "own the unit"}
			if !sameSession {
				h.Target = target
			}
			got, err := sup.Handoff(ctx, h)
			if err != nil || got != target {
				t.Fatalf("handoff %q %v", got, err)
			}
			leases, err := e.HubTaskLeases(ctx, source)
			if err != nil {
				t.Fatal(err)
			}
			if len(leases) != 1 || leases[0].Holder != target {
				t.Errorf("leases after the handoff of a live source: %+v (want u1 held by %s)", leases, target)
			}
			if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: source, Unit: "u1", Holder: other, TTL: time.Minute}); !errors.Is(err, ErrHubLeaseHeld) {
				t.Errorf("a third worker's claim of the unit the handoff target owns: %v", err)
			}
			// The target is cancelled rather than released: the root's open work unit keeps a run that has no
			// step limit going, and the fixture must end every run it started.
			sup.Cancel(target)
			waitInactive(t, sup, source, target)
		})
	}
}
