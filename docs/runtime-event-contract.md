# Runtime event contract

Schema version 1 extends `pluginapi.Event`, shared by `runtime.Events` and plugins. `id` identifies an event, `seq` is process-local live delivery order, `sequence` is monotonic within `(sessionId,runId)`, and `cursor` is the database-wide durable cursor. `at` is the UTC timestamp and `data` is the payload; run/parent/worker/task/phase/correlation/causation fields bind execution identity. These are observations, never workflow facts or permission grants.

## Reliability

- `durable`: lifecycle metadata commits to the existing session SQLite database before live notification. The live copy may hold bounded sanitized details; the durable copy is content-free, using the existing journal projection. Raw arguments/results are resolved from their existing authorized transcript/artifact owner, never copied into a second content store.
- `live`: bounded in-memory output. Slow readers lose oldest notifications; `EventStats.Dropped` counts deliveries lost. No output stream is inserted into durable history.
- `unavailable`: a lifecycle commit failed. The bus exposes `PersistenceFailures`, latches an error, cancels affected active execution, and refuses future run preparation/history claims until reopen. A cursor is not issued for that event.
- Diagnostic records use the existing rotating diagnostic log. Sensitive live details are session-scoped, redacted and bounded before subscription. Private provider reasoning and scratchpads are removed, not persisted.

## Read and resume

`QueryRuntimeEvents(ctx, RuntimeEventQuery)` requires a Session ID; filters support Run, Worker, Task, type, text and `after` cursor. Limit defaults to 200, caps at 1000. `ResumeRuntimeEvents` reads the next page with identical semantics. `GetExecutionTimeline` uses the same query. `GetRunSnapshot` exposes current Core state and the high watermark, not a reconstructed replay state.

Subscribe first with `SubscribeRuntimeEvents(ctx,sessionID,buffer)`, then query. Deduplicate by event ID/cursor; handle notifications already present in the page. Subscribe buffers cap at 4096. Cancellation closes the stream. Query checks session existence and revocation; live authorization is checked before forwarding. A missing/revoked Session does not disclose other Session events.

Each Session retains at most 10,000 lifecycle metadata rows. Pages report `prunedThrough` and `highWatermark`; a nonzero cursor older than retained history returns `ErrEventCursorExpired`, requiring a fresh snapshot. An empty filtered page advances only through its captured high watermark. Event deletion cascades with the Session and uses the existing deletion/purge path. Migration `0011` and its down migration are handled by `internal/migrate`; no second database or schema authority.

## Security and recovery

Core is the only event writer. Desktop/plugins cannot append authoritative events. A returned notification never authorizes a tool, marks a task completed or satisfies a verifier. Core restart retains lifecycle history but not ephemeral process state; a new Run gets a new ID. Debugger replay reads history and never re-executes a non-idempotent tool.
