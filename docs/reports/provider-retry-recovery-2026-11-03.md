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
| Loop (`internal/agent/recovery.go`, `send.go`) | When a sweep of every candidate ends in availability, the step waits and sends again from the first candidate. Wait = max(exponential backoff × jitter 75–100 %, shortest Retry-After of the sweep). Stops before sleeping if the wait would pass the outage window or reach the run's deadline; a cancel during the wait ends the run at once. |
| Config | `execution.retry {disabled, baseDelayMs=1000, maxDelayMs=30000, maxOutageSeconds}`; default outage: until the deadline, 1800 s without one. `AgentOptions.Retry` per run; CLI `LCX_DISABLE_RETRIES` now sets `retry.disabled`. No request, token or cost limit added. |
| Logs | `provider.failover` gains `reason`/`status`; new `provider.retry`, `provider.retry_stopped` (`retry_disabled`, `outage_window`, `deadline`), `provider.recovered`. Final error: `provider unavailable for <d>: N failed requests, M retries; stopped because …; last error: …`, wrapping the last `ProviderError`. |
| Deadline clock | `observeModel(total, request)`: backoff time counts as model time but no longer inflates the answer margin. |

Session and context: a retry re-sends the same window on the same session; each attempt is a new admitted request with its own envelope (`unknown:provider_error` when failed). A failed or cut response returns nothing to execute, so no tool runs twice (PROVIDER-005 guarantee unchanged).

Reference: OMP `packages/coding-agent/src/session/turn-recovery.ts` and `docs/non-compaction-retry-policy.md` (retry owned by the session, exponential backoff with jitter, Retry-After honoured, fallback before waiting). Credential rotation and model-fallback chains from OMP were not ported: LumenCortex already has failover candidates and the Owner forbids key changes.

## Verification

- `go build ./...`, `go vet ./...`: pass.
- `go test ./...` (all packages): pass; `runtime` 203 s.
- New loop tests (`internal/agent/recovery_test.go`): consecutive 502 → 429(Retry-After) → cut stream recovered with one tool execution and identical transcripts; outage window stop with reason; deadline stop without sleeping; cancel during backoff; auth/bad_request not retried; sweep restarts at the first candidate.
- Mutation check: 7/7 caught (Retry-After ignored, no sweep reset, no outage check, no deadline check, cancel ignored, no jitter, longest instead of shortest hint).
- CLI smoke (built binary, fake upstream on 127.0.0.1, isolated HOME/XDG):

| Scenario | Result |
|---|---|
| 502, 429 `Retry-After: 1`, cut SSE with a tool call, tool call, 503, answer | `completed`, 6 requests, waits 194 ms / 1000 ms / 740 ms / 196 ms, marker `RAN` once, the cut call never ran |
| always 503, `LCX_DEADLINE=4s` | `interrupted`/`provider:availability` at 3.8 s, 7 requests, `stop: deadline` |
| always 503, `LCX_DISABLE_RETRIES=1` | `interrupted` after 1 request, `stop: retry_disabled` |

## Behaviour changes to note

- A run without a deadline now waits up to 30 min through an outage instead of ending within seconds; set `execution.retry.maxOutageSeconds` or `disabled` for the old behaviour.
- `execution.budget.maxModelRequests` counts every retry (each is admitted), as it already counted failovers.
- Health registry marks are unchanged (Retry-After or 30 s); within a step the candidate list is fixed, so a marked model is still retried by its own step.

## Not done

- Not merged into `main` or any benchmark branch; BENCH005 keeps its frozen binary.
- No independent model-diverse review: project `sol-*` agents are not present in this workspace; the bundled reviewer is the same model family as the implementer.
