# ADR-0022: Observable and controllable execution platform

Status: implementation contract under Owner task LC-COGNITIVE-AGENT-EXECUTION-PLATFORM-001 (2026-11-08).

## Authority

Keep ADR-0005 worktree ownership, ADR-0008 bounded concurrency, ADR-0016 external state, ADR-0018 provenance/fences, ADR-0020 notebooks and ADR-0021 registry. Core remains the only execution owner. This adds capabilities to those owners, not alternative registries, supervisors, session stores or notebooks.

## Events

Extend the existing `pluginapi.Event` used by `runtime.Events`. Preserve `seq` (process-local live ordering), `at` and `data`; add schema/id/run/worker/task/correlation/causation/sensitivity/reliability and durable cursor/run sequence. A durable event is committed in the existing session SQLite database before live notification. Live output remains bounded/lossy and never waits for subscribers. Storage failure is latched, exposed and cancels affected active execution; it is never relabelled a durable event. Query/resume surfaces the failure.

Durable payload is a bounded content-free projection using the existing journal whitelist: IDs, counts, status, usage, timing and provenance references. Raw arguments/results are not a second durable content store; authorized details resolve the existing session transcript/artifacts. Private reasoning, scratchpads and credentials are neither event payload nor event-derived graph facts. Live text is bounded and redacted before subscription. Session-scoped history cascades on deletion and is inaccessible behind a session fence.

Per-session/run sequence allocation and append share a write transaction. A separate monotonically increasing database cursor supports pagination across runs. Retention is explicit: bounded session history, cursor-expiry response and oldest retained cursor, never silent gap-as-complete. High-frequency output is live only. No durable process snapshots or replay of side effects.

## Control and execution

Agent debugging uses a run-owned safe-boundary controller attached to the existing agent loop and supervisor. Pause does not suspend arbitrary goroutines or operating-system processes. An in-flight external operation completes/cancels under its own deadline; the next boundary pauses. Steps consume model/tool/turn boundaries without replay. Resume/step revalidate current permission/fences. Human control epochs reject stale Agent controls.

Program debugging is separate: `internal/debugger/dap` protocol transport, an Engine-owned manager with explicitly configured adapters, bounded sessions and the existing owned-process lifecycle. DAP expressions are not inherently read-only: default inspection resolves variable references without evaluating arbitrary target code; side-effect evaluation and attach require separate grants. Paths, source mappings, targets and process ownership are confined by the approved workspace execution boundary.

Hub extends existing subagent/task/worktree state and supervisor, with explicit DAG dependencies and inbox messages. Cross-worker data is message/artifact data, never a shared mutable kernel or a direct foreign-session write. Integration requires existing verification/worktree gates.

Persistent Eval uses the hardened Eval SPI/registry/manager. Linux untrusted execution requires verified hard controls and a host-produced versioned boundary assertion. A dedicated UID broker accepts only fixed typed operations under host policy, authenticates peer credentials and one-use session grants, and constructs bwrap arguments itself. Untrusted shell cannot inherit broker identity, socket or control FDs. Eval OFF remains unchanged; missing proof is UNKNOWN and admission refuses.

Lifecycle integration extends the existing `internal/verify.Registry` dispatcher and plugin host. Hooks may deny or narrow allowed fields, never widen permission or author workflow/verification facts. Context Graph/Attention consume bounded provenance-validated summaries, not raw logs. Delivery remains independently verified.

## Compatibility and review

Desktop remains embedded and calls typed Core APIs through its existing bridge; the broker is a narrow Linux privilege separation boundary, not a duplicate general runtime daemon. Original DAP design bytes and P0 evidence are preserved. Implementation phase commits record focused behavior evidence; final full suites and actual UI/adapter experiments validate the assembled candidate. No new model calls or publication are authorized.
