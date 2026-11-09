package runtime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/taskstate"
)

// Delivery is host-selected and independent of the Agent's execution status.
type DeliveryContract = taskstate.DeliveryContract
type DeliveryFile = taskstate.DeliveryFile
type DeliveryResult = taskstate.DeliveryResult
type DeliverySnapshot = taskstate.DeliverySnapshot
type DeliveryIssue = taskstate.DeliveryIssue
type DeliveryFileEvidence = taskstate.DeliveryFileEvidence

const (
	DeliveryPending     = taskstate.DeliveryPending
	DeliveryVerified    = taskstate.DeliveryVerified
	DeliveryFailed      = taskstate.DeliveryFailed
	DeliveryUnavailable = taskstate.DeliveryUnavailable
)

// ErrNoDeliveryContract: the session has no host delivery contract to verify.
var ErrNoDeliveryContract = errors.New("session has no delivery contract")

// VerifyDelivery retries only read-only verification of a completed execution.
// It never starts an Agent, refreshes baselines, or replays successful tools.
func (e *Engine) VerifyDelivery(ctx context.Context, sessionID string) (*DeliveryResult, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	ctx, done := e.trackRun(ctx, sessionID)
	defer done()
	_, lease, err := e.leaseWorkspace(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	row, err := e.store.Get(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if row.Status != "completed" {
		return nil, errors.New("delivery verification requires a completed Agent execution")
	}
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if state.Delivery == nil {
		return nil, ErrNoDeliveryContract
	}
	runID := ""
	if state.DeliveryResult != nil {
		runID = state.DeliveryResult.RunID
	}
	// A retry first supersedes the stored outcome, as a Start does: until this attempt settles, and if
	// its settlement cannot be saved, readers see pending, never an earlier verified that no longer holds.
	// An attempt whose start cannot be recorded does not run.
	state.DeliveryResult = &DeliveryResult{Status: DeliveryPending, RunID: runID, Issues: slices.Clone(state.Delivery.BaselineIssues)}
	if state.Resolution.By == taskstate.ByEvidence {
		state.Reopen(time.Now().UTC())
	}
	if _, err := e.putTaskState(ctx, sessionID, deliveryReason, state, 0, nil); err != nil {
		return nil, fmt.Errorf("record delivery verification start: %w", err)
	}
	return e.settleDelivery(ctx, sessionID, runID), nil
}

// deliveryReason is the Task State version reason of a delivery record outside a Start.
const deliveryReason = "delivery"

// settleDelivery runs after Loop.Run has committed completed and owned jobs have
// stopped, for a session whose contract the caller knows. Even a verifier panic or
// evidence-storage failure cannot change that committed execution outcome. Every
// exit publishes the result: an asynchronous host has no other way to see it.
func (e *Engine) settleDelivery(ctx context.Context, sessionID, runID string) *DeliveryResult {
	result := e.verifyDelivery(ctx, sessionID, runID)
	e.events.publish("delivery.result", sessionID, result)
	return &result
}

// verifyDelivery verifies and records one attempt. The stored pending record its caller wrote stays
// in place when this record cannot be saved. Queued host edits are applied before the resolution rule
// runs and consumed with the record, so the user's resolution is never replaced by an automatic one.
func (e *Engine) verifyDelivery(ctx context.Context, sessionID, runID string) DeliveryResult {
	loadCtx, loadCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	state, err := e.loadTaskState(loadCtx, sessionID)
	loadCancel()
	if err != nil {
		return DeliveryResult{Status: DeliveryUnavailable, RunID: runID, Issues: []DeliveryIssue{{Phase: "state", Reason: err.Error()}}}
	}
	if state.Delivery == nil {
		return DeliveryResult{Status: DeliveryUnavailable, RunID: runID, Issues: []DeliveryIssue{{Phase: "state", Reason: "the stored Task State lost the delivery contract its Start recorded"}}}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := inspectDelivery(func() DeliveryResult {
		pre, err := e.DispatchExecutionHook(ctx, LifecycleRequest{Event: LifecycleBeforeDelivery, SessionID: sessionID, RunID: runID})
		if err != nil {
			return unavailableDelivery("hook", "", err)
		}
		if pre.Denied() {
			return unavailableDelivery("hook", "", errors.New("delivery denied: "+pre.FeedbackText()))
		}
		workspace, err := e.agentWorkspace(ctx, sessionID)
		if err != nil {
			return unavailableDelivery("workspace", "", err)
		}
		repo, err := repository.New(workspace)
		if err != nil {
			return unavailableDelivery("workspace", workspace, err)
		}
		return repo.VerifyDelivery(ctx, state.Delivery)
	})
	result.RunID = runID
	// Recording the attempt is cleanup, not resumed Agent execution: a cancelled verifier is recorded.
	saveCtx, saveCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer saveCancel()
	edits, err := e.inboxEdits(saveCtx, sessionID)
	if err != nil {
		result.Status = DeliveryUnavailable
		result.Issues = append(result.Issues, DeliveryIssue{Phase: "persistence", Reason: "queued host edits unreadable: " + err.Error()})
		return result
	}
	before := state.Clone()
	var through int64
	if len(edits) > 0 {
		state.Apply(edits)
		through = edits[len(edits)-1].Seq
	}
	state.DeliveryResult = &result
	if result.Status != DeliveryVerified && state.Resolution.By == taskstate.ByEvidence {
		state.Reopen(time.Now().UTC())
	}
	resolved := false
	if result.Status == DeliveryVerified {
		if ev, err := e.taskEvidence(saveCtx, sessionID, state); err == nil {
			resolved = state.AutoDone(ev, time.Now().UTC())
		} else {
			e.events.publish("task_state.evidence_unavailable", sessionID, map[string]any{"reason": deliveryReason, "error": err.Error()})
		}
	}
	if _, err := e.putTaskState(saveCtx, sessionID, deliveryReason, state, through, agent.HostStateEdges(before, state)); err != nil {
		result.Status = DeliveryUnavailable
		result.Issues = append(result.Issues, DeliveryIssue{Phase: "persistence", Reason: err.Error()})
		return result
	}
	if resolved {
		e.events.publish("task_state.resolved", sessionID, map[string]any{"state": state.Resolution.State, "by": state.Resolution.By})
	}
	_, _ = e.DispatchExecutionHook(saveCtx, LifecycleRequest{Event: LifecycleAfterDelivery, SessionID: sessionID, RunID: runID, Detail: map[string]any{"status": result.Status}})
	return result
}

func unavailableDelivery(phase, path string, err error) DeliveryResult {
	return DeliveryResult{Status: DeliveryUnavailable, Issues: []DeliveryIssue{{Phase: phase, Path: path, Reason: err.Error()}}}
}

// This boundary catches verifier defects, not Agent/tool exceptions, and retains
// their cause as unavailable evidence instead of manufacturing success or failure.
func inspectDelivery(verify func() DeliveryResult) (result DeliveryResult) {
	defer func() {
		if value := recover(); value != nil {
			result = unavailableDelivery("panic", "", fmt.Errorf("delivery verifier panic: %v", value))
		}
	}()
	return verify()
}
