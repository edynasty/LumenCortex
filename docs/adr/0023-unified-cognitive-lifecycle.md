# ADR-0023: Unified cognitive lifecycle and observed application

- **Status:** Accepted for the isolated completion candidate; merge/release approval remains with Owner.
- **Date:** 2026-11-13
- **Owners:** Astra, Cognitive Core implementation owner
- **Supersedes:** Candidate-only host-demo application and tool-end-as-use assumptions; no historical evidence is rewritten.
- **Superseded by:** none

## Context

The imported `91af861` candidate has scoped evidence, native graph revisions, privacy fences and conditional Experience APIs. Ordinary Agent runs did not traverse the Experience lifecycle; unrelated successful tools could be mistaken for strategy reuse. Graph retention, pinned dependencies and evidence-aware decisions needed a shared execution/state boundary.

## Decision

1. Reuse one Engine, Agent Loop, SQLite Store, evaluator, source graph, Governor and existing maintenance lease. No new service, database, scheduler or autonomous Agent.
2. Add small shared native fields: epistemic class, optional model confidence, pinned `NodeRef` dependencies, host `MemoryPolicy` and revision reason. Do not invent a universal memory object. Stored qualification is historical; applicability is computed for a reader now.
3. Offer bounded propose/recall/adopt/apply/assess tools only on cognition-enabled ordinary runs. Session/run identity is framework-bound. Host retains check approval, lifecycle, sharing and privacy authority.
4. Application is explicit and matches the complete original tool sequence and canonical argument digests. Conditions are checked at adoption and again before application/actions. Ordinary policy, workflow, hooks, budgets and timeouts still apply. Stored prose is never replayed automatically; exact-action reuse is the deliberate conservative boundary.
5. Persist admission and settled execution IDs separately from outcome evidence. A complete settled failed action is an observation; denied, interrupted, incomplete or unknown execution is not confirmed use. Only an independently executed approved outcome check determines success/failure feedback, and it is executed when the application's last action has settled: the observation is kept on the receipt and no later check replaces it.
6. Finalize proposals/feedback after the existing task terminal boundary. Recovery holds the existing session run lock, marks abandoned pending applications unknown and finishes memory settlement, never repeating actions. Memory errors remain separate from task results.
7. Host Governor Apply may change tiers, archive owned unprotected nodes and canonicalize equivalent evidence. Every batch is CAS/all-or-nothing; canonical targets cannot change within it. Restore checks current sources, time and dependencies and cannot revive invalidated identities. Branch/promote/epoch remain advisory because they introduce new claims.
8. Preserve historical versions and source/consumer privacy. Reads/mutation replies project only the reader's Uses. Dependency and decision exposure use `derived_origins`; all new content participates in existing privacy purge/recovery.

Application actions share the ordinary Workflow observer and hooks. Their exact execution ID/outcome, Task State and nested result provenance settle atomically before the next action; live authority is recomputed, background jobs are refused, and a human gate or denied transition stops the sequence. No time/tool/digest search may infer invocation identity. Generic Node proposals cannot create or rewrite canonical bindings; an unchanged inactive alias pin is retained history, not a precondition for withdrawal. `read_thread` Attention includes bounded conditional Experience locators and checked counterexample warnings without promoting them to facts.

## Invariants

- No model self-verification, permission increase, inferred execution, fake PASS or unknown→fact shortcut.
- Retrieval rank, access frequency, importance and confidence do not replace evidence.
- Equal supported conflicting claims remain conflict; evidence array order cannot decide truth.
- Low-frequency safety knowledge and counterexamples are protected; capacity pressure is explicit, not destructive LRU/TTL eviction.
- Old source/revision/fence cannot overwrite new state. Privacy deletion overrides history retention.
- No hidden model reasoning, credentials or raw command text is introduced as durable cognitive memory. Commands remain in the existing observable transcript; Experience records contain digests/locators.

## Alternatives considered

- **Store summaries and credit any later successful tool:** rejected; unrelated actions and pre-existing artifacts falsely appear as learning.
- **Automatic parameterized replay / arbitrary graph promotion:** rejected; the Framework cannot establish semantic equivalence or grant new action authority. A changed method needs a genuinely observed candidate and new checks.
- **Another learning loop/database or embedding service:** rejected; existing Go contracts support the required lifecycle and small lexical retrieval without duplicated ownership.
- **TTL/LRU deletion or lossy fact merging:** rejected; recency and similarity cannot justify deleting safety/negative evidence or dropping provenance.

## Persistence and compatibility

Migration 0017 adds bounded STRICT application, proposal, decision and reader-access records plus a native kind index after unchanged Hub 0013 and cognitive 0014–0016. Existing history is not backfilled into success. The normal migration runner supplies locking, backup and rollback. Missing current tables and historical branch collisions are refused before mutation. Downgrade refuses completion rows or completion-only native metadata rather than discard evidence. Production/user databases are not migrated by this task.

The final candidate stores per-action `outcome` instead of a success boolean. Older pre-freeze candidate receipts without that exact outcome remain unconfirmed; no historical feedback or receipt is rewritten to invent credit. This is a candidate JSON contract correction, not a production schema migration.

## Consequences and validation

Conditional learning, correction and governed memory are reachable through normal Agent execution. Exact-action matching, eight Uses per record and 64 KiB node bounds limit generalization and scale; no universal transfer or longitudinal benefit is claimed. Deterministic local providers drive real Engine/tools/checks/SQLite in integration tests and the public offline demo. Red/green findings, full build/vet/tests, affected Race, migration and platform results are recorded in the [completion report](../reports/cognitive-core-final-completion-2026-11-13.md), with independent status separate.

[中文](0023-unified-cognitive-lifecycle.zh-CN.md) · [Architecture](../cognitive-core/architecture.md) · [Memory lifecycle](../cognitive-core/memory-lifecycle.md).
