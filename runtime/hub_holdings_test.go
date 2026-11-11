package runtime

import (
	"context"
	"testing"
	"time"
)

// TestHubAgentHoldingsAreTheAgentsOwnLeases pins the per-agent ownership projection: HubAgent.Holdings
// lists the claims the agent holds, wherever the unit's DAG lives, not every claim under its root.
func TestHubAgentHoldingsAreTheAgentsOwnLeases(t *testing.T) {
	useGlobalConfig(t, withConcurrency(8, 4))
	e := openTestEngine(t)
	sup, err := NewRunSupervisor(e)
	if err != nil {
		t.Fatal(err)
	}
	defer sup.Close()
	root, worker := workerAndRoot(t, e, &finalProvider{})
	ctx := context.Background()
	if err := e.ConfigureHub(ctx, root, []HubTaskSpec{{ID: "u1", Goal: "HUB:unit"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: root, Unit: "u1", Holder: worker, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	view, err := sup.View(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	agents := map[string]HubAgent{}
	for _, a := range view.Agents {
		agents[a.SessionID] = a
	}
	if _, ok := agents[worker]; !ok {
		t.Fatalf("the worker is not in the view: %+v", view.Agents)
	}
	if got := agents[worker].Holdings; len(got) != 1 || got[0].UnitID != "u1" || got[0].RootID != root || got[0].Holder != worker {
		t.Errorf("worker holdings %+v, want u1 of %s", got, root)
	}
	if got := agents[root].Holdings; len(got) != 0 {
		t.Errorf("the root holds nothing but shows holdings %+v", got)
	}
}
