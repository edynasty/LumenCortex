package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/edynasty/LumenCortex/internal/session"
)

// Task Lease (Hub collaboration §4): a work unit of a Hub task DAG has at most one valid owner at a
// time. The lease is durable, expires on its own, and its holder renews it at step boundaries, so a
// worker that dies returns its unit to ready without a scheduler; a second claimer is refused while the
// lease is live, and wins once it expired. The lease is admission, not authority: a worker still runs
// under its policy, its run lock and its worktree.

// HubTaskLease is one claim of a work unit.
type HubTaskLease = session.TaskLease

// DefaultHubTaskLeaseTTL is how long a claim survives without renewal. A worker renews at every model
// step boundary, so the value bounds how long a crashed worker's unit stays unavailable.
const DefaultHubTaskLeaseTTL = 15 * time.Minute

var (
	// ErrHubLeaseHeld: another worker's live lease owns the unit.
	ErrHubLeaseHeld = session.ErrLeaseHeld
	// ErrHubLeaseNotHeld: the holder/lease named no live claim.
	ErrHubLeaseNotHeld = session.ErrLeaseNotHeld
)

// HubLeaseRequest claims one work unit of root's task DAG for holder.
type HubLeaseRequest struct {
	Root   string        `json:"root"`
	Unit   string        `json:"unit"`
	Holder string        `json:"holder"`
	TTL    time.Duration `json:"ttl,omitempty"`
}

// ClaimHubTask admits holder to one unit of the root's task graph and takes the unit's lease. A unit
// whose verified dependency is incomplete is refused exactly as a proposal admission is, so a lease can
// never make an unready unit runnable.
func (e *Engine) ClaimHubTask(ctx context.Context, req HubLeaseRequest) (HubTaskLease, error) {
	if err := e.runtimeEventAccess(ctx, req.Root); err != nil {
		return HubTaskLease{}, err
	}
	if req.Holder == "" {
		return HubTaskLease{}, errors.New("a Task Lease needs a holder")
	}
	if err := e.runtimeEventAccess(ctx, req.Holder); err != nil {
		return HubTaskLease{}, err
	}
	state, err := e.loadTaskState(ctx, req.Root)
	if err != nil {
		return HubTaskLease{}, err
	}
	views, err := e.hubTasks(ctx, req.Root, state)
	if err != nil {
		return HubTaskLease{}, err
	}
	var unit *HubTask
	for i := range views {
		if views[i].ID == req.Unit {
			unit = &views[i]
			break
		}
	}
	if unit == nil {
		return HubTaskLease{}, errors.New("hub task graph has no such work unit")
	}
	if unit.State == "completed" || unit.State == "cancelled" || unit.State == "failed" {
		return HubTaskLease{}, errors.New("hub work unit is terminal")
	}
	for _, dep := range unit.DependsOn {
		complete := false
		for _, candidate := range views {
			if candidate.ID == dep && candidate.State == "completed" {
				complete = true
				break
			}
		}
		if !complete {
			return HubTaskLease{}, errors.New("hub work unit waits for a verified dependency")
		}
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultHubTaskLeaseTTL
	}
	leaseID, err := newRunID()
	if err != nil {
		return HubTaskLease{}, err
	}
	lease, err := e.store.ClaimTaskLease(ctx, HubTaskLease{RootID: req.Root, UnitID: req.Unit, Holder: req.Holder, LeaseID: leaseID}, ttl)
	if err != nil {
		if errors.Is(err, session.ErrLeaseHeld) {
			e.events.publish("task.lease.rejected", req.Root, map[string]any{"unitId": req.Unit, "holder": req.Holder})
		}
		return HubTaskLease{}, err
	}
	e.events.publish("task.lease.claimed", req.Root, map[string]any{
		"unitId": lease.UnitID, "holder": lease.Holder, "leaseId": lease.LeaseID,
		"expiresAt": lease.ExpiresAt, "ttlMs": ttl.Milliseconds(),
	})
	return lease, nil
}

// RenewHubTaskLeases extends every lease holder owns and reports them: the renewal a worker's step
// boundary performs, and the one a host may request for a worker it knows is alive.
func (e *Engine) RenewHubTaskLeases(ctx context.Context, holder string) ([]HubTaskLease, error) {
	leases, err := e.store.RenewHeldTaskLeases(ctx, holder, DefaultHubTaskLeaseTTL)
	if err != nil {
		return nil, err
	}
	for _, lease := range leases {
		e.events.publish("task.lease.renewed", lease.RootID, map[string]any{
			"unitId": lease.UnitID, "holder": lease.Holder, "leaseId": lease.LeaseID, "expiresAt": lease.ExpiresAt,
		})
	}
	return leases, nil
}

// ReleaseHubTaskLease drops one claim, returning the unit to ready at once.
func (e *Engine) ReleaseHubTaskLease(ctx context.Context, root, unit, holder string) error {
	lease, err := e.store.TaskLeases(ctx, root)
	if err != nil {
		return err
	}
	for _, l := range lease {
		if l.UnitID != unit || l.Holder != holder {
			continue
		}
		if err := e.store.ReleaseTaskLease(ctx, root, unit, holder, l.LeaseID); err != nil {
			return err
		}
		e.events.publish("task.lease.released", root, map[string]any{"unitId": unit, "holder": holder, "leaseId": l.LeaseID})
		return nil
	}
	return session.ErrLeaseNotHeld
}

// HubTaskLeases lists the claims under root, with expired rows already recovered: an expired lease is
// gone and its unit is ready, which is what a reader and a claimer see alike.
func (e *Engine) HubTaskLeases(ctx context.Context, root string) ([]HubTaskLease, error) {
	if err := e.runtimeEventAccess(ctx, root); err != nil {
		return nil, err
	}
	if _, err := e.RecoverExpiredHubTaskLeases(ctx, root); err != nil {
		return nil, err
	}
	return e.store.TaskLeases(ctx, root)
}

// RecoverExpiredHubTaskLeases deletes the expired claims under root and reports them. Their work units
// are ready again; nothing else is written.
func (e *Engine) RecoverExpiredHubTaskLeases(ctx context.Context, root string) ([]HubTaskLease, error) {
	expired, err := e.store.RecoverExpiredTaskLeases(ctx, root)
	if err != nil {
		return nil, err
	}
	for _, lease := range expired {
		e.events.publish("task.lease.expired", root, map[string]any{
			"unitId": lease.UnitID, "holder": lease.Holder, "leaseId": lease.LeaseID, "expiresAt": lease.ExpiresAt,
		})
	}
	return expired, nil
}

// TransferHubTaskLeases moves every claim of from to to: the handoff of a work unit's ownership. The
// moved claims take the receiver's full lease lifetime, because the receiver's first renewal is its
// first model step. It reports how many moved.
func (e *Engine) TransferHubTaskLeases(ctx context.Context, from, to string) (int, error) {
	n, err := e.store.MoveTaskLeases(ctx, from, to, DefaultHubTaskLeaseTTL)
	if err != nil {
		return 0, err
	}
	if n > 0 {
		e.events.publish("task.lease.transferred", from, map[string]any{"from": from, "to": to, "count": n, "ttlMs": DefaultHubTaskLeaseTTL.Milliseconds()})
	}
	return n, nil
}

// renewHeldLeases is the step-boundary renewal of the run's claims. It runs on the run goroutine and
// never fails the run: a renewal error means the unit may be claimed by someone else, and the worker
// learns that from the next claim attempt, not from a broken run.
func (e *Engine) renewHeldLeases(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := e.RenewHubTaskLeases(ctx, sessionID); err != nil {
		e.events.publish("task.lease.error", sessionID, map[string]any{"phase": "renew", "error": err.Error()})
	}
}

// releaseHeldLeases frees every unit the session held when its run left the supervisor, finished or
// cancelled: a finished worker's unit is ready for the next one without waiting for the expiry.
func (e *Engine) releaseHeldLeases(sessionID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	leases, err := e.store.ReleaseTaskLeasesOf(ctx, sessionID)
	if err != nil {
		e.events.publish("task.lease.error", sessionID, map[string]any{"phase": "release", "error": err.Error()})
		return
	}
	for _, lease := range leases {
		e.events.publish("task.lease.released", lease.RootID, map[string]any{"unitId": lease.UnitID, "holder": lease.Holder, "leaseId": lease.LeaseID, "reason": "run_end"})
	}
}

// hubLeaseViews projects the root's live claims into the Hub task view.
func (e *Engine) hubLeaseViews(ctx context.Context, root string) map[string]HubTaskLease {
	out := map[string]HubTaskLease{}
	leases, err := e.store.TaskLeases(ctx, root)
	if err != nil {
		return out
	}
	for _, lease := range leases {
		if lease.Expired {
			continue
		}
		out[lease.UnitID] = lease
	}
	return out
}
