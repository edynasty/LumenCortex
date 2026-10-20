# ADR-0008: Configurable agent concurrency

- **Status:** Accepted (owner product decision 2026-10-20: both limits user-configurable in Core and Desktop Settings; design delegated to the architect)
- **Date:** 2026-10-20
- **Owners:** project owner
- **Supersedes:** the fixed `resource.Budget.MaxAgents` default of 3 and `runtime.MaxConcurrentSubagentsPerParent = 4`. Amends ADR-0006 (per-parent limit of 4)
- **Superseded by:** none

## Context

Before this change two hard-coded limits governed agent runs:

- every `RunSupervisor.Start` took a slot from `resource.Manager.AcquireAgent` (default 3 per Engine; only `runtime.Options.Budget.MaxAgents` could change it, and no host set it);
- `spawn_subagent` and `start_task` refused once a parent had 4 running children of either kind (`MaxConcurrentSubagentsPerParent`), counted across processes.

The defaults contradicted each other: a parent plus four children needs five slots, so one Desktop thread could run two children at most. Neither limit was visible or adjustable to a user, and the owner decided that both must be user-configurable.

What an agent run costs: one goroutine, a bounded request window (≤ 32 k-token default context), provider HTTP buffers, and (for execution children) one git worktree plus whatever shell commands it runs. The memory per run is small; the real cost is CPU, disk I/O and the provider's rate limits, which grow with the number of runs.

## Decision

1. **One configuration object in the global `lumencortex.json`:**

   ```json
   "concurrency": { "maxAgents": 4, "maxChildrenPerParent": 3 }
   ```

   `maxAgents` (1–16, default 4) bounds the agent runs one process executes at once: threads and children together. `maxChildrenPerParent` (1–8, default 3) bounds one session's running children of both kinds. The defaults let one thread use all of its child slots by itself (1 + 3 = 4). The schema rejects values outside the bounds and unknown keys.

2. **Global only.** Concurrency bounds the machine, not a project. A workspace file that sets it, trusted or not, gets a `global_only` warning and is ignored, so a cloned repository can never raise the user's limits.

3. **Read at every start, applied without a restart.** `RunSupervisor.Start` reads the effective limits before taking a slot; `spawn_subagent`/`start_task` read the per-parent limit before creating anything. A saved change therefore applies to the next start in every open Engine. A lower limit never stops a running agent: it only refuses new ones until enough have finished. `Engine.Health()` refreshes and reports the limit in force.

4. **Refuse at once; no queue.** A start beyond a limit fails immediately with the existing sentinels (`ErrAgentLimitExceeded`, `ErrSubagentLimitExceeded`), now carrying `(<running> running, limit <n>)`. For a child, the tool result tells the model to let a child finish (`wait_subagent` for a read-only one) and retry. A refused start writes nothing: no session status change, no child, no worktree, and a refused proposal stays `proposed`.

5. **Host override kept.** `runtime.Options.Budget.MaxAgents > 0` fixes the process limit of that Engine and wins over the configuration (embedded hosts, tests). Desktop and the CLI do not set it.

## Invariants

- The number of agent slots taken in one Engine never exceeds the limit in force when each slot was taken (`AcquireAgent` is one CAS loop; the limit is an atomic).
- A configuration change never cancels, pauses or reorders a running agent.
- A refused start leaves durable state unchanged (session status, relations, worktrees, Task State proposals).
- Workspace configuration cannot change concurrency.
- Cancellation, Supervisor `Close`, Reconcile and host resume are unchanged: a slot is released on every exit path of the run goroutine, exactly once.

## Alternatives considered

### Bounded queue of pending starts

A queued start either blocks its caller or needs a durable `queued` session status. Blocking a parent's tool call while the parent holds its own slot can deadlock (every slot held by a parent that waits for a child slot) and needs timeouts and fairness. A durable queue adds a lifecycle state that cancellation, Reconcile and Desktop must all learn. The model already has the tools to wait (`wait_subagent`, `list_subagents`) and a user sees the refusal at once. Rejected until a measured workload shows refusals that waiting would fix.

### Memory-based admission

`resource.Manager.Reserve` exists but no run reserves memory; per-run memory is small and not the scarce resource. Admitting by bytes would need per-run accounting the loop does not have. Rejected; agent count stays the admission unit.

### Workspace-level limits for trusted workspaces

A trusted project lowering its own limits is plausible, but it would make the effective limit depend on which workspace an Engine opened while slots are per process in Desktop. Rejected for clarity; revisit only with a concrete need.

### Reload limits only on Engine open or by an explicit host call

Saves a small file read per start but makes CLI hand edits invisible until restart and requires every host to remember a reload call. The start path already loads the configuration for model planning, so one more bounded read is negligible next to a model request.

## Consequences

### Positive

- One thread can use its full child budget with the defaults (previously two children).
- Users adjust both limits in Desktop Settings or the global file; changes apply to the next start.
- Refusals name the running count and the limit, for the user and the model.

### Negative / trade-offs

- Each start and each spawn reads the configuration once more (a bounded file read and schema validation).
- The global default rises from 3 to 4 concurrent runs; the per-parent default falls from 4 to 3 (the old 4 was unreachable under the old global 3).
- A host resume of an interrupted child (an explicit `Start` of that child) is bounded by `maxAgents` but not by `maxChildrenPerParent`, as before this change.

## Security and trust impact

No permission changes. A workspace cannot raise concurrency (global only). Upper bounds (16/8) cap the damage of a mistaken global value.

## Persistence / compatibility impact

No schema migration. Configuration files without `concurrency` keep working with the new defaults. `runtime.MaxConcurrentSubagentsPerParent` is removed (no consumer outside Core tests); `config.DefaultMaxChildrenPerParent` replaces it. Error sentinels and their `errors.Is` identities are unchanged; their messages gain a suffix.

## Validation plan

- `config`: `TestConcurrencyIsReadFromTheGlobalFileOnly`, `TestConcurrencyOutsideItsBoundsIsInvalid`.
- `runtime`: `TestSubagentControllerEnforcesConfiguredChildLimit` (configured limit; a raised limit admits the next child of a running parent), `TestRunSupervisorAppliesConfiguredMaxAgentsAtEveryStart` (live lowering keeps running agents, refuses new ones without writing, admits once below; host override wins), `TestExecutionChildrenAndSubagentsShareTheParentLimit` (both kinds share the default limit).
- `go test -count=1 ./...`, `go build ./...`, `go vet ./...`.

## Documentation impact

`docs/lumencortex.schema.json`, `docs/go-provider.md` (run ownership, configuration), ADR-0006 amendment, Desktop Settings report.
