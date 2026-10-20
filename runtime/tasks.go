package runtime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/edynasty/LumenCortex/config"
	"github.com/edynasty/LumenCortex/internal/agent"
	"github.com/edynasty/LumenCortex/internal/repository"
	"github.com/edynasty/LumenCortex/internal/runlock"
	"github.com/edynasty/LumenCortex/internal/session"
	"github.com/edynasty/LumenCortex/internal/taskstate"
	"github.com/edynasty/LumenCortex/internal/toolset"
	"github.com/edynasty/LumenCortex/protocol"
)

// Execution tasks (ADR-0006 D4, C2): a child session with relation "execution", its own worktree based
// on the parent workspace's committed HEAD, Policy workspace without shell, MCP or spawn tools, work
// units seeded from the objective and the configured checks. It runs under its own run lock and
// worktree lease and outlives the parent's run; its terminal transitions reach the parent as
// task_result rows at the parent's next Start.

// Relation kinds of child sessions.
const (
	RelationSubagent  = "subagent"
	RelationExecution = "execution"
)

// Delegation grants (AgentOptions.Delegation).
const (
	DelegationNone     = "none"
	DelegationPropose  = agent.DelegationPropose
	DelegationIsolated = agent.DelegationIsolated
)

var (
	// ErrTaskProposalNotFound: the parent's Task State holds no task with that id.
	ErrTaskProposalNotFound = taskstate.ErrTaskNotFound
	// ErrTaskProposalStarted: the task was already started.
	ErrTaskProposalStarted = taskstate.ErrTaskAlreadyStarted
	// ErrExecutionTaskNeedsCommit: a task's worktree is based on a commit; the parent's workspace is
	// not a git repository or has none.
	ErrExecutionTaskNeedsCommit = errors.New("execution tasks need a git workspace with at least one commit")
)

// ExecutionTask is a started execution task.
type ExecutionTask struct {
	SessionID       string `json:"sessionId"`
	ParentSessionID string `json:"parentSessionId"`
	// TaskID is the parent's task id when the task was a proposal; isolated tasks get theirs from the
	// parent's run after the start.
	TaskID  string         `json:"taskId,omitempty"`
	Base    string         `json:"base"`
	Runtime SessionRuntime `json:"runtime"`
	Status  string         `json:"status"`
}

// TaskResult is the JSON of a task_result row, after protocol.TaskResultHeader and a newline. Every
// field is read from durable state when the parent starts: the child's status and cause, its final
// answer, its open work units, its checks at the worktree's current state and its worktree.
type TaskResult struct {
	TaskID    string `json:"taskId,omitempty"`
	SessionID string `json:"sessionId"`
	Objective string `json:"objective"`
	Status    string `json:"status"`
	ErrorKind string `json:"errorKind,omitempty"`
	Error     string `json:"error,omitempty"`
	Final     string `json:"final,omitempty"`
	// OpenUnits are the child's work units that did not reach a terminal status.
	OpenUnits []TaskResultUnit `json:"openUnits,omitempty"`
	// Checks are the configured checks against the worktree as it is now; empty when none are
	// configured. ChecksError says why they could not be read.
	Checks      []TaskResultCheck `json:"checks"`
	ChecksError string            `json:"checksError,omitempty"`
	// Handoff is the worktree the user reviews and applies; HandoffError says why it could not be read.
	Handoff      *TaskHandoff `json:"handoff,omitempty"`
	HandoffError string       `json:"handoffError,omitempty"`
}

type TaskResultUnit struct {
	ID     string `json:"id"`
	Goal   string `json:"goal"`
	Status string `json:"status"`
}

type TaskResultCheck struct {
	ID        string `json:"id"`
	Required  bool   `json:"required"`
	Satisfied bool   `json:"satisfied"`
	// Status is the newest result's status at any fingerprint, "pending" without one.
	Status     string `json:"status"`
	Provenance string `json:"provenance,omitempty"`
}

type TaskHandoff struct {
	Branch     string   `json:"branch"`
	Path       string   `json:"path"`
	Base       string   `json:"base,omitempty"`
	Head       string   `json:"head,omitempty"`
	Exists     bool     `json:"exists"`
	DirtyFiles []string `json:"dirtyFiles,omitempty"`
	// DirtyCount counts every dirty file; DirtyFiles lists at most maxResultFiles.
	DirtyCount       int `json:"dirtyCount"`
	UnappliedCommits int `json:"unappliedCommits"`
}

const (
	maxResultFinalBytes = 4096
	maxResultFiles      = 20
	maxResultUnits      = 20
)

// normalizeDelegation maps "none" to the loop's empty grant and refuses unknown grants.
func normalizeDelegation(d string) (string, error) {
	if !agent.ValidDelegation(d) {
		return "", fmt.Errorf("unknown delegation: %q", d)
	}
	if d == DelegationNone {
		return agent.DelegationNone, nil
	}
	return d, nil
}

// taskStarter is the loop's TaskStarter for a run granted delegation isolated.
type taskStarter struct {
	supervisor *RunSupervisor
	parentID   string
	parentOpts AgentOptions
}

func (t *taskStarter) StartTask(ctx context.Context, objective string) (string, any, error) {
	task, err := t.supervisor.startExecutionTask(ctx, t.parentID, objective, "", t.parentOpts)
	if err != nil {
		return "", nil, err
	}
	return task.SessionID, task, nil
}

// taskStarterFor is the loop's TaskStarter: only a run granted delegation isolated has one.
func taskStarterFor(delegation string, t agent.TaskStarter) agent.TaskStarter {
	if delegation != agent.DelegationIsolated {
		return nil
	}
	return t
}

// StartProposedTask starts the parent's proposed task taskID (ADR-0006 D4): the user's authorization
// of a proposal recorded under delegation propose. It holds the parent's run lock (ErrRunHeld while
// the parent runs), so two calls never both start it, and starts exactly the stored objective. An
// unknown id is ErrTaskProposalNotFound, a started task ErrTaskProposalStarted. A child created for the
// proposal by an earlier call that failed before recording it is reused, not duplicated.
func (s *RunSupervisor) StartProposedTask(ctx context.Context, parentID, taskID string) (ExecutionTask, error) {
	if s == nil || s.engine == nil {
		return ExecutionTask{}, ErrRunSupervisorClosed
	}
	e := s.engine
	lock, err := e.acquireRun(ctx, parentID)
	if err != nil {
		return ExecutionTask{}, err
	}
	defer lock.Release()
	state, err := e.loadTaskState(ctx, parentID)
	if err != nil {
		return ExecutionTask{}, err
	}
	task, ok := state.FindTask(taskID)
	if !ok {
		return ExecutionTask{}, ErrTaskProposalNotFound
	}
	if task.State != taskstate.TaskProposed {
		return ExecutionTask{}, ErrTaskProposalStarted
	}
	child, err := e.proposalChild(ctx, parentID, taskID)
	if err != nil {
		return ExecutionTask{}, err
	}
	var started ExecutionTask
	if child != "" {
		started, err = s.resumeUnstartedTask(ctx, parentID, child, taskID)
	} else {
		started, err = s.startExecutionTask(ctx, parentID, task.Objective, taskID, AgentOptions{})
	}
	if err != nil {
		return ExecutionTask{}, err
	}
	now := time.Now().UTC()
	if err := state.StartTask(taskID, started.SessionID, now); err != nil {
		return started, err
	}
	state.Reopen(now)
	if _, err := e.putTaskState(ctx, parentID, taskstate.ReasonUser, state, 0); err != nil {
		// The child runs; a retry finds it through its relation and records it.
		return started, fmt.Errorf("task %s started as %s but recording it failed: %w", taskID, started.SessionID, err)
	}
	return started, nil
}

// proposalChild is the execution child an earlier StartProposedTask created for taskID, "" for none.
func (e *Engine) proposalChild(ctx context.Context, parentID, taskID string) (string, error) {
	relations, err := e.store.ChildRelations(ctx, parentID, MaxSubagentTreeNodes)
	if err != nil {
		return "", err
	}
	for _, r := range relations {
		if r.Kind != RelationExecution {
			continue
		}
		var meta struct {
			TaskID string `json:"taskId"`
		}
		if json.Unmarshal(r.Metadata, &meta) == nil && meta.TaskID == taskID {
			return r.ChildSessionID, nil
		}
	}
	return "", nil
}

// resumeUnstartedTask completes an earlier start of child: a child that never ran is started now.
func (s *RunSupervisor) resumeUnstartedTask(ctx context.Context, parentID, child, taskID string) (ExecutionTask, error) {
	row, err := s.engine.store.Get(ctx, child)
	if err != nil {
		return ExecutionTask{}, err
	}
	if row.Status == "created" && !s.Active(child) {
		units, err := s.engine.taskUnits(row.Goal)
		if err != nil {
			return ExecutionTask{}, err
		}
		ref, err := s.engine.childModelRef(ctx, parentID)
		if err != nil {
			return ExecutionTask{}, err
		}
		if _, err := s.Start(ctx, child, executionOptions(AgentOptions{}, ref, units, parentID)); err != nil {
			return ExecutionTask{}, err
		}
	}
	return s.engine.executionTask(ctx, parentID, child, taskID)
}

// startExecutionTask creates and starts one execution child of parentID. It refuses beyond
// concurrency.maxChildrenPerParent running children. If anything fails before the child's first run
// has started, the child is removed with its fresh worktree and branch: no partial task is left behind.
func (s *RunSupervisor) startExecutionTask(ctx context.Context, parentID, objective, taskID string, parentOpts AgentOptions) (ExecutionTask, error) {
	objective, err := taskstate.ValidTaskObjective(objective)
	if err != nil {
		return ExecutionTask{}, err
	}
	e := s.engine
	if err := s.checkChildLimit(ctx, parentID); err != nil {
		return ExecutionTask{}, err
	}
	parentRuntime, err := e.SessionRuntime(ctx, parentID)
	if err != nil {
		return ExecutionTask{}, err
	}
	base, err := committedHead(ctx, parentRuntime.Path)
	if err != nil {
		return ExecutionTask{}, err
	}
	ref, err := e.childModelRef(ctx, parentID)
	if err != nil {
		return ExecutionTask{}, err
	}
	units, err := e.taskUnits(objective)
	if err != nil {
		return ExecutionTask{}, err
	}

	handle, err := e.createSession(ctx, SessionOptions{
		Goal:     objective,
		Metadata: map[string]any{session.MetadataKeySubagent: map[string]any{"parentSessionId": parentID, "mode": RelationExecution}},
	})
	if err != nil {
		return ExecutionTask{}, err
	}
	child := handle.ID
	identity, err := e.AttachWorktree(ctx, child, base)
	if err != nil {
		e.discardUnstartedTask(child, SessionRuntime{})
		return ExecutionTask{}, err
	}
	relationMeta, _ := json.Marshal(map[string]any{"taskId": taskID, "base": base})
	if err := e.store.AddRelation(ctx, session.Relation{ParentSessionID: parentID, ChildSessionID: child, Kind: RelationExecution, Metadata: relationMeta}); err != nil {
		e.discardUnstartedTask(child, identity)
		return ExecutionTask{}, err
	}
	if _, err := s.Start(ctx, child, executionOptions(parentOpts, ref, units, parentID)); err != nil {
		e.discardUnstartedTask(child, identity)
		return ExecutionTask{}, err
	}
	payload := map[string]any{"childSessionId": child, "taskId": taskID, "base": base, "branch": identity.Branch}
	_, _ = e.store.AppendCheckpoint(context.Background(), parentID, "task.started", payload)
	e.events.publish("task.started", parentID, payload)
	return e.executionTask(ctx, parentID, child, taskID)
}

// discardUnstartedTask removes a child whose first run never started: its worktree (fresh, holding
// nothing), its branch, its rows and its lock files. Errors are ignored; this is a failure path.
func (e *Engine) discardUnstartedTask(child string, identity SessionRuntime) {
	ctx := context.Background()
	if identity.Kind == RuntimeWorktree {
		_ = e.repo.GitRemoveWorktree(ctx, identity.Path, true)
		_ = e.repo.GitDeleteBranch(ctx, identity.Branch)
		_ = runlock.Remove(e.runDir(), worktreeLeaseID(identity.Path))
	}
	_ = e.store.Delete(ctx, child)
	_ = runlock.Remove(e.runDir(), child)
}

func (e *Engine) executionTask(ctx context.Context, parentID, child, taskID string) (ExecutionTask, error) {
	row, err := e.store.Get(ctx, child)
	if err != nil {
		return ExecutionTask{}, err
	}
	identity, err := e.SessionRuntime(ctx, child)
	if err != nil {
		return ExecutionTask{}, err
	}
	return ExecutionTask{SessionID: child, ParentSessionID: parentID, TaskID: taskID, Base: identity.Base, Runtime: identity, Status: row.Status}, nil
}

// committedHead is the commit HEAD names in workspace.
func committedHead(ctx context.Context, workspace string) (string, error) {
	repo, err := repository.New(workspace)
	if err != nil {
		return "", err
	}
	head, err := repo.GitHead(ctx)
	if err != nil || head == "" {
		return "", ErrExecutionTaskNeedsCommit
	}
	return head, nil
}

// childModelRef is a child's model: config subagentModel, else the parent's pinned model (design §4.5).
func (e *Engine) childModelRef(ctx context.Context, parentID string) (config.ModelRef, error) {
	parent, err := e.store.Get(ctx, parentID)
	if err != nil {
		return "", err
	}
	cfg, err := config.Load(e.workspace)
	if err != nil {
		return "", err
	}
	if ref := cfg.Effective().SubagentModel; ref != "" {
		return ref, nil
	}
	return config.ModelRef(parent.ModelRef), nil
}

// taskUnits is the work-unit seed of a task: one unit for the objective that lists every required
// check, so it completes only on passing evaluator verdicts.
func (e *Engine) taskUnits(objective string) (json.RawMessage, error) {
	specs, _, err := e.effectiveChecks()
	if err != nil {
		return nil, err
	}
	var checks []string
	for _, s := range specs {
		if s.Required {
			checks = append(checks, s.ID)
		}
	}
	return json.Marshal(map[string]any{"workUnits": []map[string]any{{"id": "task", "goal": objective, "checks": checks}}})
}

// executionOptions are an execution child's run options. The child's authority is fixed here, not by
// the host: Policy workspace (file tools confined to its worktree, no shell), no MCP, no spawn, no
// delegation, never interactive. Step and output bounds, effort and the tool denylist come from the
// parent run when there is one.
func executionOptions(parent AgentOptions, ref config.ModelRef, units json.RawMessage, parentID string) AgentOptions {
	return AgentOptions{
		ModelRef:            ref,
		Effort:              parent.Effort,
		Policy:              toolset.PolicyWorkspace,
		MaxSteps:            parent.MaxSteps,
		MaxToolCallsPerStep: parent.MaxToolCallsPerStep,
		ToolDenylist:        append([]string(nil), parent.ToolDenylist...),
		WorkUnits:           units,
		MaxTokens:           parent.MaxTokens,
		Temperature:         parent.Temperature,
		parentSessionID:     parentID,
		disableSubagents:    true,
		disableMCP:          true,
	}
}

// constrainExecutionChild applies an execution child's fixed authority to a Start of that child: an
// explicit resume by the host keeps the model, step bounds and input it asks for, but never a wider
// policy, MCP, spawn tools or delegation. Other sessions are unchanged.
func (e *Engine) constrainExecutionChild(ctx context.Context, sessionID string, opts AgentOptions) (AgentOptions, error) {
	relation, err := e.store.ParentRelation(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return opts, nil
	}
	if err != nil {
		return opts, err
	}
	if relation.Kind != RelationExecution {
		return opts, nil
	}
	opts.Policy = toolset.PolicyWorkspace
	opts.Delegation = ""
	opts.Interactive = false
	opts.parentSessionID = relation.ParentSessionID
	opts.disableSubagents = true
	opts.disableMCP = true
	opts.subagents = nil
	opts.tasks = nil
	return opts, nil
}

// activeChildren counts the parent's children of any kind that run: in this supervisor, or recorded
// running by another process.
func (s *RunSupervisor) activeChildren(ctx context.Context, parentID string) (int, error) {
	relations, err := s.engine.store.ChildRelations(ctx, parentID, MaxSubagentTreeNodes)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, r := range relations {
		if s.Active(r.ChildSessionID) {
			count++
			continue
		}
		row, err := s.engine.store.Get(ctx, r.ChildSessionID)
		if err != nil {
			return 0, err
		}
		if row.Status == agent.StatusRunning {
			count++
		}
	}
	return count, nil
}

// CancelSubtree is the host's Stop for a session and everything it started (ADR-0006 D4): it cancels
// the session's run and the runs of all its descendants that this supervisor executes, and returns
// the ids it cancelled. Cancel alone never reaches a child. Runs held by another process are not
// reachable from here.
func (s *RunSupervisor) CancelSubtree(ctx context.Context, sessionID string) ([]string, error) {
	if s == nil || s.engine == nil {
		return nil, ErrRunSupervisorClosed
	}
	ids := []string{sessionID}
	for i := 0; i < len(ids) && len(ids) <= MaxSubagentTreeNodes; i++ {
		relations, err := s.engine.store.ChildRelations(ctx, ids[i], MaxSubagentTreeNodes)
		if err != nil {
			return nil, err
		}
		for _, r := range relations {
			ids = append(ids, r.ChildSessionID)
		}
	}
	var cancelled []string
	for _, id := range ids {
		if s.Cancel(id) {
			cancelled = append(cancelled, id)
		}
	}
	return cancelled, nil
}

// terminalStatus reports whether a run status is a terminal transition a task_result reports.
func terminalStatus(status string) bool {
	switch status {
	case agent.StatusCompleted, agent.StatusFailed, agent.StatusInterrupted, agent.StatusWaitingGate:
		return true
	}
	return false
}

// taskResults builds the task_result rows of a parent's Start and advances the delivery cursor in
// state: one row for each execution child whose terminal transition (its newest seq and status) the
// cursor has not seen. The caller commits the rows and state in one transaction, under the parent's
// run lock, so each transition is delivered exactly once. A child still running is delivered later.
func (e *Engine) taskResults(ctx context.Context, parentID string, state *taskstate.State) ([]protocol.Message, error) {
	relations, err := e.store.ChildRelations(ctx, parentID, MaxSubagentTreeNodes)
	if err != nil {
		return nil, err
	}
	var rows []protocol.Message
	for _, r := range relations {
		if r.Kind != RelationExecution {
			continue
		}
		child, err := e.store.Get(ctx, r.ChildSessionID)
		if err != nil {
			return nil, err
		}
		if !terminalStatus(child.Status) {
			continue
		}
		seq, err := e.store.NewestMessageSeq(ctx, child.ID)
		if err != nil {
			return nil, err
		}
		mark := taskstate.TaskMark{Seq: seq, Status: child.Status}
		if state.Delivered(child.ID, mark) {
			continue
		}
		result, err := e.taskResult(ctx, *state, r, child)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		rows = append(rows, protocol.Message{Role: "user", Origin: protocol.OriginTaskResult, Content: protocol.TaskResultHeader + "\n" + string(raw)})
		state.MarkDelivered(child.ID, mark)
	}
	return rows, nil
}

// taskResult reads one child's result. Storage errors fail the Start; a worktree or checks that
// cannot be read are reported in the result instead.
func (e *Engine) taskResult(ctx context.Context, parent taskstate.State, relation session.Relation, child session.Session) (TaskResult, error) {
	out := TaskResult{SessionID: child.ID, Objective: truncateBytes(child.Goal, 512), Status: child.Status, Checks: []TaskResultCheck{}}
	for _, t := range parent.Tasks {
		if t.SessionID == child.ID {
			out.TaskID = t.ID
		}
	}
	if out.TaskID == "" {
		var meta struct {
			TaskID string `json:"taskId"`
		}
		_ = json.Unmarshal(relation.Metadata, &meta)
		out.TaskID = meta.TaskID
	}
	if len(child.Error) > 0 {
		var cause struct{ Kind, Message string }
		_ = json.Unmarshal(child.Error, &cause)
		out.ErrorKind, out.Error = cause.Kind, truncateBytes(cause.Message, 512)
	}
	if child.Final != nil {
		out.Final = truncateBytes(*child.Final, maxResultFinalBytes)
	}
	state, err := e.loadTaskState(ctx, child.ID)
	if err != nil {
		return TaskResult{}, err
	}
	for _, u := range state.Units().Incomplete() {
		if len(out.OpenUnits) == maxResultUnits {
			break
		}
		out.OpenUnits = append(out.OpenUnits, TaskResultUnit{ID: u.ID, Goal: truncateBytes(u.Goal, 256), Status: string(u.Status)})
	}

	identity, err := e.SessionRuntime(ctx, child.ID)
	if err != nil {
		return TaskResult{}, err
	}
	if identity.Kind != RuntimeWorktree {
		// The worktree was removed: checks would be read against another workspace.
		out.ChecksError = "the task's worktree was removed"
		out.HandoffError = out.ChecksError
		return out, nil
	}
	if info, err := e.SessionChecks(ctx, child.ID); err != nil {
		out.ChecksError = err.Error()
	} else {
		for _, c := range info.Checks {
			rc := TaskResultCheck{ID: c.Spec.ID, Required: c.Spec.Required, Satisfied: c.Satisfied, Status: "pending"}
			if c.Newest != nil {
				rc.Status, rc.Provenance = c.Newest.Status, c.Newest.Provenance
			}
			out.Checks = append(out.Checks, rc)
		}
	}
	in, err := e.InspectSessionWorktree(ctx, child.ID)
	if err != nil {
		out.HandoffError = err.Error()
		return out, nil
	}
	h := &TaskHandoff{Branch: in.Branch, Path: in.Path, Base: identity.Base, Head: in.Head, Exists: in.Exists, DirtyCount: len(in.DirtyFiles), UnappliedCommits: len(in.UnappliedCommits)}
	h.DirtyFiles = in.DirtyFiles[:min(len(in.DirtyFiles), maxResultFiles)]
	out.Handoff = h
	if in.Uncertain != "" {
		out.HandoffError = in.Uncertain
	}
	return out, nil
}

// truncateBytes cuts s to at most n bytes at a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + " …"
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
