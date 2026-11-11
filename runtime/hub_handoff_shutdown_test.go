package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// A shutdown after the claim moves must keep the store open until the failed
// target Start returns the claim. Reopening must show the source as its owner.
func TestHubShutdownWaitsForHandoffRollback(t *testing.T) {
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
	moved, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		sup.Close()
		e.Close()
		testHookAfterHandoffLeaseTransfer = nil
	})
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
	testHookAfterHandoffLeaseTransfer = func() { close(moved); <-release }
	result := make(chan error, 1)
	go func() {
		_, err := sup.Handoff(ctx, HubHandoff{Source: source, Target: target, Note: "shutdown after transfer"})
		result <- err
	}()
	select {
	case <-moved:
	case <-time.After(10 * time.Second):
		t.Fatal("handoff did not transfer the claim")
	}
	leases, err := e.store.TaskLeases(ctx, source)
	if err != nil || len(leases) != 1 || leases[0].Holder != target {
		t.Fatalf("claim before shutdown: %+v, %v", leases, err)
	}
	closed := make(chan error, 1)
	go func() { closed <- e.Close() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		sup.mu.RLock()
		closing := sup.closed
		sup.mu.RUnlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not enter shutdown")
		}
		time.Sleep(time.Millisecond)
	}
	// A second Close must join the same shutdown instead of letting its caller close the store early.
	joined := make(chan struct{})
	go func() { sup.Close(); close(joined) }()
	select {
	case err := <-closed:
		t.Fatalf("Engine.Close returned before handoff rollback: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-joined:
		t.Fatal("concurrent supervisor Close returned before handoff rollback")
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-result:
		if !errors.Is(err, ErrRunSupervisorClosed) {
			t.Fatalf("handoff during shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("handoff did not finish rollback")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Engine.Close did not join the handoff")
	}
	select {
	case <-joined:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent supervisor Close did not join shutdown")
	}
	reopened, err := Open(Options{Workspace: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	leases, err = reopened.store.TaskLeases(ctx, source)
	if err != nil || len(leases) != 1 || leases[0].Holder != source {
		t.Fatalf("claim after reopen: %+v, %v; want source %s", leases, err, source)
	}
	page, err := reopened.QueryRuntimeEvents(ctx, RuntimeEventQuery{SessionID: source, Type: "run.stopped", Limit: 10})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("source stop after reopen: %+v, %v", page.Events, err)
	}
}
