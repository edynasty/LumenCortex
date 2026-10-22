# ADR-0014: Per-call approval for a tool call above the run's ceiling

- **Status:** Proposed (design only; implementation waits for owner acceptance)
- **Date:** 2026-10-22
- **Owners:** project maintainers
- **Supersedes:** none
- **Superseded by:** none

## Context

A run's authority is a policy tier fixed at Start: `read-only`, `workspace` or `full`
(`internal/toolset/toolset.go`), intersected with the agent's ceiling (ADR-0009). A tool above the
tier is hidden from the model and refused if called (`Denied: true`). There is no way for a user to
let one specific side effect through without switching the whole thread to a higher tier.

The owner decided on 2026-10-22 that Ask stays restrained but that a write or other side effect
may happen "according to the specific tool permission and the user's authorization", and that this
needs a formal ADR. The 2026-10-15 product review deferred "approvals that pause a run mid-flight
(needs a new waiting protocol, lock and crash semantics)". This ADR supplies that protocol.

Existing pieces this design reuses:

- `waiting_gate`: the loop ends a run with a non-terminal status, releases the run lock, and the
  host continues with a Start after `ApproveWorkflowGate` wrote the decision into Task State
  (`internal/agent/loop.go` gate check, `runtime/agent.go` ApproveWorkflowGate).
- `persistToolResult` commits a tool row and its Task State change in one transaction.
- `session.Reconcile` appends an `interrupted` tool row for any call of the newest assistant row
  that has no result (`internal/session/reconcile.go`).
- `window.Build` refuses an unpaired transcript (`KindTranscriptUnpaired`).

## Decision

D1. **Askable permissions are declared by the agent.** An agent definition gains
`approvals: [permission...]` (subset of `write`, `exec`). It cannot reach above `full`, and it is
independent of the agent's ceiling, which still fixes what runs without asking. Built-in Ask declares `["write"]`; every other built-in declares none; custom
agents default to none. A run whose tier denies a permission listed in `approvals` sees those tools
in its tool list with a description suffix "(requires the user's approval for each call)". Tools
whose permission is neither allowed nor askable stay hidden, as today. Children (explore, worker,
start_task runs) never ask: they are non-interactive and keep today's refusal.

D2. **An askable call pauses the run.** When the model calls an askable tool, the loop executes
every earlier call of that assistant message as usual, then stops before the askable call. The
askable call and every later call of the same message become the *pending set*, in order. The loop
records the pending set in Task State (`approvals.pending`: call id, tool, permission, a SHA-256 of
the canonical arguments, requested_at), writes no tool row for them, sets the session status to
`waiting_approval` with cause kind `approval_required`, checkpoints `agent.safe` and publishes
`approval.requested`. The run lock is released like `waiting_gate`.

D3. **The user decides through Core.** `Engine.DecideToolCall(sessionID, callID, decision, actor)`
takes the run lock, checks that the call is the first undecided entry of `approvals.pending` and
that the arguments hash still matches the stored assistant row, and writes the decision into Task
State (reason `user`). The status stays `waiting_approval`; the host continues with a Start, as
after a gate approval. A decision is valid for that one call id only; there is no "always allow"
in this ADR.

D4. **Start resolves decisions before building a window.** A Start on a `waiting_approval` session
processes the pending set in order: an approved call is executed exactly once and its tool row is
committed with the Task State change that removes it from the pending set (one transaction,
existing `persistToolResult`); a denied call gets a tool row "denied by the user" with
`Denied: true`; a later askable call that is still undecided stops the run again (D2). Non-askable
calls in the pending set run as before. Only then does the loop build the next request.

D5. **Crash semantics.** `Reconcile` does not fill the tail gap while the session status is
`waiting_approval` (the gap is intentional and recorded in Task State). A crash after Start set the
status to `running` and before the tool row commit leaves `running` + a gap, which Reconcile turns
into the existing `interrupted` tool row: an approved side effect may or may not have happened, and
the model is told to re-check, as for any interrupted call. An approval is never replayed after an
interruption.

D6. **Context reset, delete and worktree changes cancel pending approvals.** `ResetContext`,
session deletion, worktree removal or apply while `waiting_approval` write denial rows for the
pending set (reason recorded) before proceeding, so no stale approval can execute against a changed
workspace.

D7. **Desktop renders, Core decides.** Desktop shows the pending call with the existing
ApprovalCard: tool, target path or command, and a diff preview for `apply_patch`/`write_file`/
`replace_in_file` computed by Core from the stored arguments. Desktop holds no approval state of its
own.

## Invariants

- No tool call above the tier executes without a recorded user decision for that exact call id and
  argument hash.
- An approved call executes at most once.
- The transcript is always paired when a request is built; the only intentional gap is the pending
  set while `waiting_approval`.
- Children never pause for approval.
- Approval never widens the ceiling of later calls.

## Alternatives considered

### Grant-and-reissue

Approve a (tool, args-hash) grant and tell the model to call again. Rejected: the model may change
the arguments, which turns into another approval round or a silent mismatch, and the transcript
carries a refused call plus a duplicate.

### Placeholder tool row

Write a tool row "awaiting approval" at pause time and append the real result later. Rejected: a
call would have two results, which providers reject or interpret inconsistently.

### Raise the thread tier temporarily

Rejected: it widens authority for every call in the window, which is what the owner asked to avoid.

## Consequences

### Positive

- Ask can perform a specific write with explicit consent without a mode switch.
- Reuses the gate lifecycle, Task State versioning and the tool-row transaction; no new table.

### Negative / trade-offs

- A new non-terminal status (`waiting_approval`) touches every status switch in Core and Desktop
  (Desktop has hard-coded status styling in `presentation-model.ts`, `ThreadRow.tsx`,
  `ExecutionTasks.tsx`).
- A multi-call message can pause several times.

## Security and trust impact

Approval is a per-call capability granted by a human; it is bounded by `full` and by the agent's
`approvals` list. A project (workspace) configuration may not add `approvals` to an agent unless the
workspace is trusted (ADR-0013); global configuration may.

## Persistence / compatibility impact

Task State gains `approvals.pending` and decisions (versioned like workflow gates); sessions gain
status `waiting_approval`. Older Desktop builds would show an unknown status; the Desktop update must
ship with the Core change.

## Validation plan

- Loop tests: pause before the first askable call, earlier calls executed, later calls held;
  approve executes once; deny writes a denied row; mixed pending set; crash between Start and commit
  yields one interrupted row; Reconcile leaves a `waiting_approval` gap alone; argument hash
  mismatch refused; reset/delete/worktree apply cancel pending approvals; children never pause.
- Desktop: ApprovalCard shows the call and preview, approve/deny round-trip with a fake provider.
- One real-model run with Ask asking to edit a file.

## Documentation impact

`docs/security-and-trust.md` (human gates), `docs/agent-runtime.md` (statuses), ADR-0009
(agent definition field), Desktop interaction spec.
