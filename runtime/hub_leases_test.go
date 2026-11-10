package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/edynasty/LumenCortex/protocol"
)

// TestHubTaskLeaseClaimExpiryAndRecovery pins the Task Lease contract (collaboration §4): one valid owner
// at a time, expiry returns the unit to ready, renewal keeps it, and release frees it at once.
func TestHubTaskLeaseClaimExpiryAndRecovery(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	root, _ := taskRepo(t)
	e, sup := openTaskEngine(t, root)
	defer e.Close()
	defer sup.Close()
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	if err := e.ConfigureHub(ctx, id, []HubTaskSpec{
		{ID: "t1", Goal: "HUB:first"},
		{ID: "t2", Goal: "HUB:second", DependsOn: []string{"t1"}},
	}); err != nil {
		t.Fatal(err)
	}
	holderA := newParentSession(t, e, p)
	holderB := newParentSession(t, e, p)
	lease, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "t1", Holder: holderA, TTL: 150 * time.Millisecond})
	if err != nil || lease.Holder != holderA || lease.LeaseID == "" {
		t.Fatalf("claim %+v %v", lease, err)
	}
	// A second claimer is refused while the lease lives, from a concurrent caller included.
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "t1", Holder: holderB, TTL: time.Minute}); !errors.Is(err, ErrHubLeaseHeld) {
		t.Fatalf("second claimer %v", err)
	}
	var wg sync.WaitGroup
	results := make([]error, 4)
	for i := range 4 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "t1", Holder: holderB, TTL: time.Minute})
		}(i)
	}
	wg.Wait()
	for i, err := range results {
		if !errors.Is(err, ErrHubLeaseHeld) {
			t.Fatalf("concurrent claimer %d: %v", i, err)
		}
	}
	// A dependency that is not verified blocks the claim of the dependent unit.
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "t2", Holder: holderA, TTL: time.Minute}); err == nil {
		t.Fatal("claimed a unit whose dependency is not verified")
	}
	// Renewal extends the live claim.
	renewed, err := e.RenewHubTaskLeases(ctx, holderA)
	if err != nil || len(renewed) != 1 || !renewed[0].ExpiresAt.After(time.Now().Add(DefaultHubTaskLeaseTTL/2)) {
		t.Fatalf("renew %+v %v", renewed, err)
	}
	// Release frees the unit at once, and a fresh short lease expires into ready.
	if err := e.ReleaseHubTaskLease(ctx, id, "t1", holderA); err != nil {
		t.Fatal(err)
	}
	if err := e.ReleaseHubTaskLease(ctx, id, "t1", holderA); !errors.Is(err, ErrHubLeaseNotHeld) {
		t.Fatalf("double release %v", err)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "t1", Holder: holderA, TTL: 100 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	// Expiry returns the unit to ready and the next claimer wins.
	time.Sleep(200 * time.Millisecond)
	views, err := e.HubTaskLeases(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 0 {
		t.Fatalf("expired lease still listed: %+v", views)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "t1", Holder: holderB, TTL: time.Minute}); err != nil {
		t.Fatalf("claim after expiry %v", err)
	}
	if err := e.ReleaseHubTaskLease(ctx, id, "t1", holderB); err != nil {
		t.Fatal(err)
	}
	// The projection shows an owner and its expiry, and the task state reads leased.
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "t1", Holder: holderA, TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	view, err := sup.View(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var t1 *HubTask
	for i := range view.Tasks {
		if view.Tasks[i].ID == "t1" {
			t1 = &view.Tasks[i]
		}
	}
	if t1 == nil || t1.Lease == nil || t1.Lease.Holder != holderA || t1.State != "leased" {
		t.Fatalf("view task %+v", t1)
	}
	// A run that ends releases its claims.
	e.releaseHeldLeases(holderA)
	remaining, err := e.HubTaskLeases(ctx, id)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("release at run end %+v %v", remaining, err)
	}
}

// TestHubTaskLeaseRenewedAtStepBoundary pins the automatic renewal: a worker that takes a step keeps its
// claim past the original expiry.
func TestHubTaskLeaseRenewedAtStepBoundary(t *testing.T) {
	useGlobalConfig(t, withConcurrency(4, 3))
	root, _ := taskRepo(t)
	e, sup := openTaskEngine(t, root)
	defer e.Close()
	defer sup.Close()
	p := &hubScriptProvider{}
	id := newParentSession(t, e, p)
	ctx := context.Background()
	if err := e.ConfigureHub(ctx, id, []HubTaskSpec{{ID: "self", Goal: "HUB:self"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ClaimHubTask(ctx, HubLeaseRequest{Root: id, Unit: "self", Holder: id, TTL: 300 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	// One run with one model step: the AfterModelCall boundary renews the claim.
	p.respond = func(int, protocol.ProviderRequest) protocol.ProviderResponse {
		time.Sleep(60 * time.Millisecond)
		return protocol.ProviderResponse{Message: protocol.Message{Content: "done"}, FinishReason: "stop"}
	}
	if _, err := sup.Start(ctx, id, AgentOptions{Policy: "read-only", Interactive: true, MaxSteps: 1}); err != nil {
		t.Fatal(err)
	}
	waitInactive(t, sup, id)
	// The run's end released the claim, so the unit is ready; the renewal itself is observable in the
	// event stream.
	page, err := e.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: id, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	renewed, released := 0, 0
	for _, event := range page.Events {
		switch event.Type {
		case "task.lease.renewed":
			renewed++
		case "task.lease.released":
			released++
		}
	}
	if renewed == 0 {
		t.Fatal("no step-boundary renewal was recorded")
	}
	if released == 0 {
		t.Fatal("the run's end released no claim")
	}
}
