# ADR-0018: Request provenance, derived origins and fences (schema v6)

- **Status:** Proposed (implemented by T1-S1; contract frozen only after the independent freeze review)
- **Date:** 2026-10-25
- **Owners:** project maintainers
- **Supersedes:** none
- **Superseded by:** none

## Context

The continuing-cognition design (`LumenCortex-Cognitive-Design-20261023`, execution plan §3) needs durable,
body-free answers to four questions that Core cannot answer today:

1. Which provider requests may have been sent, and with which visible parts?
2. Which side-effect tool executions were admitted, and did they finish?
3. Which sources were revoked or privacy-fenced, so that nothing new may be derived from them?
4. Which check definitions did the host approve?

Core has no run identity, writes no pre-send record, and a crash between "request sent" and "answer
stored" leaves no trace. T1-S1 freezes the schema (migration `0006`) and the `internal/session` persistence
contract that T1-S2…S7, T3 and T4 build on. It adds no behaviour to the send loop, the Auto judge, the
Governor or tool dispatch.

## Decision

### Schema (migration 0006, user_version 6)

Four new tables and six new `eval_results` columns. No fifth table.

| Object | Owner | Foreign key |
|---|---|---|
| `request_envelopes` | consumer Session | `session_id -> sessions ON DELETE CASCADE` |
| `derived_origins` | consumer Session | `session_id -> sessions ON DELETE CASCADE` |
| `source_fences` | host | none |
| `check_approvals` | host | none |
| `eval_results` + `episode_id`, `pre_fingerprint`, `post_fingerprint`, `spec_digest`, `approval_epoch`, `episode_status` | Session | unchanged |

- Only the consumer side cascades. Every source reference (source session/seq, path+blob, check result)
  is a plain column, so deleting a source leaves the consumer's edge in place; it resolves to "source
  missing" later instead of turning into an edge-less row that looks clean.
- `source_fences` and `check_approvals` have no foreign key, so a tombstone or an approval outlives every
  Session.
- The tables are created without `IF NOT EXISTS`. A database that already holds one of the names is
  refused: the runner rolls the migration back and `user_version` stays 5.
- Nothing is backfilled. Legacy sessions have no envelope and no edge; legacy `eval_results` rows have a NULL
  `episode_id` and never satisfy a gate. No legacy message acquires a complete lineage.
- `lcx-go db downgrade --to 5` drops the four tables and the six columns (backup first). Provenance and
  approval records are lost by design. Refusing the downgrade while a fence is active belongs to T3-S1.

### Request machine

```text
admitted ──► answered      main: AppendAnswered;  other purposes: SettleRequest (+ bounded decision)
admitted ──► unknown       the request may have been sent and no answer was stored
admitted ──► failed        certainly never sent (Client.Complete not called), including "fenced"
```

`prepared` exists only in memory (the CHECK excludes it). `answered`, `failed` and `unknown` are terminal;
every other pair, including self-transitions and any move out of a terminal state, is `ErrInvalidTransition`.
The Store enforces this with a precondition read and a conditional `UPDATE ... WHERE state = 'admitted'`
whose row count must be one. `purpose` is validated in Go (`main`, `auto_effort`), not by CHECK, so R1b can
add a purpose without rebuilding the table.

### Execution machine (`derived_origins.kind = 'execution'`)

```text
admitted ──► completed | failed | outcome_unknown      CommitToolResult
admitted ──► outcome_unknown                           Reconcile
```

All three results are terminal. `lineage_complete` starts at 1 (0 when the pre-state was unavailable) and is
only lowered: by `coverage != complete`, `outcome_unknown`, a cut edge list, a fence, or Reconcile.

### Tool tiers and snapshots (loop side, T1-S4)

`internal/agent/execution.go` classifies each call before it runs (plan T1-S4, frozen table):

| Tier | Tools | Before the tool runs |
|---|---|---|
| admission + pre-state | `write_file`, `replace_in_file`, `apply_patch` (blobs of the target paths); `shell` and every `mcp__*` whose effective permission is `exec` (bounded `git status` + `rev-parse HEAD`) | pre ref, then `AdmitExecution` |
| admission only | other `mcp__*`, `start_task`, `spawn_subagent` | `AdmitExecution` without a pre ref |
| none | read-only and in-memory Task State tools | nothing; the row commits through `CommitToolResult` |

An MCP tool's tier comes from the effective permission on its spec (server `readOnlyHint` or the ADR-0013
user override, resolved at registration); a spec without one is `exec`. Snapshots (`internal/repository/
snapshot.go`) run outside every transaction, only read (`GIT_OPTIONAL_LOCKS=0` on every git process, no
`write-tree` or `hash-object -w`), and store refs, never content. Each snapshot phase is cut at 5 s; a phase
that runs out, like any pre-state that cannot be read, does not block the tool: the admission says
`pre_unavailable` and the post coverage `unavailable`. The tool runs only after `AdmitExecution` returned nil;
`ErrFenced` ends the run revoked and anything else fails closed, with nothing run. After the tool returns,
the post-state is taken and `CommitToolResult` stores the row, the post record and one file edge
`<path>@<blob>` ← the issuing request per changed path. Outcome: OK → `completed`, not OK → `failed`; a tool
error → `outcome_unknown` (it may have acted), except a file tool whose targets the post-state shows
unchanged. A cancelled call keeps the existing semantics (no tool row) and stays `admitted` until Reconcile
settles it at the next start. A shell edge attributes every change in the work tree during the call,
including another writer's; a moved HEAD, a cut status or more than `MaxEdgesPerCommit` paths make it
`partial`.

### Message-level edges, user rewrites and child sessions (T1-S5)

Edges of message-level bodies use `kind = 'message'` or `'child_session'` and name locators, never text:
`ts:<kind>:<id>@sha256:<hex>` is one version of a Task State entry (note, ref, unit, task; the digest is
that of the entry's stored JSON), `session:<id>` a child session, `session:<id>@<seq>` a child as of its
transcript seq, `tool:<requestId>:<callId>` a tool row, `task_result:<id>@<seq>` a delivered task result.

| Write | Edge | Source | Origin |
|---|---|---|---|
| a Task State tool call writes or changes an entry | entry version | the issuing request | `agent_generated` |
| `start_task` / `spawn_subagent` starts a child | `child_session` | the issuing request | `agent_generated` |
| `wait_subagent` / `list_subagents` reports children | the tool row | each child (`session:<id>@<newest seq>`) | `agent_generated` |
| the user rewrites an agent note or takes over an agent reference suggestion | new entry version | the replaced version (`message`) | `user_decision` |
| the user starts a proposed task | `child_session` | the proposal entry | `agent_generated` |
| a task result is delivered with its cursor | `task_result:<id>@<seq>` | the child as of `<seq>` | `agent_generated` |

A Task State version's edges travel on `TaskStateWrite.Edges` and commit in the version's transaction
(`PutTaskState`, `AppendMessageTaskState`, the Start version of `BeginRun`, and `CommitToolResult`, where
they join the tool row's edges in one list, fence-checked and cut at `MaxEdgesPerCommit` together). They
are validated before the transaction opens. A note or reference the user writes from scratch, and the
brief, have no edge: pasted text cannot be traced. The final answer is the answered main row
(`answered_seq`).

Lineage propagates forward inside the store (`inheritedLineage`): an edge from a `message` source is
complete only when the same session records at least one edge to that ref and all of them are complete, so
an entry written before provenance existed makes every later rewrite incomplete; an edge from a `session`
source is complete only when that session's newest answered main request at or before the seq in
`derived_from` is complete. A user rewrite keeps the user's wording but not a user's authority: the entry
records `from` (the replaced version) and moves the agent's cites to `review`; the Task State block shows
it as `[user edit of agent text, needs review]`, recall ranks it as agent text, and freshness is computed
on the review cites, so a changed source still marks it stale.

### Store API (`internal/session`, frozen signatures)

```go
AdmitRequest(ctx, RequestAdmission) (RequestEnvelope, error)
AppendAnswered(ctx, sessionID, requestID string, protocol.Message) (seq int64, err error)
SettleRequest(ctx, requestID string, Settlement) error
AdmitExecution(ctx, ExecutionAdmission) error
CommitToolResult(ctx, ToolCommit) (seq, taskStateVersion int64, err error)
Request(ctx, requestID string) (RequestEnvelope, error)     // read
Execution(ctx, executionID string) (ExecutionRecord, error) // read
Reconcile(ctx, sessionID string) (inserted int, err error)  // extended, signature unchanged
sessionFence(ctx, rowQuerier, sessionID) (epoch int64, fenced bool, err error) // unexported, in-transaction
SourceScopeRef(kind, ref string) string                                       // exported scope-key builder
```

Derived edges are written only as part of `CommitToolResult`; there is no bare insert.

### Fences (read side only in T1-S1)

A fence is any `source_fences` row with `scope_kind = 'session'` and `scope_ref = <session id>`; a source
fence has `scope_kind = 'source'` and `scope_ref = SourceScopeRef(source_kind, source_ref)`. A cleaned fence
still fences: cleaning removes bodies, not the fact of revocation. T1-S1 writes no fence; T3-S1 owns that.
The read paths fail closed:

- `AdmitRequest` on a fenced session writes a `failed` envelope (reason `fenced`, `fence_epoch` set) and
  returns `ErrFenced`; `AdmitExecution` writes nothing and returns `ErrFenced`.
- `AppendAnswered` on a session fenced after admission stores no message, settles the envelope `unknown`
  (reason `fenced_before_answer`), commits that and returns `ErrFenced`.
- `CommitToolResult` with a fenced session or a fenced edge source stores no message, Task State or edge,
  settles the execution (lineage 0), commits that and returns `ErrFenced`. The transcript then ends in a
  tool-call gap that Reconcile closes with the interrupted-tool row.

So a revoked source cannot be revived by a new record. T3-S1 may refine the fenced branches; it cannot loosen
the terminal states.

### Reconcile

`Store.Reconcile` gains one step in its existing transaction: `admitted` envelopes of the session become
`unknown` (reason `restart_without_answer`), and `admitted` executions become `outcome_unknown` with
`lineage_complete = 0`. It selects only `admitted` rows, so terminal rows are untouched and a second run
changes nothing (not even `updated_at`). Nothing is executed or resent. A late `AppendAnswered` or
`CommitToolResult` for a recovered row is `ErrInvalidTransition`.

### Transaction discipline

Every write goes through one helper: one pinned connection, `BEGIN IMMEDIATE`, a callback that receives only
that connection, `COMMIT` without the caller's cancellation. With `SetMaxOpenConns(1)` a nested Store call
would wait for the connection it holds, so callbacks never call a Store method or `s.db`. Digests, snapshots,
refs and git state are computed by the caller before the call; nothing inside a transaction touches files,
git, the network or a model. A busy database beyond `busy_timeout` (5 s) is a storage failure the caller
surfaces; nothing is retried.

### What is stored, and what is not

Ids, sequence ranges, epochs, enumerated states and `sha256:<64 hex>` digests. Validators reject prose:
envelope refs match `[A-Za-z0-9._:/@#+=%~-]` (≤ 96 bytes), ids match `[A-Za-z0-9._:-]` (≤ 128), reasons are
short machine codes `[a-z][a-z0-9_.-]{0,63}` (provider error text can echo a request, so it is refused), and
a decision is a flat object of short strings, numbers and booleans. A derived-edge source ref may hold a
workspace path or blob id, so it is printable UTF-8 without control characters, ≤ 512 bytes; it is a locator,
never content. No message
text, tool arguments, provider echo or hidden reasoning is stored. Bounds (code and DDL agree; a test keeps
them equal):

| Constant | Value |
|---|---|
| `MaxEnvelopeRefs` | 128 (longer lists are cut and the envelope is `lineage_complete = 0`) |
| `MaxEnvelopeRefBytes` | 96 |
| `MaxRefsJSONBytes` | 16384 |
| `MaxEdgesPerCommit` | 64 (more are cut and everything stored is `lineage_complete = 0`) |
| `MaxEdgeRefBytes` | 512 |
| `MaxIDBytes` / `MaxReasonBytes` / `MaxDecisionBytes` | 128 / 200 / 256 |

### Run id

`runtime.prepareRun` draws a 128-bit random hex id after `Reconcile` and before the Auto judge, passes it as
`agent.Options.RunID`, and never writes it to `sessions`. `request_envelopes.run_id` and
`derived_origins.run_id` are nullable; legacy rows have NULL. "Same run" means the same Start in the same
process; a restart, takeover or new Start is a new run.

### Column choices beyond the plan's list

`request_envelopes.decision_json` (bounded flat object, answered only) holds the Auto judge's `{"level": ...}`
decision. `derived_origins.support_status` is `unverified|verified|invalidated` and is `unverified` for every
row the Store writes; promotion belongs to later slices. `check_approvals` carries per-kind NOT NULL CHECKs
(a `constraint_binding` needs its constraint, check, spec and binding digests).

## Invariants

- Exactly four new tables; the cascade runs only from `sessions` to consumer-owned rows.
- A request is `admitted` before it can be sent, and an admitted request is never `failed` by recovery.
- Terminal states never change; a conditional UPDATE backs every transition.
- `unknown`/`outcome_unknown` are never rewritten, and nothing is replayed.
- `lineage_complete` is never raised after a row is written.
- No body, echo or reasoning in any new column.
- 0006 never uses `IF NOT EXISTS` for these four tables.

## Alternatives considered

### Triggers that make terminal states immutable

Rejected. The privacy cleanup (T3-S4) must still clear digests of terminal rows, and a trigger would block
it. Immutability of the state column is enforced in the Store with conditional updates.

### Foreign keys from edges to their sources

Rejected. Deleting a source Session would cascade the consumer's edge away and silently turn a dependent into
a row without provenance.

### Persisting `prepared`

Rejected. It would add a write per request that carries no information the `admitted` row lacks.

## Consequences

### Positive

- Crash recovery of requests and side-effect executions is a deterministic, idempotent step of the existing
  `Reconcile`.
- Later slices depend on a stable Store surface and a snapshot-tested DDL.

### Negative / trade-offs

- Each provider request and each admitted tool call gains one short write transaction (T1-S2/S4).
- `downgrade` to 5 discards provenance and approval records.
- The fenced branches fail closed and drop answers a fence arrived too late for; T3 decides the audit policy.

## Security and trust impact

Adds a fail-closed admission read and body-free records. It stores no credentials, no hidden reasoning and no
provider echo, and it adds no tool, permission or network path.

## Persistence / compatibility impact

SQLite `user_version` 5 → 6 through the existing runner (exclusive lock, versioned backup, one transaction
with `user_version`). A v5 binary refuses a v6 database with `ErrSchemaTooNew`. `lcx-go db downgrade --to 5`
restores the exact v5 schema (golden-file test). Public Core API is unchanged; `agent.Options.RunID` is an
internal field.

## Validation plan

`internal/migrate/provenance_test.go` (upgrade from every version, round trip, collisions, atomicity, schema
snapshot, constraints) and `internal/session/provenance_test.go` (every transition pair, atomicity, fences,
Reconcile idempotence, deletion semantics, no bodies, self-lock detection under concurrent reads),
`runtime/run_id_test.go`. See `docs/reports/cognition/t1-s1-schema-20261025.md`.

Tool tiers (T1-S4): `internal/agent/execution_test.go` (tier table, admission before execution,
outcomes, fences, cancellation, bounded snapshots, writes per call), `internal/repository/snapshot_test.go`
(read-only snapshots, index untouched), `runtime/execution_test.go` (every tier through the real Engine with
a stdio MCP server, edge cap, concurrent `git add`, 10k-file latency) and
`runtime/execution_crash_unix_test.go` (SIGKILL matrix). See `docs/reports/cognition/t1-s4-20261025.md`.

Message-level edges (T1-S5): `internal/session/derived_test.go` (version and edges commit together or not
at all, 40-rewrite chain, lineage inheritance from unrecorded entries and children, joint cut),
`internal/taskstate/provenance_test.go` (rewrite marks, entry diffs) and `runtime/derived_test.go` (the
launder sequence, spawn/wait, proposed and direct tasks through the real Engine). See
`docs/reports/cognition/t1-s5-20261025.md`.

## Documentation impact

`docs/data-model.md` (SQLite persistence, schema version); the T1-S1 report.
