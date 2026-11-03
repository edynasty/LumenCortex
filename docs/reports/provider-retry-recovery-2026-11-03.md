# Provider retry recovery (2026-11-03)

Branch `feat/provider-retry-resilience` from `ff5ccdb` (`fix/bench003-shell-timeout`). Local and mock tests only; no paid request. BENCH005, its frozen binary, adapter and proxy configuration are untouched.

## Problem

- Each adapter retried an availability error at most twice inside `Complete` (0.5 s, 1 s backoff, no jitter) and slept through a `Retry-After` of at most 5 s; a longer one was returned at once.
- The loop then failed over once per candidate; when every candidate failed, the run ended `interrupted`/`provider:availability`. A gateway outage of more than a few seconds therefore ended a long task.
- In-adapter retries were not admitted, recorded or logged by Core (TODO PROVIDER-006).

## Change

| Area | Change |
|---|---|
| `protocol.ProviderError` | `Reason` (`rate_limited`, `overloaded`, `server_error`, `timeout`, `conflict`, `transport`, `stream_incomplete`, `malformed_response`) and `Status`; the loop still branches on `Class` only. |
| Adapters (`openai`, `anthropic`) | One HTTP attempt per `Complete`; `MaxRetries`, `RetryBaseDelay`, `DisableRetries` removed. Retry-After is read by `provider/internal/retryafter`: `retry-after-ms`, then `Retry-After` seconds (fractions allowed) or HTTP date, capped at 24 h, never slept in the adapter. |
| Loop (`internal/agent/recovery.go`, `send.go`) | The failover chain runs first. When it ends without an answer and some candidate failed with availability (also when the chain ends in capacity), the step waits and sends again. Only availability-failed candidates are retried, rotating in failover order, each no earlier than its own Retry-After counted from its own failure; a candidate capacity rejected for good is skipped. Wait = backoff × jitter 75–100 % when a candidate is ready within it, else that candidate's remaining Retry-After. Each move is a replayable `failover.recovery` decision. Stops before sleeping if the wait would pass the outage window or reach the run's deadline, or no candidate is left; a cancel during the wait ends the run at once. |
| Config | `execution.retry {disabled, baseDelayMs=1000, maxDelayMs=30000, maxOutageSeconds}`; default outage: until the deadline, 1800 s without one. `AgentOptions.Retry` per run; CLI `LCX_DISABLE_RETRIES` now sets `retry.disabled`. No request, token or cost limit added. |
| Logs | `provider.failover` gains `failure` (reason code) and `status`; new `provider.retry`, `provider.retry_stopped` (`retry_disabled`, `no_candidate`, `outage_window`, `deadline`), `provider.recovered`. Journal keeps `failure`/`stop` (whitelisted; error text dropped); diagnostic log levels warn (failover, retry_stopped) and info (retry, recovered). Final error: `provider unavailable for <d>: N failed requests, M retries; stopped because …; last error: …`, wrapping the last availability `ProviderError`. |
| Deadline clock | `observeModel(total, request)`: backoff time counts as model time but no longer inflates the answer margin. |

Session and context: a retry re-sends the same window on the same session; each attempt is a new admitted request with its own envelope (`unknown:provider_error` when failed). A failed or cut response returns nothing to execute, so no tool runs twice (PROVIDER-005 guarantee unchanged).

Reference: OMP `packages/coding-agent/src/session/turn-recovery.ts` and `docs/non-compaction-retry-policy.md` (retry owned by the session, exponential backoff with jitter, Retry-After honoured, fallback before waiting). Credential rotation and model-fallback chains from OMP were not ported: LumenCortex already has failover candidates and the Owner forbids key changes.

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...` (all packages, `runtime` included): pass.
- Loop tests (`internal/agent/recovery_test.go`): consecutive 502 → 429(Retry-After) → cut stream recovered with one tool execution and identical transcripts; outage window stop; deadline stop without sleeping; cancel during backoff; auth/bad_request not retried; chain then rotation with a replayed `failover.recovery` decision; capacity at the end of an outage waited out; capacity-rejected candidate not resent; per-candidate Retry-After kept. Config copy isolation, journal projection and log level tests added.
- Mutation checks: first round 7/7 caught (Retry-After ignored, no sweep reset, no outage check, no deadline check, cancel ignored, no jitter, longest hint); review-fix round 5/5 caught (capacity end not waited out, exhaustion not recorded, readiness ignored, no rotation, readiness not aged); config clone fix: test fails without it.
- CLI smoke after the review fixes (built binary, fake upstream on 127.0.0.1, isolated HOME/XDG):

| Scenario | Result |
|---|---|
| 502, 429 `Retry-After: 1`, cut SSE with a tool call, tool call, 503, answer | `completed`, 6 requests, waits 192 ms / 1000 ms / 695 ms / 153 ms, marker `RAN` once, the cut call never ran |
| always 503, `LCX_DEADLINE=4s` | `interrupted`/`provider:availability`, 7 requests, 6 retries, `stop: deadline` (refused 938 ms wait at 3.9 s) |
| always 503, `LCX_DISABLE_RETRIES=1` | `interrupted` after 1 request, `stop: retry_disabled` |

## Independent review (2026-11-03)

Reviewer agent `RetryReview` on `fb40879`: PASS for retry mechanics, admission, tool duplication, adapter classification, Retry-After parsing, deadline/cancel and config/CLI compatibility. Findings fixed in the follow-up commit:

| ID | Finding | Fix |
|---|---|---|
| F1 P1 | A chain ending in capacity ended the run although an earlier candidate was only unavailable | Recovery when any candidate failed with availability |
| F2 P2 | The sweep reset re-sent a capacity-exhausted candidate | Only availability-failed, non-exhausted candidates are retried |
| F3 P2 | Shortest sweep hint released a candidate before its own Retry-After; hints never aged | Per-candidate absolute readiness |
| F4 P2 | `execution.retry` pointer shared between config copies | Cloned in `cloneFile` and `Effective` (budget too) |
| F5 P2 | Journal dropped `reason`/`stop`; events logged at debug | Fields `failure`/`stop` whitelisted; warn/info levels |

Found while fixing: the old sweep reset recorded no decision, so the step record's decision log could lead to a different model than the one that answered (VerifyStep). The `failover.recovery` decision closes that; tests now replay every step.

Recheck on `367a3db`: F1–F5 CLOSED; ReplayDecision/VerifyStep PASS. Fixed in the next commit:

| ID | Finding | Fix |
|---|---|---|
| F6 P2 | Recorded backoff truncated to whole ms: `baseDelayMs=maxDelayMs=1` gave 0 ms waits | Backoff rounded up like readiness (`ceilMs`); decision test for 750 µs |
| F7 P2 | Test required ≥100 ms remaining of a 120 ms Retry-After (scheduler-dependent) | Bound removed; the request-gap assertion proves the Retry-After |
| Trace (pre-existing, now reachable) | A first capacity rejection emitted only `window.capacity_retry`, which the trace builder ignored, so a later answer closed the wrong request span | `window.capacity_retry` closes the request span with `capacity`; test fails without the fix |

## Behaviour changes to note

- A run without a deadline now waits up to 30 min through an outage instead of ending within seconds; set `execution.retry.maxOutageSeconds` or `disabled` for the old behaviour.
- `execution.budget.maxModelRequests` counts every retry (each is admitted), as it already counted failovers.
- Health registry marks are unchanged (Retry-After or 30 s); within a step the candidate list is fixed, and recovery uses each candidate's own Retry-After instead of the registry.
- Event field: the failure reason code is `failure` (not `reason`), because the journal whitelist is global and `reason` carries free text in other events.

## Not done

- Not merged into `main` or any benchmark branch; BENCH005 keeps its frozen binary.
- Review was by the bundled reviewer agent (same model family as the implementer); the project `sol-*` agents named in the workspace rules are not present. A model-diverse review is still open before any merge.
