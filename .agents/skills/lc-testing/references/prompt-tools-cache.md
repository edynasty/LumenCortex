# Prompt, tools, provider wire and prompt cache

Load this when a change touches system-prompt text or assembly, tool names/descriptions/schemas/order, toolset composition or permissions, provider request/response encoding, cache markers, usage accounting or request layout. All checks here are **offline**: scripted providers, `httptest` SSE servers, captured request bodies. Never send a real model request to prove a prompt or cache property.

Seams to reuse (do not write a new fake):

| Seam | Where | Gives you |
|---|---|---|
| `scriptedProvider` (records `seen []ProviderRequest`) | `internal/agent/loop_test.go` | exact requests the loop sent, step by step |
| `scriptedProvider` / `recordingProvider` | `runtime/agent_test.go`, `runtime/agents_test.go` | Engine-level runs with captured requests |
| `bind` / `bindRef` / `binderFor` + private HOME and `test/b0…b63` catalog | `runtime/testutil_test.go` (`TestMain`) | inject a provider into a real Engine |
| `fakeAnthropic` (`httptest` SSE server, per-request bodies and headers) | `provider/anthropic/engine_test.go` | wire bytes, headers, stream edge cases |
| `budgetGolden` (absolute request-body golden) | `provider/anthropic/wire_test.go` | byte-level wire pin (uncached body) |
| Request envelopes (digest of what was sent) | `internal/agent/envelope_test.go`, `runtime/request_envelope_test.go` | proof the stored record equals the request |

## 1. Prompt

Invariants to test when prompt text or assembly changes:

1. **Required clauses survive.** Assert each required behavior clause by a short, stable phrase, per run purpose (task vs interactive, read-only vs full, child vs top-level): `internal/agent/interactive_test.go` `TestSystemPromptFollowsTheRunPurpose` is the pattern (phrase present/absent, `strings.Count == 1`). Structure tests plus a clause list, not a byte golden of the whole prompt (master plan EV-10 B): a byte golden must be re-pinned on every wording change and stops protecting anything.
2. **Permissions are stated correctly**: a read-only run is told what it may do and what it may not; the prompt does not name tools the run does not have (ADR-0019 §7). Pair with `runtime/prompt_test.go` `TestEffectivePromptReportsWithheldTools`, `TestEffectivePromptChildAuthority`.
3. **Report equals reality**: `TestEffectivePromptMatchesARealRun`, `TestEffectivePromptLayerOrderAndLabels`, `TestEffectivePromptWritesNothing`.
4. **No dynamic data in the stable prefix**: build the system prompt twice with different time, cwd, Task State, session id and note contents; the system bytes must be identical unless the change intends otherwise. Dynamic state belongs after the cache breakpoints.
5. **Agent/skill layering**: `runtime/agents_test.go` `TestAgentInstructionsAppendedAfterDefaultPrompt`; skills catalog lists descriptions, never bodies (`internal/skills/registry_test.go`).

Known gap (EVO-07, `OPUS_CORE_EVOLUTION_MASTER_PLAN.md` §4.1): no test pins the default prompt's general engineering rules, and the candidate's prompt rewrite (`468df7e`) dropped four of them. When you restore or change such rules, add the clause test in the same change. Do not restore a historical prompt wholesale (for example `b8ce9f2`) without a reviewed decision: it re-introduces TB2-specific text and breaks prefix stability.

## 2. Tools

| Property | How to test | Existing anchor |
|---|---|---|
| Name uniqueness | construction rejects duplicates (`duplicate tool` errors in `internal/toolset`); assert the sent `tools` array has unique names | `internal/agent/schema_test.go` `TestRequestToolSchemasAreValidJSONSchema` (collects names); `internal/mcp/registry_test.go` `TestAgentToolNameIsBoundedAndDeterministic` |
| JSON Schema validity | every sent tool's parameters is an object schema, `required` declared, no `null` | `TestRequestToolSchemasAreValidJSONSchema` |
| Argument validation | invalid args return a tool error the model can read; nothing executes | `internal/toolset/*_test.go` per tool |
| Permission boundary | withheld tools are absent from the request and refused if called anyway | `internal/agent/stepspecs_test.go` `TestStepToolsReportsWithheldTools`, `TestStepToolsDelegationGrantAndDenylist`; `runtime/agents_test.go` `TestAgentCeilingsCapTheGrant` |
| Serialization and digest | the envelope digest changes iff a model-visible field changes | `internal/agent/envelope_test.go` `TestToolSpecsDigestFollowsWhatTheModelSees`, `TestEnvelopeDescribesTheRequestThatWasSent` |
| Order stability | same toolset → byte-identical `tools` array across steps and runs (dedupe then sort, `internal/agent/stepspecs.go`). No dedicated test exists: when order logic changes, add one that builds the array twice from differently ordered inputs and compares bytes | indirectly `TestToolCatalogsMatchAFullPolicyRun` |
| Catalog agreement | report, run, `ToolSchemaCost` and capability registry agree | `runtime/prompt_test.go` `TestToolCatalogsMatchAFullPolicyRun`; `runtime/mcp_lifecycle_test.go` `TestToolSchemaCost` |
| Provider compatibility | the same tool definitions encode for each adapter | `provider/anthropic/wire_test.go`, `provider/openai/*_test.go` |
| Causality | every tool result answers a tool call in the same request; unpaired results are dropped, never sent alone | `internal/window/window_test.go` `TestGroupsUnpaired` |

## 3. Provider wire

- Use `httptest` SSE (`fakeAnthropic` pattern). Distinct cases, test the one you change: cut stream (no `message_stop` / `[DONE]` / `finish_reason`), in-band error event, HTTP 4xx/5xx/429 with and without `retry-after`, malformed tool-call arguments, empty content.
- Usage buckets: cache read, cache write and uncached input are separate; explicit `0` differs from missing (`protocol/usage.go` *(candidate)*; on `main` the provider packages own usage decoding). A missing usage record is `UNKNOWN`, never zero.
- Retries: assert attempt count, events and deadline bound; never sleep for a real backoff when a clock seam exists.
- Provider-private reasoning/echo stays opaque and provider-private; it is never persisted as task memory (`runtime/models_test.go` `TestHiddenReasoningIsNeverPersisted`, `runtime/request_envelope_test.go` `TestHiddenReasoningNeverReachesTheDatabaseThroughEnvelopes`).
- Captured real-traffic replays are env-gated (`provider/openai/malformed_dsml_replay_test.go`, `LCX_BENCH004_TRACE_ROOT`): without the variable they `SKIP` — report them as such, not as coverage.

## 4. Prompt cache and request layout

Request order today: System → Task State → transcript (`internal/window/window.go`). A Task State change invalidates the transcript prefix after it; this is the dominant cache constraint (master plan §4.3).

Offline prefix regression (the default method):

1. Run a scripted multi-step session with the capturing provider.
2. Serialize consecutive requests; assert (a) system and tools bytes identical across steps, (b) every earlier transcript message is byte-identical and in order in the next request, (c) where the first difference is, and that it is the intended one (new user/tool message, Task State slot).
3. For Anthropic `cache_control`: assert the marker positions (≤ 4: last tool, last non-empty system block, last two user messages) and that the body is byte-identical with caching off.
4. Resume: the first request after resume shares the same system+tools bytes; record whether the transcript prefix is preserved (today a resumed run is cold by design: receipt projection is off on resume, `internal/agent/receipt_notes.go` *(candidate)*).

Candidate anchors *(candidate `7c032ae`)*: `internal/agent/receipt_notes_test.go` `TestReceiptNotesProjectionUsesTypedHistory`, `TestReceiptsMatchNotesAndFailClosed`, `TestReceiptNotesProjectionRequiresCompleteFullTaskContext`; `internal/agent/receipt_notes_fable_*_test.go`; `internal/agent/cache_fallback_test.go` `TestStage2CacheFallback*`; `provider/anthropic/cache_test.go` `TestStage2CachePolicy*`, `TestStage2CacheDisabledBodyIsByteIdentical`; `internal/agent/cache_accounting_test.go`; reproduction and acceptance matrix in `docs/reports/r5-cache/CACHE_REGRESSION_TESTS.md`. On `main` these files do not exist; the ADR-0024 Context Space suite (`context-cognitive.md`) is the in-repo coverage.

The 103-request frozen replay needs data outside the repository (workspace `bench/evidence/…`, a tokenizer file) and an `-overlay` test (EVO-03): when that data is absent it is `NOT_RUN(evidence-missing)`, never PASS. Never claim a cache-hit rate from offline tests: they prove byte prefixes, not provider cache reads.

## 5. Report

For prompt/tool/cache changes add to the report: which clauses/properties were asserted, first-difference position for prefix checks, digest values before/after when a digest change is intended, and any golden diff (reviewed, explained). A deliberate prefix break (one-time cache invalidation) is stated as such.
