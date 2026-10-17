# ADR-0006: Task context, resolution and conversational execution

- **Status:** Accepted (owner decisions D1′–D4 and final Decisions 1–4, 2026-10-15). D3/D4 implementation is gated on the validation items below. D3 is implemented as C1 (2026-10-18, local); see [the C1 report](../reports/C1-interactive-2026-10-18.md), including one implementation-level decision for owner review (required checks after a turn that changed the workspace).
- **Date:** 2026-10-15
- **Owners:** project owner
- **Supersedes:** none. Amends design §4.4, §4.7, §4.9, I10 and §8.2 (M3.1, M4.1 scope; adds C1–C3)
- **Superseded by:** none

## Context

The review (`../../../LumenCortex-architecture-audit/design/handoffs/product-architecture-review-2026-10-15.md`) found three problems:

- The goal and the user's context attachments leave the request after about six steps.
- Every text answer is treated as a completion claim, and the loop forces continuation while work units are open (`internal/agent/loop.go` L182-233).
- Users have no evidence-grounded way to know whether an objective is met.

It also verified the following:

- **P5:** a parent run ends while its `spawn_subagent` child keeps running. The child's result reaches the parent only if the parent's model calls `wait_subagent` in a later run. `Cancel(parent)` does not reach the child.
- **P6:** `RunSupervisor.Close` interrupts children.
- **P7:** policy `workspace` exposes no `shell`.
- `internal/toolset/path.go` confines the file tools, symlinks included, to the run's workspace. `shell` only sets its working directory, so Core cannot confine it.
- The model may move any open work unit to `cancelled`, which today satisfies the completion gate (`internal/cognition/workunit.go` L374-393).

## Decision

### D1′ — Brief and references (M3.1)

- Task State gains:
  - `brief`: at most 4 KiB. It defaults to `sessions.goal`, which stays immutable, and replaces the goal in the Task State block.
  - `references`: entries `{id, kind: path|thread, target, state: included|excluded|suggested, by: user|agent, at}`.
- The Task State block lists the included references by target and note. Content is read through tools; inlining content is a measured M3.2 option, not a default.
- **Exclusion is sticky.**
  - The agent may only add `suggested` entries.
  - It never changes an entry the user set, and never re-suggests an excluded target.
  - Exclusion is context curation, not access control: tools may still read the file when the task requires it.
- **Edits at any time.** Host edits go into a `task_state_inbox` table: one `INSERT`, no run lock. Edits are applied in inbox order:
  - by the run at the next step boundary, in the transaction of that step's Task State version (reason `user`), before the request is built;
  - or by the next Start.
- **Visibility.** An edit committed before step N's request is built is visible in step N. The request already in flight is unaffected.
- Migration 0003 creates the inbox.

### D2 — Resolution (field and manual control in M3.1; evidence rule in M4.1)

- Task State `resolution`: `{state: open|done|dropped, by: evidence|user|reopen, actor, at, refs}`. It is independent of run status, archival and worktree cleanup.

- **Obligations** (each keeps the thread open):
  - non-terminal work units;
  - a pending gate;
  - a workflow that cannot finish;
  - required checks not decided `passed` at the current fingerprint;
  - **pending application**: commits not yet applied, or a dirty owned worktree;
  - open execution tasks.

- **Applying changes discharges only pending application.** It is not acceptance of the objective.

- **Auto-done.** The runtime latches `done` (`by: evidence`, with refs) in the transaction that discharges the last obligation. That transaction is the end of a run, an Apply, or a child's resolution. It latches only when there is positive acceptance evidence:
  - a host workflow that `CanFinish`;
  - every required check decided `passed` by `runtime` or `human:*` at the current fingerprint;
  - or every unit `completed` with evaluator verdicts.

  It does not latch when:
  - the model cancelled a unit since the last reopen (cancellation discharges an obligation but never counts as acceptance);
  - a child failed or was dropped.

  In those cases the host shows **"ready for review"**, a derived value that is not stored.

- **Before M4.1** the only positive evidence is a host workflow. Otherwise the thread can be marked done manually, or it waits for M4.1.

- **Manual control.** `ResolveThread(session, open|done|dropped, actor)` goes through the inbox. Automation never overrides a user's `done` or `dropped`.

- **Reopen.** A new obligation after `done` or `dropped` sets `open` (`by: reopen`). Examples: a unit created, a successful mutating tool, a task started. A question-only turn reopens nothing.

- **Not reopened by drift.** Changes to `main` after `done` do not reopen. The refs record the fingerprint and SHA the decision rested on.

### D3 — A turn may end; completion stays gated (C1, after M4.1)

- `AgentOptions.Interactive` (default `false`, which keeps today's behaviour for the CLI and tests). Desktop sets it for conversation threads.
- In an interactive run without a host workflow, a text answer ends the run `completed`: the run ended normally. Open work units stay open and keep the resolution open. No `completion_blocked` continuation is appended.
- **Workflow gates are not weakened.**
  - When a host workflow is active, interactive runs keep today's `CanFinish` continuation.
  - Approval gates and workflow tool boundaries are unchanged in every mode.
- Execution tasks and read-only subagents are never interactive.
- The system prompt states the run's actual authority instead of always demanding implementation.

### D4 — Execution tasks on the subagent infrastructure (C2, after C1)

- **Separate tool, separate grant.**
  - `start_task` is registered only when `AgentOptions.Delegation` is `propose` or `isolated`. The default is `none`.
  - It is not a mode of `spawn_subagent`, because that tool is available to read-only runs. The grant is independent of the parent's `Policy`: a read-only conversation may start an isolated task only with this grant, and Core enforces it at tool registration and again at execution.
- **The child.** It is created with:
  - relation `execution`;
  - its **own owned worktree**, based on the parent workspace's committed HEAD;
  - `Policy = workspace`, with file tools path-confined to that worktree;
  - no `shell`, no MCP, no spawn tools;
  - work units seeded from the task;
  - verification through evaluator checks, which run as runtime processes from trusted config.

  Direct shell for a task child is out of the initial scope because Core cannot confine it. This is a restriction on the model's tools, not process isolation: trusted evaluator commands still run with the user's filesystem and network access, and a worktree is not an OS sandbox. A future shell capability requires a real sandbox and its own decision.
- **`propose`.** The tool records a proposal in Task State and returns `approval_required`. `StartProposedTask(parent, proposalID)` starts exactly that proposal. It runs under the parent's run lock and needs no model round-trip.
- **`isolated`.** The tool starts the child directly.
- **Ownership and lifecycle.**
  - Children obey the per-parent limit of 4 and run under their own run lock and worktree lease (ADR-0005).
  - `Cancel(parent)` does not cancel children. The host's "Stop" cancels the subtree (`SubagentTree` + `Cancel`).
  - Supervisor close interrupts them (P6), Reconcile marks them `interrupted`, and resuming is an explicit Start of the child.
  - Deleting the parent never destroys an execution task: ADR-0005 §6 refuses with `ErrSessionHasDependents` until the task is cleaned up or deleted on its own, so retained tasks keep their worktree, branch, results and provenance.
- **Result delivery.**
  - At the parent's Start, under its run lock and in the BeginRun transaction, Core appends one runtime row with origin `task_result` for each child whose terminal transcript seq is newer than the delivery cursor stored in the parent's Task State. The row carries status, final answer, open units, check verdicts and the handoff summary. The cursor advances in the same transaction.
  - That gives exactly-once delivery per terminal transition. A crash before commit delivers on the next Start.
  - The host may start the parent with no input when a child finishes, if the thread opts in.
- **Restated I10.** A child's authority is at most what the user granted the parent run (`Policy` + `Delegation`). Writes are confined to the child's own worktree. Applying to the main checkout is always a user action.

## Validation already run (2026-10-15, Core `2c5fe7b`)

P5–P7 are in the probe set. They show that:

- the parent ends `completed` while its child stays `running`;
- `Cancel` on an ended parent returns `false` and the child continues;
- the child's final answer is not added to the parent transcript;
- `Close` leaves the child `interrupted`;
- `workspace` exposes `write_file` and `apply_patch` but not `shell`.

## Validation required before C1/C2 implementation

1. **C1.** Deterministic loop tests for each case:
   - interactive with open units → `completed`, units unchanged, no correction row;
   - interactive with a workflow that cannot finish → today's continuation;
   - non-interactive → unchanged.

   Reasoning never enters Task State or `task_result` rows; this regression is part of the C1 suite.
2. **C2.** Deterministic tests:
   - `start_task` is absent from `Specs` and denied by `Execute` without the grant;
   - a child's write outside its worktree, including through a symlink, is rejected;
   - two Starts after one child finished deliver one row;
   - a crash between child terminal and parent Start delivers once;
   - `StartProposedTask` with an unknown or already-started proposal is refused.
3. **One model conformance probe on `local/deepseek-v4.1-flash` only.** Under `propose`, the agent answers plain questions without proposing tasks, and proposes when the user asks for a change. Advisory; not a release gate.

## Consequences

- One new inbox table and new Task State fields (0003). One new tool, two run options, one host operation (`StartProposedTask`) and one A7 origin.
- No new agent framework.
- Before M4.1 most threads resolve manually. This is intended: completion without evidence is the failure this decision avoids.

## Documentation impact

Design §4.4, §4.7, §4.9 and I10 are amended by reference in the design header; the full text lands with M3.1, M4.1, C1 and C2. Desktop §7 rows land with M3.3 and C3.
