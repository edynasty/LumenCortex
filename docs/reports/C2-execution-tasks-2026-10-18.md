# C2 Execution tasks and delegation (2026-10-18)

Status: **implemented and validated locally; not pushed.** Core `7a060af` (code, tests, ADR status, this report); this hash line is a follow-up commit. Desktop is unchanged (C3 is the paired Desktop item).

## Scope and authority

- Item: C2, ADR-0006 D4 ([ADR-0006](../adr/0006-task-context-resolution-and-conversational-execution.md)). Design §4.9 now states the contract (`../LumenCortex-architecture-audit/design/README.md`; that directory is not under version control).
- Baseline: Core `7883722` (C1 plus its shell-mutation correction, [C1 report](C1-interactive-2026-10-18.md)).
- Out of scope, not started: C3 (Desktop grant line, proposal and task cards, subtree Stop), M5.1, a host-side auto-Start of the parent when a child finishes (ADR D4 "may", opt-in, host-owned).

## Implemented behaviour

| Aspect | Behaviour | Code |
|---|---|---|
| Grant | `AgentOptions.Delegation`: `""`/`none` (default), `propose`, `isolated`; unknown values are refused before anything is written. Independent of `Policy`. Children never hold a grant. | `runtime/agent.go` `prepareRun`, `runtime/tasks.go` `normalizeDelegation` |
| Tool | `start_task {objective}` is registered only under a grant (and an allowlist that admits it). Without a grant a call is denied by the loop's tool-set check. `startTask` re-checks the grant; `isolated` without a `TaskStarter` counts as no grant. | `internal/agent/delegation.go`, `internal/agent/loop.go` |
| propose | Records `{id, objective, state: proposed}` in Task State `tasks`, committed with the tool row; returns `approval_required`. Limits: 100 tasks, objective ≤16 KiB. | `internal/taskstate/tasks.go` |
| StartProposedTask | `RunSupervisor.StartProposedTask(ctx, parent, taskID)` holds the parent's run lock (`ErrRunHeld` while it runs), starts exactly the stored objective, marks the task `started` with the child id and reopens the thread. Unknown → `ErrTaskProposalNotFound`; started → `ErrTaskProposalStarted`. A child created by an earlier call that failed before recording is found through its relation (`taskId` in relation metadata) and reused. | `runtime/tasks.go` |
| isolated | The tool starts the child (needs `RunSupervisor.Start`; `RunAgent` refuses `isolated`), then records it as `started` and reopens the thread. The task limit is checked before the child exists, so a started child is never left unrecorded by the limit. | `internal/agent/delegation.go`, `runtime/tasks.go` `taskStarter` |
| Child | Relation `execution`; its own owned worktree at the parent workspace's committed HEAD (`ErrExecutionTaskNeedsCommit` outside git or without a commit); `Policy workspace` (file tools path-confined, no `shell`); no MCP, spawn or delegation; never interactive; one work unit `task` listing every required check; model `subagentModel`, else the parent's pin. A failure before its first run starts removes the child, its worktree, branch and locks. | `runtime/tasks.go` `startExecutionTask`, `executionOptions`, `discardUnstartedTask` |
| Fixed authority on resume | Every Start of an execution child (`RunAgent` or `RunSupervisor.Start`) re-applies `workspace`/no MCP/no spawn/no delegation/non-interactive, whatever the host passes. | `runtime/tasks.go` `constrainExecutionChild` |
| Lifecycle | Per-parent limit 4 counts running children of both kinds, including ones recorded `running` by another process. `Cancel(parent)` reaches no child. `RunSupervisor.CancelSubtree(ctx, id)` cancels the session and every descendant this supervisor runs. Close and Reconcile are unchanged generic paths (children end `interrupted`). Deleting the parent is refused with `ErrSessionHasDependents` (ADR-0005 §6, existing). `SubagentTree` lists both kinds with `kind`. | `runtime/tasks.go`, `runtime/subagents.go`, `runtime/worktree_safety.go` |
| Delivery | At each parent Start, under its run lock: one `task_result` row (`protocol.TaskResultHeader` + JSON) per execution child in a terminal status (`completed`, `failed`, `interrupted`, `waiting_gate`) whose mark (newest transcript seq, status) differs from Task State `taskResults`. Rows are appended before the input and replace the resume row. Rows, cursor and the rest of the Start commit in the one `BeginRun` transaction. Row content: task id, status, error kind/message, final answer ≤4 KiB, open units (≤20), checks at the worktree's current state, handoff (branch, path, base, head, dirty files ≤20 + count, unapplied commits). An unreadable worktree or checks are reported in the row, not as a Start failure. | `runtime/tasks.go` `taskResults`, `runtime/taskstate.go` `startTaskState`, `internal/session/start.go` |
| Context | The Task State block lists the newest 10 tasks with their state. | `internal/taskstate/render.go` |

New observable surface: `protocol.OriginTaskResult`, `protocol.TaskResultHeader`; events `task.started`, `task.delivered`; checkpoint `task.started`. No schema migration: Task State fields are additive JSON; relations already existed.

### Implementation-level decisions (proposed, for owner review)

1. **Host resume cannot widen a child.** The ADR says "resuming is an explicit Start of the child" but not what options it may carry. Core re-applies the child's fixed authority on every Start of an `execution` child, otherwise I10 could be bypassed by the host passing `Policy: full`.
2. **A delivery mark is (newest seq, status), not seq alone.** A child resumed after `interrupted` that ends again yields a second row. That is a new terminal transition, and it is delivered exactly once.
3. **`waiting_gate` is terminal for delivery.** The child stopped and needs a human, and the parent conversation is where the user learns that.
4. **A started task reopens the parent's thread** (both paths). A new obligation exists whose outcome is not yet known.
5. **The concurrency limit of 4 is shared** by read-only subagents and execution children, and counts children running in another process.

## Changes

| File | Change |
|---|---|
| `internal/taskstate/tasks.go` (new), `state.go`, `render.go` | `Task`, `TaskMark`, propose/start/record/cursor operations, limits, rendering |
| `internal/agent/delegation.go` (new), `types.go`, `loop.go` | `Options.Delegation`, `TaskStarter`, `Loop.Tasks`, `start_task` spec and execution |
| `internal/session/start.go`, `store.go` | `RunStart.Results`; `BeginRun` returns the seqs of all appended rows; `NewestMessageSeq` |
| `protocol/agent.go` | `OriginTaskResult`, `TaskResultHeader` |
| `runtime/tasks.go` (new) | relation kinds, delegation constants, `StartProposedTask`, `CancelSubtree`, execution child start/discard, child constraint, `TaskResult` delivery |
| `runtime/agent.go`, `supervisor.go`, `taskstate.go` | grant validation, child constraint on both Start paths, task starter wiring, results in the Start transaction, delivery events |
| `runtime/subagents.go` | shared `activeChildren`/`childModelRef`; `SubagentNode.Kind`; tree includes execution children |
| `runtime/worktree_safety.go` | relation constant (no behaviour change) |
| `runtime/tasks_test.go` (new), `internal/session/gap_test.go` | C2 suite; `BeginRun` ordering test |
| `docs/adr/0006-…md`, design §4.9 | status and contract text |

## Tests (deterministic; real engine, SQLite and git)

| Test | Proves |
|---|---|
| `TestStartTaskNeedsADelegationGrant` | ADR validation 2a: without a grant `start_task` is not in the request's tools and a call is denied; nothing recorded or started. `RunAgent` with `isolated` and an unknown grant are refused and write no row |
| `TestProposedTaskRunsConfinedAndDeliversItsResultOnce` | `approval_required` + proposal in Task State, no child; unknown proposal refused (2e); start creates an `execution` child in its own owned worktree at HEAD with the exact objective; second start refused (2e); the child has `write_file` but no `shell`/`spawn_subagent`/`start_task`; writes via `../`, an absolute path, and a committed symlink to an outside directory are refused and nothing appears outside (2b); its write lands in its worktree, not the main checkout; deleting the parent → `ErrSessionHasDependents`; **engine closed and reopened between the child's terminal transition and the parent's Start** (2d), then two Starts → exactly one `task_result` row (2c) with task id, status, final answer, branch, base and dirty count; the parent model read it; cursor advanced; the child's opaque reasoning echo appears in no `task_result` row and nowhere in the database, WAL or SHM |
| `TestIsolatedTaskOutlivesTheParentAndStopsOnlyWithTheSubtree` | `isolated` starts the child from the tool; the child outlives the parent's run; `Cancel(parent)` returns false and leaves the child running; a running child is not delivered; `CancelSubtree` cancels exactly the child → one `interrupted` row; a host resume asking for `full` + `isolated` + interactive gets no `shell`/`start_task`/`spawn_subagent`; its new completed transition is delivered as a second row with its final answer |
| `TestBeginRunWritesInputIdentityAndRunningTogether` (extended) | results are appended before input, in order, and suppress the resume row |

Mutation checks, each reverted:

| Mutation | Failing tests |
|---|---|
| Delivery cursor not advanced (`MarkDelivered` removed) | both delivery tests (no row commits without its cursor) |
| `constrainExecutionChild` result ignored in `RunSupervisor.Start` | the isolated test: the resumed child gets `shell` and `spawn_subagent` |

| Check | Result |
|---|---|
| `go build ./...`, `go vet ./...`, `go test -count=1 ./...` (C2 tree) | pass (all 22 packages) |
| `go test -race` on the C2 tests, `BeginRun` and subagent tests | pass |
| Existing subagent, worktree-safety, resume, echo and reasoning tests | pass; the only edited existing test is `gap_test.go`, for the new `BeginRun` return value |

### Model probe (ADR validation 3, advisory): `local/deepseek-v4.1-flash` only

A throwaway program, outside the repositories, ran the real runtime with `Interactive: true`, `Delegation: propose` and `Policy: read-only`. The key was passed through an `{env:}` reference only. Each case ran 3 times:

| Case | Proposals | Result |
|---|---|---|
| question about README.md | 0, 0, 0 | answered |
| concept question (git worktree) | 0, 0, 0 | answered |
| design discussion ("do not change anything") | 0, 0, 0 | answered |
| explicit change request | 1, 1, 1 | proposed, said approval is pending |
| implicit change request (add a file) | 1, 1, 1 | proposed, said approval is pending |

15/15 as expected. The probe needed a tool allowlist because of the finding below.

## Finding: three tool schemas are rejected by the local endpoint (pre-existing, not C2) — fixed separately

- **Severity:** high for that endpoint. Every request carrying Core's default tool set fails with `provider:bad_request` (HTTP 400 "Invalid tool definition"), with or without `start_task`.
- **Evidence:** a one-tool-per-request bisection shows `git_diff`, `git_status` and `list_subagents` are rejected; all other tools, `start_task` included, are accepted. Those are the tools whose schemas have no required field. `internal/toolset/toolset.go` `obj(props, required ...string)` stores the nil variadic as `"required": null`, which is not valid JSON Schema.
- **Impact on C2:** an execution child's `workspace` tool set includes `git_diff`/`git_status`, so on this endpoint a child could not run.
- **Fix (separate commit, see below):** `obj` omits `required` when no property is required. `internal/agent/schema_test.go` `TestRequestToolSchemasAreValidJSONSchema` runs the loop with a real `toolset.Set` (policy `full`, LSP tools, subagent tools) under `propose` and `isolated`, takes the tools of the actual provider request, marshals each `Parameters` as the OpenAI adapter sends it, and fails on any `null` member, a non-object root, or a `required` that is not a non-empty list of unique declared properties (recursing through `properties` and `items`). It names `git_status`, `git_diff`, `list_subagents`, `start_task` and the LSP/task tools so a set that silently loses one fails too. Before the fix it fails with exactly the three schemas above; after it passes.

### Live run after the fix (`local/deepseek-v4.1-flash`, no allowlist)

A throwaway program outside the repositories (`/tmp/lc-c2-probe`, not committed) ran the real runtime through `RunSupervisor` with an HTTP transport that records each request's tool names, null-bearing schemas and HTTP status. Global config in a temporary `XDG_CONFIG_HOME`; key through `{env:}` only; one required check `greeting` (`grep -q 'hello from task' notes.txt`) in a fresh git repo. Input: "change notes.txt so that it contains a second line … hello from task", `Interactive`, no `ToolAllowlist`.

| Request tool set | Tools | Null-bearing schemas | HTTP |
|---|---|---|---|
| parent, `Policy full` + `propose` | 18 (all builtins incl. `shell`, `git_*`, subagent and task tools, `start_task`) | 0 | 200 |
| parent, `Policy read-only` + grant | 14 (incl. `git_diff`, `git_status`, `list_subagents`, `start_task`) | 0 | 200 |
| execution child (`workspace`) | 13 (incl. `git_diff`, `git_status`, `write_file`; no `shell`/spawn/`start_task`) | 0 | 200 |

No 4xx response in any run.

| Run | Result |
|---|---|
| `propose`, read-only parent | proposal `t1` with a self-contained objective, `approval_required`, no child; `StartProposedTask` → `execution` child in its own worktree at HEAD; a second `StartProposedTask` → "already started"; resolution stays `open`. Child `completed`: worktree `notes.txt` = `first line\nhello from task\n`, main checkout unchanged and clean. Next parent Start: one `task_result` row (`completed`, check `greeting` required/satisfied/`passed`/runtime, handoff branch/base/head, `dirtyFiles: [notes.txt]`, `unappliedCommits: 0`); the model reported it as done in the worktree, not in the main checkout. Third Start: still one row. Events `task.started`, `task.delivered` |
| `propose`, `Policy full` parent | 1 run: the model answered without proposing (it held write tools itself); 1 run: proposed, child completed the same way |
| `isolated`, read-only parent | 4 runs: 3 started the child from the tool call and gave the same result and single delivery as above; 1 answered with the plan without calling `start_task` (model choice; no error) |

After delivery the parent resolution stays `open` with obligation `checks`: the required check is not satisfied in the parent's own workspace, because the change is still only in the child's worktree. That is the D2 rule, not a defect.

| Check (schema fix tree) | Result |
|---|---|
| `TestRequestToolSchemasAreValidJSONSchema` with the old `obj` | fails: `git_diff`, `git_status`, `list_subagents` `$.required is null` |
| same with the fix | pass |
| `go build ./...`, `go vet ./...` | pass |
| `go test -count=1 ./...` | all packages pass except one run of `provider/openai` `TestIdleAndRequestTimeouts/request_cap_while_chunks_keep_arriving` (timing, under full-suite load); it passes 5/5 in isolation with and without the fix and does not touch tool schemas |

## Not covered

- Supervisor `Close` → children `interrupted`, and Reconcile of a crashed child, are the existing generic paths (P6 probe, existing reconcile tests). No C2-specific test re-proves them.
- The shared limit of 4 is exercised by the existing subagent limit test through the shared `activeChildren`. No test fills the limit with execution children.
- Runs held by another process are not reachable by `CancelSubtree`, as documented.

## Risks and open items

- **Publication:** local only. Desktop does not pin this commit. C3 must bump the pin and adapt to `SubagentNode.Kind` and the new options.
- **Owner review:** the five implementation-level decisions above were accepted by the owner (2026-10-18).
- **Evaluator commands are not sandboxed** (ADR D4): trusted checks still run with user filesystem and network access in the child's worktree.

## Next

C3 (Desktop): the grant line per thread, proposal cards with Start (`StartProposedTask`), task cards from `task_result` rows and `SubagentTree`, and subtree Stop (`CancelSubtree`). The `required: null` schemas are fixed and the real-model execution-child run is recorded above.
