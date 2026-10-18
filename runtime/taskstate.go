package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/protocol"
)

// TaskStateEdit is one host edit of Task State (ADR-0006 D1′/D2): exactly one of Brief, Reference and
// Resolve. Edits are queued without the run lock; a running loop applies them at its next step
// boundary, else the next Start does.
type TaskStateEdit = taskstate.Edit

// TaskReferenceEdit and TaskResolveEdit are the Reference and Resolve parts of a TaskStateEdit, named
// here so a host outside this module can build them.
type (
	TaskReferenceEdit = taskstate.ReferenceEdit
	TaskResolveEdit   = taskstate.ResolveEdit
)

// TaskStateInfo is the host view of a session's Task State: the newest stored version with the queued
// edits applied in memory, so an idle thread shows its edits before any run applies them.
type TaskStateInfo struct {
	SessionID string `json:"sessionId"`
	// Version is the newest stored version (0: none stored yet).
	Version int64 `json:"version"`
	// PendingEdits counts queued edits that no run has applied yet; State already includes them.
	PendingEdits int             `json:"pendingEdits"`
	State        taskstate.State `json:"state"`
	// Obligations is what keeps the thread open; ReadyForReview is the derived "ready for review"
	// value. Neither is stored. EvidenceError reports a workspace that could not be inspected, in
	// which case both leave pending application out.
	Obligations    []string `json:"obligations"`
	ReadyForReview bool     `json:"readyForReview"`
	EvidenceError  string   `json:"evidenceError,omitempty"`
}

// TaskState returns the host view of the session's Task State. It takes no lock.
func (e *Engine) TaskState(ctx context.Context, sessionID string) (TaskStateInfo, error) {
	if _, err := e.store.Get(ctx, sessionID); err != nil {
		return TaskStateInfo{}, err
	}
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return TaskStateInfo{}, err
	}
	edits, err := e.inboxEdits(ctx, sessionID)
	if err != nil {
		return TaskStateInfo{}, err
	}
	state.Apply(edits)
	info := TaskStateInfo{SessionID: sessionID, Version: state.Version, PendingEdits: len(edits), State: state}
	ev, err := e.taskEvidence(ctx, sessionID, state)
	if err != nil {
		info.EvidenceError = err.Error()
	}
	info.Obligations = state.Obligations(ev)
	if info.Obligations == nil {
		info.Obligations = []string{}
	}
	info.ReadyForReview = err == nil && state.ReadyForReview(ev)
	return info, nil
}

// EditTaskState validates and queues one host edit and returns the resulting view.
func (e *Engine) EditTaskState(ctx context.Context, sessionID string, edit TaskStateEdit) (TaskStateInfo, error) {
	if err := edit.Validate(); err != nil {
		return TaskStateInfo{}, err
	}
	raw, err := json.Marshal(edit)
	if err != nil {
		return TaskStateInfo{}, err
	}
	seq, err := e.store.QueueEdit(ctx, sessionID, raw)
	if err != nil {
		return TaskStateInfo{}, err
	}
	e.events.publish("task_state.edit_queued", sessionID, map[string]any{"seq": seq})
	return e.TaskState(ctx, sessionID)
}

// ResolveThread sets the thread's resolution as the user (ADR-0006 D2): open, done or dropped. It goes
// through the inbox like every host edit; automation never overrides it.
func (e *Engine) ResolveThread(ctx context.Context, sessionID, state, actor string) (TaskStateInfo, error) {
	return e.EditTaskState(ctx, sessionID, TaskStateEdit{Resolve: &taskstate.ResolveEdit{State: state, Actor: actor}})
}

// ResetContext moves the window start to the seq the next transcript row will get (design §4.4), so
// later requests hold System, Task State and the rows added after the reset. It takes the run lock and
// returns ErrRunHeld while a run holds it.
func (e *Engine) ResetContext(ctx context.Context, sessionID string) (TaskStateInfo, error) {
	lock, err := e.acquireRun(ctx, sessionID)
	if err != nil {
		return TaskStateInfo{}, err
	}
	defer lock.Release()
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return TaskStateInfo{}, err
	}
	next, err := e.store.NextMessageSeq(ctx, sessionID)
	if err != nil {
		return TaskStateInfo{}, err
	}
	if next > state.WindowStartSeq {
		state.WindowStartSeq = next
		if _, err := e.putTaskState(ctx, sessionID, taskstate.ReasonContextReset, state, 0); err != nil {
			return TaskStateInfo{}, err
		}
	}
	e.events.publish("context.reset", sessionID, map[string]any{"windowStartSeq": state.WindowStartSeq})
	return e.TaskState(ctx, sessionID)
}

func (e *Engine) loadTaskState(ctx context.Context, sessionID string) (taskstate.State, error) {
	row, err := e.store.LatestTaskState(ctx, sessionID)
	if err != nil {
		return taskstate.State{}, err
	}
	return taskstate.Decode(row.Version, row.JSON)
}

// inboxEdits reads the queued edits. A row that does not decode becomes an empty edit, which Apply
// skips and the next write consumes.
func (e *Engine) inboxEdits(ctx context.Context, sessionID string) ([]taskstate.QueuedEdit, error) {
	rows, err := e.store.InboxEdits(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return queuedEdits(rows), nil
}

func queuedEdits(rows []session.InboxRow) []taskstate.QueuedEdit {
	out := make([]taskstate.QueuedEdit, 0, len(rows))
	for _, row := range rows {
		edit, _ := taskstate.DecodeEdit(row.JSON)
		out = append(out, taskstate.QueuedEdit{Seq: row.Seq, At: row.CreatedAt, Edit: edit})
	}
	return out
}

// putTaskState writes state as the next version on its own; the caller holds the run lock.
func (e *Engine) putTaskState(ctx context.Context, sessionID, reason string, state taskstate.State, inboxThrough int64) (int64, error) {
	raw, err := state.Encode()
	if err != nil {
		return 0, err
	}
	version, err := e.store.PutTaskState(ctx, sessionID, session.TaskStateWrite{
		Base: state.Version, Reason: reason, JSON: raw, InboxThrough: inboxThrough,
	})
	if err != nil {
		return 0, err
	}
	e.events.publish("task_state.changed", sessionID, map[string]any{"version": version, "reason": reason})
	return version, nil
}

// taskEvidence is what the resolution rules read beyond the state: the stored workflow's verdicts, the
// session's workspace and its required checks under the effective configuration.
func (e *Engine) taskEvidence(ctx context.Context, sessionID string, state taskstate.State) (taskstate.Evidence, error) {
	ev := taskstate.Evidence{}
	wf, err := state.RestoreWorkflow()
	if err != nil {
		return ev, err
	}
	if wf != nil {
		ev.Workflow, ev.WorkflowCanFinish, ev.GatePending = true, wf.CanFinish(), len(wf.PendingGates()) > 0
	}
	pending, refs, err := e.workspaceEvidence(ctx, sessionID)
	if err != nil {
		return ev, err
	}
	ev.PendingApplication, ev.Refs = pending, refs
	if ev.Tasks, err = e.executionEvidence(ctx, sessionID); err != nil {
		return ev, err
	}
	specs, _, err := e.effectiveChecks()
	if err != nil {
		return ev, err
	}
	evaluator, err := e.sessionEvaluator(ctx, sessionID)
	if err != nil {
		return ev, err
	}
	if ev.Checks, err = checkEvidence(ctx, evaluator, specs); err != nil {
		return ev, err
	}
	return ev, nil
}

// latchEvidence applies the evidence rule outside a run (ADR-0006 D2), after an Apply: the caller
// holds the run lock. Queued host edits are applied first, so a user's resolution is never overridden,
// and are consumed only when a version is written. A failure is reported as an event: the operation
// that triggered the latch has already succeeded.
func (e *Engine) latchEvidence(ctx context.Context, sessionID, reason string) {
	fail := func(err error) {
		e.events.publish("task_state.evidence_unavailable", sessionID, map[string]any{"reason": reason, "error": err.Error()})
	}
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		fail(err)
		return
	}
	edits, err := e.inboxEdits(ctx, sessionID)
	if err != nil {
		fail(err)
		return
	}
	state.Apply(edits)
	ev, err := e.taskEvidence(ctx, sessionID, state)
	if err != nil {
		fail(err)
		return
	}
	if !state.AutoDone(ev, time.Now().UTC()) {
		return
	}
	var through int64
	if len(edits) > 0 {
		through = edits[len(edits)-1].Seq
	}
	if _, err := e.putTaskState(ctx, sessionID, reason, state, through); err != nil {
		fail(err)
		return
	}
	e.events.publish("task_state.resolved", sessionID, map[string]any{"state": state.Resolution.State, "by": state.Resolution.By})
}

// workspaceEvidence reports whether the session has work pending application — its owned worktree is
// dirty or holds commits not applied to the main checkout — and the head a resolution rests on. A
// borrower's pending work belongs to the owner; a local session has none.
func (e *Engine) workspaceEvidence(ctx context.Context, sessionID string) (bool, []string, error) {
	in, err := e.InspectSessionWorktree(ctx, sessionID)
	if err != nil {
		return false, nil, err
	}
	if in.Uncertain != "" {
		return false, nil, fmt.Errorf("%w: %s", ErrWorktreeOwnership, in.Uncertain)
	}
	if in.Role == WorktreeOwner {
		pending := len(in.DirtyFiles) > 0 || len(in.UnappliedCommits) > 0
		if in.Head == "" {
			return pending, nil, nil
		}
		return pending, []string{"head:" + in.Head}, nil
	}
	head, err := e.repo.GitHead(ctx)
	if err != nil || head == "" {
		// A workspace outside Git has no head to record.
		return false, nil, nil
	}
	return false, []string{"head:" + head}, nil
}

// executionEvidence lists the session's execution children with their stored run status: an open one
// is an obligation and an unsuccessful one blocks the evidence rule (ADR-0006 D2).
func (e *Engine) executionEvidence(ctx context.Context, sessionID string) ([]taskstate.TaskEvidence, error) {
	relations, err := e.store.ChildRelations(ctx, sessionID, MaxSubagentTreeNodes)
	if err != nil {
		return nil, err
	}
	var out []taskstate.TaskEvidence
	for _, r := range relations {
		if r.Kind != RelationExecution {
			continue
		}
		child, err := e.store.Get(ctx, r.ChildSessionID)
		if err != nil {
			return nil, err
		}
		out = append(out, taskstate.TaskEvidence{SessionID: child.ID, Status: child.Status})
	}
	return out, nil
}

// startTaskState builds the Start's Task State version (design §4.4, ADR-0006 D1′, D4): the workflow
// and work-unit seeds, each only while the state holds none, then the queued edits, then the delivery
// cursor of the task_result rows it returns. A seed is a new obligation and reopens a resolved thread.
// It returns a nil write when nothing changes, and rejects an invalid or conflicting seed before
// anything is written.
func (e *Engine) startTaskState(ctx context.Context, sessionID string, opts AgentOptions) (*session.TaskStateWrite, []protocol.Message, error) {
	state, err := e.loadTaskState(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	stored, err := state.Encode()
	if err != nil {
		return nil, nil, err
	}
	seeded, err := state.Seed(opts.Workflow, opts.WorkUnits)
	if err != nil {
		return nil, nil, err
	}
	if seeded {
		state.Reopen(time.Now().UTC())
	}
	edits, err := e.inboxEdits(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	var through int64
	if len(edits) > 0 {
		state.Apply(edits)
		through = edits[len(edits)-1].Seq
	}
	results, err := e.taskResults(ctx, sessionID, &state)
	if err != nil {
		return nil, nil, err
	}
	raw, err := state.Encode()
	if err != nil {
		return nil, nil, err
	}
	if through == 0 && bytes.Equal(raw, stored) {
		return nil, nil, nil
	}
	return &session.TaskStateWrite{Base: state.Version, Reason: taskstate.ReasonStart, JSON: raw, InboxThrough: through}, results, nil
}

// agentTaskStateChange converts the loop's change to the store's write.
func agentTaskStateChange(change *agent.TaskStateChange) (*session.TaskStateWrite, error) {
	if change == nil {
		return nil, nil
	}
	raw, err := change.Next.Encode()
	if err != nil {
		return nil, err
	}
	return &session.TaskStateWrite{Base: change.Next.Version, Reason: change.Reason, JSON: raw, InboxThrough: change.InboxThrough}, nil
}

// errNoWorkflow: the session has no workflow to approve a gate of.
var errNoWorkflow = errors.New("session has no active workflow")
