# Go Scope, Time and Evidence — P0 contract

Go is the sole production Runtime. This contract now governs the unmerged 2026-11-13 completion candidate on main `00344b5`; inherited fixes are not silently promoted to main or independent PASS. [Current architecture](architecture.md) and [ADR-0023](../adr/0023-unified-cognitive-lifecycle.md) define lifecycle and execution additions. Node remains historical reference.

## Scope

`knowledge.Scope` distinguishes kind, owner, project, workspace, session, reader and source identities. `runtime.CognitiveScopeFor` derives authority from the Engine's existing Store binding and actual session workspace. No model permission string grants access.

- Global: same-owner explicit grant in the pure type contract; no P0 global persistent Store or implicit global inheritance.
- Project: shared only within exact project authority.
- Workspace: narrows to exact worktree identity; moving/copying a workspace does not transfer ownership.
- Session: retained after session completion but unavailable to another session.
- Reader: additionally binds the exact current run/consumer identity.
- Source: additionally requires an explicit source grant within its owner session.

Project → workspace → session → reader/source narrowing is permitted. Widening, source↔reader inheritance and cross-project export are refused. Historical revisions obey the latest scope narrowing. Derived records cannot broaden their source audience. Source/reader/privacy fences override all visibility and promotion. Privacy erasure takes priority over retained history. Expiration is a use-time condition, not an ownership change.

## Time

`Times` separates observed time, real-world valid interval, recorded time, verified time and invalidated time. Host assigns persistence/verification/invalidation timestamps. Valid time is half-open `[from, until)`; absent endpoints are unknown/unbounded. Equivalent UTC instants are equivalent regardless of display timezone. Future observations/verification are not fresh; not-yet-valid facts are Unknown; equality at the end is expired. A temporary future-valid condition does not mutate stored status.

Independent review correction (2026-11-11). Finding P0-REVIEW-F03: `PutCognitiveNode` accepted a caller-supplied `invalidatedAt` on a record it left active, at any past or future instant, and refused a revision that handed the stored instant back in another display zone. The first invalidation is now the recorded time of the revision that makes it (an invalid host revision or a failed verification); the Store refuses any other first value, compares later revisions by instant and keeps the stored form. `TestCognitiveInvalidationTimeIsFrameworkAssigned` covers it.

CAS revision order controls concurrency; wall-clock recency never chooses truth. Revalidation appends an observation/revision rather than rewriting the original. Explicit invalidation cannot be erased to revive the same identity. A full bitemporal database is unnecessary here: immutable revisions, explicit intervals, source identities and use-time projection distinguish history from current applicability.

## Evidence and promotion

`SourceRef` identifies host-issued source kind/id/version, originating session/run, sensitivity and provenance. `Evidence` adds ID/type/trust/verification, Scope/Times/Freshness, exact assertion/check/binding/spec/approval/result identity, support/refutation digests and invalidation reason.

Candidate → original Source → traceable Evidence → framework-verified Evidence → conditionally usable Belief are distinct states. Database insertion and model confidence cannot promote a proposal. Only a real closed evaluator episode at a stable fingerprint and current host-approved exact assertion binding attaches verified evidence. A failed qualified check is a counterexample; an error/unknown is not. An entity or diagnostic relation is not semantic evidence. Graph proposals cannot set verified fields or change an existing kind; a changed claim loses prior evidence authority.

Schema checks are version-bound; process checks are run-bound, same session/run/state, no intervening effect, and strictly within the five-minute reuse window. Negative clock age and boundary equality are not fresh. A postrun host verification has no invented agent-run ownership. Future tasks independently verify process-bound conditions and their own results.

Private sources cannot be widened to project/workspace/global. Conflicting exact propositions in the same scope and overlapping valid interval remain conflicts; distinct projects or disjoint intervals may coexist. Retrieval rank never establishes truth. Privacy deletion cannot revive an older PASS; unrelated surviving failed observations remain counterexamples. Content-free privacy epochs conservatively require pre-deletion PASS revalidation.

## Historical comparison, not parity maintenance

| Concept | Historical Node reference | Go production direction / P0 implementation |
|---|---|---|
| Claim | free-text nodes, sourceVersion, evidenceIds | host-issued SourceRef, typed exact equality Assertion and qualified evidence |
| Scope | historical optional fields / workspace-local graph | structured scopes; Engine reader identity; transactional fences and revision narrowing |
| Time | createdAt/observedAt/ttlMs conventions | five distinct times, immutable revisions, half-open validity and reader freshness |
| Authority | grade/trust labels and worker operations | model proposals separated from exact framework verification; no label-based promotion |
| Privacy | historical behavior only, not updated here | source/consumer fences, bounded secure-delete of all native revisions and dependencies |
| Persistence | legacy Node database, retained read-only | existing Go Engine SQLite migrations; rejects legacy Node tables; no shared format |
| Governor | historical Apply/Epoch implementation | Normal Go Store Analyze/Plan/Validate plus host-only CAS archive/tier/equivalent-consolidation; no autonomous Epoch or model self-approval |
| Experience | no new Node implementation | Go terminal extraction, exact-action independent application, checked feedback/revision and restart settlement |

No Node test proves this contract. New tests are Go and prefer real Engine APIs. Hidden reasoning, opaque provider echo, raw credentials and private absolute paths are not cognitive memory. Go Context Graph, Session, Workflow, Evidence and Store are reused without creating a second long-term memory database.

## Completion dependency and lifecycle contract

`NodeRef` pins a premise revision; `MemoryPolicy` is host-owned retention metadata, not evidence. Model confidence remains separate. Generic node writes, Governor batches, readers and assessments share scope/source/fence/CAS checks. Summaries cannot widen audience or discard original source lineage. Invalidation is immutable, restore does not grant truth, and privacy deletion overrides all retention.

`Application.Executed` requires every planned action's settled identity and a completed/failed receipt. Pending/partial/denied/interrupted sequences remain unknown. Actual tool failure may support a separately checked counterexample. Decision receipts contain only observable action/evidence state, never hidden reasoning.
