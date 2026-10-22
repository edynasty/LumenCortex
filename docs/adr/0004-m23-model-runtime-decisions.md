# ADR-0004: M2.3 model runtime cutover decisions

- **Status:** Accepted
- **Date:** 2026-10-14
- **Owners:** project owner (explicit approval of D1–D6)
- **Supersedes:** none; this records the M2.3 amendments to the target design
- **Superseded by:** none
- **Scope:** LumenCortex Go Core M2.3; Desktop M2.4 must consume these Core contracts

## Context

The independent M2.3/M2.4 architecture handoff found six gaps between the original S5 sketches and the actual Core/Desktop implementation. Core M2.3 implemented the proposed resolutions, and the independent audit accepted the implementation subject to owner ratification. The owner explicitly approved all six resolutions on 2026-10-14, before M2.3 publication.

Sources of evidence and implementation details:

- `../reports/M2.3-model-runtime-cutover.md`
- `../reports/M2.3-independent-audit.md`
- `../../../LumenCortex-architecture-audit/design/README.md` (sibling workspace path; authoritative target design)
- `../../../LumenCortex-architecture-audit/design/handoffs/M2.3-M2.4-architecture-handoff-2026-10-14.md` (sibling workspace path; API contract)

## Accepted decisions

| ID | Binding decision | Implementation boundary |
|---|---|---|
| D1 | Keep Governor's LLM curator opt-in through `lcx-go governor plan --curator`, resolving the configured default model via the ProviderFactory. The default plan remains deterministic. | `cmd/lcx-go/governor.go` |
| D2 | Log a `model.candidates` decision on every agent step, including the filtered candidates and reproducible inputs; enforce I7 replay and I14 trace reachability. | `internal/agent/policy.go`, runtime tests |
| D3 | Provider adapters classify protocol and transport failures and return `protocol.ProviderError` with the error class. Generic agent policy reads the class and run context without importing provider-specific errors. | `protocol/`, `provider/openai/`, `internal/agent/` |
| D4 | Send `max_tokens` only when explicitly requested in run options or configured as a model output limit; otherwise omit the wire parameter. Candidate filtering still reserves the effective output budget. Never scale the transmitted value by reasoning effort. | `config.ResolvedModel.OutputSet`, `internal/agent/send.go` |
| D5 | An OpenAI-compatible provider without `baseURL` or `endpoint` must be rejected before a request or run-start write. Never implicitly send its key or transcript to api.openai.com. | `provider/openai/client.go` |
| D6 | M2.3 may use only a bounded request-token estimator (`internal/window.Estimate`) for candidate capacity checks. The full window builder remains an M3.2 task. | `internal/window/` |

D7 (build required provider clients before the run transaction) was already decided in the handoff; this ADR leaves that decision unchanged.

## Invariants and constraints

- Session `model_ref` remains pinned across normal continuation and failover. An explicit user model change is the only normal re-pin path.
- Secrets and custom headers are scoped to their resolved provider; missing destinations fail closed.
- Hidden provider reasoning never becomes persistent state, recovery context, or decision-log input.
- Model-policy decisions are deterministic and verifiable from observable inputs; no legacy Decision Layer, Laya/Jev, or category router is reintroduced.
- Desktop M2.4 consumes Core's configuration, catalog, trust and model identity APIs. It does not replicate routing/merging semantics.
- Approval of D1–D6 does **not** expand M2.3 into the M3.1 effort escalation or M3.2 context/echo implementation.

## Alternatives and trade-offs

- D1 chooses an opt-in curator rather than removing it completely or invoking it by default.
- D3 centralizes provider-specific error classification at the adapter boundary rather than in the generic agent loop.
- D4 avoids a synthetic 4096-token wire cap when `limit.output` is missing.
- D5 removes an implicit convenience default to prevent credential and transcript misrouting.
- D6 accepts approximate token estimates until the full, independently scoped window builder lands.
- Existing accepted limitations are still tracked by the independent audit: adapter retry delay before failover, legacy Node test timing, and cross-host redirect/header behavior.

## Security, persistence and validation

- D5 is the direct credential-routing security boundary; D3 separates cancellation from retryable adapter timeout behavior.
- M2.3 migration 0002 adds `sessions.model_ref`; no additional migration is authorized by this ADR.
- Validation evidence: `docs/reports/M2.3-independent-audit.md` (Go tests, race, build, vet, Node baseline comparison, negative probes, mutation tests).
- Approval records an **architecture decision**, not a claim of successful release CI. Publication status is established separately from the actual remote commits and CI runs.

## Documentation and follow-up

- The sibling target design and handoff should identify D1–D6 as owner-approved.
- The implementation/audit reports retain historical evidence and link to this approval.
- Next milestone is Desktop M2.4. Its acceptance must include `ErrInvalidEffort` error mapping and pinning behavior on Continue.

## Amendment 2026-10-22: the reasoning format is declared per model (PROVIDER-001)

- **Status:** accepted in phase 0, work package P0-A. D1–D6 are unchanged; this amendment records how
  one provider-neutral key, `reasoning.format`, selects a thinking encoding.
- **Evidence:** probes of the owner's Anthropic-protocol gateway on 2026-10-22 with the owner's own
  credentials (no key or model id is recorded here). Models that require adaptive thinking answer
  `thinking: {type: "enabled", budget_tokens}` with HTTP 400 ("requires adaptive thinking") and answer
  `thinking: {type: "disabled"}` with HTTP 400 as well; `thinking: {type: "adaptive"}` with
  `output_config: {effort}` and omitting `thinking` are accepted. Models of the same family differ:
  both encodings are accepted by some, only adaptive by others. Every probed Claude model rejects a
  non-default `temperature` ("temperature is deprecated"), with or without thinking. `xhigh` is
  accepted as an effort but is not one of LC's five levels (it is since 2026-10-23: the ladder
  became none, minimal, low, medium, high, xhigh, max, and adaptive thinking sends xhigh as-is).
- **Decision:** the thinking encoding is a property of the model, declared by the existing
  `reasoning.format` key. `anthropic-messages` accepts `""` (the package default: the budget encoding,
  so requests of a model that declares no format stay byte-identical to earlier builds) and
  `"adaptive"` (levels low/medium/high/max become `thinking: {type: "adaptive"}` plus
  `output_config: {effort}`; no level sends neither parameter; `temperature` is never sent and a
  forced `tool_choice` is downgraded to `auto`). `none` is not a format of that package: an adaptive
  model has no off switch, so `config` refuses `none` in an adaptive model's `efforts`. A format a
  package does not implement is `config.ErrInvalidConfig` naming the key, and the adapter refuses it
  as a bad request before sending, because silently ignoring the key is what sent the request that
  produced the 400. `config.ReasoningFormats(pkg)` publishes the accepted values per package, the
  package default first, for hosts that build an editor.
- **Alternatives rejected:** inferring the encoding from the model id or its family. The evidence
  above shows the requirement differs within one family, a gateway may alias ids, and a new model
  would silently receive the wrong encoding. Defaulting every Claude model to adaptive was rejected
  too: it changes the thinking semantics of models that already work with budgets and would silently
  drop a level's budget guarantee. Adding `xhigh` as a sixth effort level is deferred (PROVIDER-003):
  it changes the effort enum, the chat-completions mapping, `ClampEffort` and the Desktop effort
  picker together.
- **Boundary:** `provider/anthropic` (request building, client, factory), `config` (format table,
  merged-catalog validation, resolved default), `docs/go-provider.md` and the
  `reasoning.format` enum of `docs/lumencortex.schema.json`. Task State, persistence, routing,
  failover and reasoning echo are untouched.
- **Validation:** the budget encoding is pinned by a golden test whose bytes were captured from the
  adapter before the change (streamed and not); table tests cover adaptive × {"", none, low, medium,
  high, max} with a request temperature and a forced tool choice; a client test shows the format
  reaching the wire on both paths; config tests cover unknown formats, `none` in adaptive `efforts`,
  the accepted configurations and a workspace that changes the package the format is checked
  against. No real model endpoint was called by the tests.
