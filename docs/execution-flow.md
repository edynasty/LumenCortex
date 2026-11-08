# Agent execution flow

This document describes the runtime logic of one autonomous Agent run, including Workflow Contracts, cognitive retrieval, tool execution, mutation, persistence, cancellation, and resume.

## End-to-end logic

<p align="center">
  <img src="assets/architecture/agent-execution-flow.webp" alt="LumenCortex agent execution flow" width="100%">
</p>

_Maintainable topology source: [`diagrams/agent-execution-flow.mmd`](diagrams/agent-execution-flow.mmd)._

Source: `docs/diagrams/agent-execution-flow.mmd`.

## 1. Start or resume

A run begins with either:

- a new user goal, or
- an existing Session ID.

A new Session stores the goal, provider/model, messages, runtime options, and optional Workflow definition.

A resumed Session restores:

- complete durable messages,
- global step numbering,
- cumulative usage,
- context history,
- recent observation IDs,
- promotion history,
- Workflow snapshot if present.

If a resumed Workflow definition is supplied and differs from the definition stored in the Session, the run fails closed instead of silently switching contracts.

## 2. Human Gate check

Before spending another model request, the runtime checks whether the current Workflow Action is waiting for human approval.

If so:

```text
status = waiting_gate
persist Session
return control to caller
do not make another LLM request
```

Approval is an explicit state mutation:

```bash
lcx workflow approve SESSION_ID GATE_ID --actor NAME
```

The approval becomes a Fact with human provenance.

## 3. Derive focus

The Agent derives the current focus from:

- the stable Session goal,
- current step,
- recent tool calls,
- paths/queries/commands used by recent tools,
- current Workflow Action when active.

This focus is a retrieval/attention input, not a durable truth claim.

## 4. Candidate retrieval

The normal retrieval path uses:

1. symbol index,
2. SQLite FTS5,
3. recent observation/history seeds,
4. graph neighborhood propagation.

Embedding retrieval is planned as an optional additional recall channel.

The purpose of retrieval is to avoid full-graph lexical scanning on every step.

## 5. Move Attention Light

Attention combines relevance and graph structure to produce a finite Active Subgraph.

Important distinction:

- persistent graph size may continue growing,
- Active Subgraph size is bounded,
- recent chat history is separately bounded by the Working-Set Pager.

This is the core long-task invariant.

## 6. Active Promotion

When selected detail becomes dense or repeatedly useful, the Promotion Controller can create a smaller parent abstraction.

Promotion is non-destructive:

```text
detail A ─┐
detail B ─┼─> parent abstraction
detail C ─┘

children remain available for drill-down
```

Promotion changes cognitive granularity; it is not equivalent to deleting history.

## 7. Build the model working set

A model request contains a bounded set:

```text
system policy
+ current cognitive Active Subgraph
+ active Workflow Contract summary
+ stable user goal
+ recent complete assistant/tool rounds
+ current allowed tool schemas
```

The full durable Session is not replayed wholesale.

## 8. Tool Working Set

When a Workflow is active, the tool set is the intersection of:

```text
Agent --tools allowlist
        ∩
current Workflow Action allowedTools
```

The provider therefore sees only the legal current tool schemas.

This is only the first enforcement point.

## 9. Execution-time enforcement

Every individual tool call is checked again immediately before execution.

This matters when one assistant turn contains multiple tool calls:

```text
call A
  ↓
Outcome writes Fact
  ↓
Route transitions Action
  ↓
call B is checked against the NEW Action
```

A tool that became illegal after call A is denied even though the model requested it earlier in the same turn.

## 10. Tool result and cognitive observation

Tool results are returned to the model and may also become durable cognitive observation nodes.

Typical trust:

- shell/runtime observation → `runtime_verified`,
- repository file observation → `repo_trusted`,
- model-only reasoning → `model_inferred`.

Workflow state and cognitive evidence are updated independently.

## 11. Workflow Outcome evaluation

After a tool result, the active Action evaluates its Outcomes.

Example:

```json
{
  "when": {
    "all": [
      { "tool": "bash" },
      { "arg": "command", "contains": "test" },
      { "result": "exitCode", "equals": 0 }
    ]
  },
  "set": {
    "tests.passed": true
  }
}
```

The Fact stores provenance in `factSources`.

## 12. Route and Gate evaluation

Facts may make an Action complete and select a Route to the next Action.

Condition Gates are evaluated automatically.

Human Gates pause the Session.

Automatic Route loops have a bounded transition guard and fail closed rather than silently spinning forever.

## 13. Workspace mutation and re-ingestion

If a successful tool reports workspace mutation and auto-ingest is enabled:

1. workspace reality is re-ingested,
2. changed source evidence is detected,
3. dependent cognition can become stale,
4. the search index is incrementally synchronized.

The current architecture does not yet provide whole-run worktree rollback for arbitrary shell mutations.

## 14. Completion Gate

A model final answer is not sufficient evidence when a Workflow is active.

The final answer is accepted only when:

```text
current Action is terminal
AND
completeWhen is satisfied
AND
all Gates are satisfied
```

Otherwise the runtime:

1. persists the premature assistant answer,
2. appends a corrective Workflow message,
3. emits `workflow.blocked_final`,
4. continues the Agent loop.

## 15. Persist and finish

On successful completion:

- Session status becomes `completed`,
- final response and usage are saved,
- Workflow state is saved,
- the task may be recorded into the cognitive graph,
- an optional Cognitive Commit can be created.

## Cancellation

One-shot Agent and Chat bridge `SIGINT` / `SIGTERM` to an `AbortSignal`.

TUI uses the same cancellation concept with `Ctrl+C`.

Cancellation behavior:

```text
signal
  ↓
provider / shell abort
  ↓
Session status = interrupted
  ↓
error metadata + usage persisted
  ↓
Session remains resumable
```

## Parallel and Subagent execution

Focused Subagents use durable Sessions.

Parallel write-capable work is guarded because real Git worktree isolation is not yet implemented. Multiple mutating tasks require explicit unsafe parallel mode or serialization.

## Failure modes

| Failure | Runtime behavior |
|---|---|
| provider retryable error | bounded retry/backoff |
| malformed/truncated tool-call JSON | retry with expanded output budget where applicable |
| empty assistant turn | bounded recovery attempt |
| unknown/out-of-working-set tool | deny |
| Workflow-illegal tool | deny |
| premature Workflow completion | reject final and continue |
| human gate | persist `waiting_gate`, stop LLM requests |
| unsafe Workflow path | fail closed |
| automatic Route cycle | fail closed |
| cancellation | persist `interrupted` |
| max steps | persist `max_steps`, throw resumable error |

## Go Unified Execution Runtime (opt-in)

The Go Engine exposes versioned `evalapi` contracts backed by `evalruntime` Python,
JavaScript and registered out-of-process workers. Host-configured executables are
required; nothing installs dependencies or enables network access automatically.
`AgentOptions.EvalEnabled` / `LCX_EVAL_ENABLED=1` opt in. Existing runs remain on
the old tool catalog when disabled. `lcx-go eval runtimes` discovers configured
workers; `lcx-go eval serve` provides the local JSON request interface.

Each actor owns a separate persistent kernel and filesystem subtree. Cross-actor
sharing is an explicit, expiring read-only observer grant for results and artifacts;
no actor may execute in or control another actor's heap. The removed EXECUTOR role
is rejected. Tool callbacks use the owning run's effective permissions and schema;
nested effects use the same provenance, approval invalidation and transaction settlement as direct
tools. Interrupt can retain acknowledged state; timeout/crash marks it lost and
never automatically replays uncertain code. macOS denies worker subprocesses and
monitors RSS rather than claiming a hard memory limit.

Linux fails closed unless two host prerequisites hold. (1) Resource isolation: the
user's systemd manager must be reachable (`XDG_RUNTIME_DIR`, session bus) and
delegate the `memory` and `pids` controllers; each worker runs in its own cgroup
v2 leaf with `memory.max`, `memory.swap.max=0` and `pids.max`, and Reset/restart
create a new leaf. Without it no runtime is offered. (2) Bubblewrap with user
namespaces: on Ubuntu 24.04 `kernel.apparmor_restrict_unprivileged_userns=1`
denies them unless an AppArmor profile grants `userns` to the exact bwrap
binary. That profile is an exception for every caller of that path, so grant it
to a dedicated root-owned copy used only by LumenCortex (for example
`/opt/lumencortex/bin/bwrap`, profile `flags=(unconfined) { userns, }` attached
to that path) rather than to `/usr/bin/bwrap`, keep the global restriction on,
and point the runtime at it with `EvalOptions.Bubblewrap` /
`LCX_EVAL_BUBBLEWRAP`. A named bwrap must be an absolute, non-symlink regular
file owned by root and not group/other-writable, or startup fails. The profile
adds no file or network confinement of its own; containment comes from bwrap's
namespaces, the private `/proc`, `/run` and `/tmp`, and the cgroup.

An eval call closes its callback capability before its result is settled: active
callbacks are cancelled and drained, queued callbacks are denied, and no callback
can update the run's spend or journal after the loop resumes.

Optional semantic verification and one independent review run before completion.
Delivery and execution do not imply semantic correctness. Invalid review output
blocks acceptance while retaining usage; its record survives the final gate write.
Schema10 adds `semantic` evaluator results without rewriting historical rows;
downgrade to schema9 refuses atomically while such rows exist.

A review applies to its workspace fingerprint. A changed, known fingerprint
requires fresh host semantic evidence for the repair; the old review remains in
history and is not rerun. Missing fingerprints cannot bypass a failed verdict.

Semantic checks must explicitly declare `files`: list every file supplying expected
truth, or use `files: []` only when the command embeds its own expectation. Approved
oracle content is SHA-256-bound even for ignored/untracked files; changed content
invalidates approval. The runtime does not infer indirect oracle reads. Workspace
fingerprints cover content, not timestamps; exceeding 8192 files or 16 MiB makes
confirmation and reuse unavailable rather than trusting incomplete evidence.
Saving configuration preserves explicit empty `files: []`, so a self-contained
semantic check keeps its oracle declaration and approval digest after a save.

Python startup resolves host-selected installation metadata, including an exact
macOS framework interpreter/library, and probes it inside the sandbox. No manifest
grants its executable's parent directory. Reset clears private home/tmp state;
closed-kernel artifacts remain readable. Python errors preserve observable partial
assignments. JavaScript successful declarations persist; a new lexical declaration
in a throwing cell does not persist, while prior/global state mutations can remain.

Execution notebooks reuse the versioned Notebook store with bounded nbformat4.5
JSON. Import/read never executes cells; explicit replay closes every live kernel
of the notebook's languages and recomputes from clean state, so a replay after a
reset or a cancel starts on a fresh session. A run, a reset, an interrupt and a
close act on the kernel that is alive — the newest one by creation time — never on
the kernel with the highest generation, because generation counts the restarts of
one kernel and does not order two. One run per notebook executes at a time: a
second run is refused instead of queued, and the run in flight is Core's state,
which a host reads (ExecutionNotebookRuns) so a route switch or a reload cannot
start a duplicate. The agent's notebook tools and the working-notebook API reject
the reserved execution namespace before lookup, even before a notebook exists;
existing execution IDs are refused too. Desktop consumes these Core APIs directly.

