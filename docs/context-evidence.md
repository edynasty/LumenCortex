# Runtime context and evidence

`Engine.GetContextGraph(ctx, sessionID)` exposes a bounded, read-time neighborhood of the existing Go session store. It does not create Node graph tables, copy repository contents, or add a second memory database. The global legacy Governor is not enabled by this API.

## Source contracts

- At most 64 newest relevant events are examined and at most 128 nodes/links are returned. `truncated` and `prunedThrough` disclose incomplete history.
- `tool.end` is emitted only after the tool result transaction commits. Side-effect candidates resolve their execution record and answered request, matching session/run/tool and checking lineage/projection. Read-only/denied candidates require a matching committed transcript tool pair. Raw tool content is not copied to the graph.
- Worker results resolve existing Hub assignments, session relations, and verification. They remain execution outcomes, not independent semantic truth. Cross-session sources are rechecked before return and carried into `read_thread` provenance, including workers outside its legacy child display limit.
- References honor exclusions and never auto-accept suggestions. Notes reuse source/freshness checking and hide missing/withdrawn sources.
- Workflow, claim, verification, and delivery observations are derived from Task State, not event-supplied assertions. A graph read does not write facts or settle a gate. Verification fingerprints are checked; historical delivery is explicitly historical, never a fresh check of the filesystem.
- Model statements and successful tool responses are not verification evidence. `record_checked` describes the existence/identity of an authoritative record, not the truth of arbitrary prose.
- No raw logs, arguments, code, provider-private reasoning, or secret-bearing bodies are stored by this layer.

## Attention

Deterministic selection prioritizes unresolved verification/delivery, recent tool failures, accepted references, worker results and workflow constraints. Each item includes its selection reason, source reference, validation and freshness. At most eight items and 4096 serialized bytes are selected. There is no automatic full-log injection, propagation algorithm, or model call.

The existing `read_thread` tool includes only this bounded attention view, not the full graph. Retrieval emits `context.retrieval.started/completed/failed` and `attention.updated` metadata. Context is retrieved explicitly; existing request envelopes and derived edges remain the recovery/withdrawal authority.

## Verification

`runtime/context_graph_test.go` covers real Engine tool success/denial, forged candidates, stale verification, excluded/suggested references, bounds, state non-mutation and revocation. `internal/agent/context_provenance_test.go` covers source tracking for evidence workers outside the display window. Workspace evidence `raw/cognitive-public-smoke.*` exercises public APIs, actual file output, Engine close/reopen and grounded execution identity with a deterministic local provider (zero external model requests).

This is capability evidence, not a measured improvement in LLM task success or the deferred model-based ADR-0007 continuity evaluation.
