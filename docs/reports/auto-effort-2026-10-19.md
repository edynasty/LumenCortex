# Auto reasoning effort

- **Date:** 2026-10-19
- **Status:** implemented, validated locally; real-endpoint probe recorded below
- **Scope:** LumenCortex Core only (`config`, `internal/agent`, `runtime`, `cmd/lcx-go`, schema, docs)
- **Baseline:** `003ebd8` (`feat(config): file-backed credential store for {secret:NAME} references`)

## What changed

`auto` is now an accepted effort request in `AgentOptions.Effort` and `effort.default`. One Start
(one user turn) resolves it to a concrete level (`none|low|medium|high|max`) under the session run
lock and before the Start transaction, so a cancelled judge call writes nothing. The resolved level
becomes that run's default: `effort.escalation` still raises it while the run is stuck, and
`ClampEffort` still fits it to each candidate's `reasoning.efforts`. An explicit concrete override
still wins; `auto` is an override value like any other.

### Configuration

```json
{
  "effort": {
    "default": "auto",
    "auto": {
      "model": "local/judge",
      "max": "high",
      "timeoutSeconds": 8
    }
  }
}
```

- `model` is optional. Absent (or its client cannot be built) means the heuristic classifier only.
- `max` defaults to `high`; it is the ceiling on the classifier's result.
- `timeoutSeconds` defaults to 8 and is bounded to 1..60.
- An untrusted workspace may name only a model of a global provider as `effort.auto.model`, the
  same rule as `subagentModel` and `failover`; otherwise the judge model is dropped with an
  `untrusted_override` warning and the heuristic applies.
- `effort.auto.model` outside the merged catalog is a configuration error, like any model ref.

### Classifier

The judge sees the turn's new input plus the previous substantive user request and the last
assistant reply, each truncated to 2000 characters. It is one non-streaming, tool-less request
through the normal provider factory (both `openai-compatible` and `anthropic-messages`), asking for
`{"level":"none|low|medium|high|max"}` with tolerant parsing (first JSON object, else a bare level
word). The call is bounded by `timeoutSeconds` and the run context; it is not a transcript row, not
a step, and writes only the `effort.auto` decision. A cancelled run cancels the judge and leaves no
new row or running session.

A judge failure, timeout, unusable answer or absent model falls back to the deterministic
heuristic. The decision records `source` (`judge|heuristic`) and `fallbackReason` as a class only
(`no_model`, `timeout`, `unparsable`, `error`, or the adapter's error class). It never records the
user text, assistant text or judge reply.

A resumed turn with no new input reuses the level its earlier run recorded (`reused: true`) instead
of calling the judge again. The latest step record is read under the same run lock.

### Heuristic rules

| Signal | Contribution |
| --- | --- |
| length ≥ 600 characters | +1 |
| length ≥ 1500 characters | +2 instead of +1 |
| a fenced code block | +1 |
| a stack trace / compiler-error marker | +1 |
| a file reference (`path/to/file.go`, `*.ts`, …) | +1 |
| numbered or bulleted items | +1 step |
| a sequence word (`then`, `after that`, `first`, …) | +1 step |
| ≥ 2 distinct imperative verbs | +1 step |
| ≥ 4 distinct imperative verbs | +2 steps instead of +1 |
| 3 or more steps | +2 |
| ≥ 3 distinct imperative verbs | +2 |
| question-only (ends with `?`, ≤ 240 characters, no code/stack/file/list/imperative signal) | −1 |
| short continuation (`continue`, `go on`, `proceed`, …) | the previous request's level |

`score ≥ 3` → `high`, `score ≥ 1` → `medium`, otherwise `low`. The heuristic never returns `none`
or `max`, so it stays conservative.

### Ceiling and per-model fitting

`result = min(level, effort.auto.max)`, then `ClampEffort` per candidate. If a candidate's non-empty
`reasoning.efforts` list has no level at or below the ceiling, no reasoning parameter is sent to it
and the step logs `effort.auto_unavailable`. A model with no `efforts` list keeps the level.
Because `ClampEffort` raises a request to the lowest listed level not below it, an escalated level
can still be fitted above the ceiling when the list also contains a level at or below it; the
ceiling bounds the classifier, not the existing per-model fitting rule.

### Decision log

- `effort.auto` (first step record of the run): inputs `source`, `fallbackReason`, `inputLength`,
  `ceiling`, `judgeModel`, `reused`; output `level`.
- `effort.auto_unavailable` (per request): the candidate offers nothing at or below the ceiling;
  `sent` is `""`.
- Replay: `effort.auto` checks invariants (known source, non-negative input length, concrete level
  at or below the ceiling) instead of recomputing, because the classifier input is not persisted.
  The other effort rules replay exactly as before.

## Files

- `config/types.go`, `config/merge.go`, `config/load.go`: `EffortAuto`, `AutoEffort`, defaults,
  trust/merge and `Effective().Auto`.
- `docs/lumencortex.schema.json`: `effortRequest` (`auto`), `effort.auto`, updated descriptions.
- `internal/agent/auto.go`: judge prompt/parsing, heuristic, auto decision, reuse.
- `internal/agent/policy.go`: `ValidEffortRequest`, `CappedAutoEffort`, auto-unavailable rule,
  replay.
- `internal/agent/send.go`, `internal/agent/types.go`: ceiling threaded to the per-request rule.
- `runtime/auto.go`: transcript context, judge wiring, resume reuse.
- `runtime/agent.go`, `runtime/models.go`: resolution before the Start transaction and the auto plan.
- `internal/session/store.go`: `LatestStep` for resume reuse.
- `cmd/lcx-go/main.go`: `LCX_EFFORT`.
- `docs/go-provider.md`: auto effort section and config/trust updates.

## Tests

Focused suites were run with `go test -count=1`:

- `./config/...` — schema fixtures (`auto-effort`, invalid auto max/timeout/model), defaults,
  untrusted/trusted workspace judge model, unknown auto model, `IsLevel`.
- `./internal/agent/...` — heuristic table, tolerant judge parsing, judge happy path, timeout,
  cancellation, unparsable/error fallbacks, no tools, auto-unavailable/ceiling/clamping, escalation
  of an auto level, decision replay and forged decisions, reuse, no text in decisions.
- `./runtime/...` — engine-level judge/heuristic/ceiling/override/unavailable/failure/reuse/config
  default tests through the real Engine and fake providers; real `openai-compatible` and
  `anthropic-messages` clients against `httptest` servers returning every level.
- `./cmd/lcx-go/...` — `LCX_EFFORT=auto`.

Full validation was run in one chain from the Core checkout:

```bash
go build ./... && go vet ./... && go test -count=1 ./...
```

All packages passed (`cmd/lcx-go`, `config`, `internal/agent`, `runtime`, and the rest of the
module). The focused suites listed above also passed on their own. The real probe is recorded
below.

## Real probe

Scratch git workspace `/tmp/lc-autoeffort-ws` with its own `lumencortex.json` (a workspace
override; the user's global `~/.config/lumencortex/lumencortex.json` was not modified). The global
configuration supplies the `local` provider and its `{secret:omp-local}` credential; the workspace
file only names model refs and the auto policy:

```json
{
  "model": "local/deepseek-v4.1-flash",
  "effort": {
    "default": "auto",
    "auto": {
      "model": "local/deepseek-v4.1-flash",
      "max": "high",
      "timeoutSeconds": 20
    }
  }
}
```

Commands (from the Core checkout; no keys are printed):

```bash
go build -o /tmp/lcx-go ./cmd/lcx-go
cd /tmp/lc-autoeffort-ws
LCX_EFFORT=auto LCX_MAX_STEPS=1 /tmp/lcx-go agent "What is 2+2? Answer in one short sentence."
LCX_EFFORT=auto LCX_MAX_STEPS=1 /tmp/lcx-go agent "Implement a new HTTP endpoint, then add request validation, then write table-driven tests, then run the build and fix any failures. Keep the change scoped."
LCX_EFFORT=auto LCX_MAX_STEPS=1 /tmp/lcx-go agent "Refactor the authentication module across this repository: first map every caller, then migrate the session store to the new token format, update the handler tests, fix the failing build, and verify with the race detector."
sqlite3 .lumencortex/lumencortex.db "SELECT json FROM agent_steps WHERE session_id='<id>' ORDER BY step;" | jq '.decisions[] | select(.kind=="effort")'
```

Results (the endpoint was reachable; the main model is `local/deepseek-v4.1-flash` with
`reasoning.efforts: ["low","medium","high","max"]`):

| Prompt | `effort.auto` source | Fallback | Resolved | Sent |
| --- | --- | --- | --- | --- |
| `What is 2+2? …` (session `session_eb57b60feace5fc0d7a0417d`) | `judge` | — | `none` | `low` (clamped; the model has no `none`) |
| `Implement a new HTTP endpoint, then …` (session `session_c4889a4b52007ad925c522f3`) | `judge` | — | `high` | `high` |
| `Refactor the authentication module …` (session `session_902785485e91749540951913`) | `heuristic` | `unparsable` | `high` | `high` |

The trivial and hard turns resolved different levels, and the log names the deciding source. The
third run is a real fallback: the judge's reply had no readable level, so the heuristic decided
`high`. The step records carry `inputLength` 42/155/222, `ceiling: "high"` and
`judgeModel: "local/deepseek-v4.1-flash"`; no user prompt text is in the step JSON
(`instr(json,'What is 2+2') = 0`), and the judge prompt is not a transcript row (0 rows matching
its system text). The hard runs ended `interrupted` at `MaxSteps=1` after recording the first step;
that is a probe bound, not an effort failure.

## Limits

- The heuristic is deliberately conservative and only returns `low|medium|high`; it cannot express
  `none` or `max`.
- The judge sees at most 2000 characters of each of the three inputs.
- The decision log intentionally cannot reproduce the classification; it records invariants only.
- The judge call is not part of the step's usage accounting (it is a separate provider request).
- No new persistence or schema migration: the resolved level lives only in the step record's
  decision log, and a resumed turn reuses it from there.
