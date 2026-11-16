# Cognitive Core v1.0, v1.1 and Cognitive Evolution Roadmap

## Design baseline

The v1.0 capability baseline is the isolated integration of `8c30f75` on `integ/cognitive-core-v1-20261116`, not main or a Core SemVer release. The Owner selected verification and documentation closure without further architecture expansion or migration 0018. This roadmap records future work, not authorization to execute it. [中文](roadmap.zh-CN.md) · [Integration report](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.md).

## v1.0 — freeze the verified procedural boundary

Retain the existing Scope/Time/Evidence, source fences, CAS, observed application, qualified feedback, normal Go SQLite Governor and host retention paths. Experience is a Verified Procedural Macro, with exact actions, eight lifetime Uses and a 64 KiB node. Preserve unknown/conflict/failure history; do not equate engineering completion with autonomous learning benefit.

Accepted limits are V-D1 (one consumer withdrawal makes the shared Experience unavailable), V-002 (tier changes still stale a pin; concurrent adoption may return CAS), and V-003 (some plan rejections occur only in Apply). `cognition_candidates` and `cognition_nodes` share authority but not a merged lifecycle. Early unreleased-candidate test stores are not converted. These are current contracts, not hidden unfinished v1.0 work.

## v1.1 — explicit, separately authorized integration improvements

| Future item | Problem / reason | Existing implementation point | Acceptance before claiming it |
|---|---|---|---|
| Experience Start locators | A model may not discover when to recall a useful procedure | Existing `BeginRun.Notices` plus `NoticeSources`; bounded origin-qualified locators | Real Start/tool use with provenance, revoked-source exclusion and budget behavior; no new schema or Context Space dependency. |
| Assessment coverage | Query mode omits standalone negative/abstraction records | Existing native selection and assessment | Relevant negative/unknown records are visible without expanding authority or hiding incomplete coverage. |
| Two-record-family view | Maintenance candidates and native nodes have separate reads/lifecycles | Shared assertions/check evidence, distinct storage adapters | Explicit provenance and conflict semantics; never silently promote candidates into facts. No second memory store. |
| V-002/V-003 refinement | False stale after tiering and late fail-closed plan rejection | Existing dependency predicates / Governor validation | Change only after selected scope; preserve CAS, canonical invariants and all rejection boundaries. |

Context Space is a separate branch and responsibility after Cognitive Core integration. Both planes stay in the same module and are composed only by `runtime`; neither directly imports the other. Cognitive Core does not implement ProjectPrefix, safepoint compaction or a Reference Directory here. Future Context Space work must preserve the existing admission and disclosure transactions.

## Longer-term Cognitive Evolution — evidence before expansion

1. Establish real-task net benefit under a separately approved bounded experiment. Preserve raw failures and distinguish outcome match from causality; v1.0 keeps `causal_benefit_unmeasured`. This task runs no paid model requests or cognitive benchmark.
2. If repeated real use justifies it, design independent per-consumer Use storage to resolve V-D1 / FFR-D3 and the lifetime bound. A migration requires its own approval, compatibility and privacy/recovery proof; **0018 is not pre-authorized**.
3. Extend semantic/procedural transfer only with observable validation and counterexamples. Model-generated beliefs/abstractions remain unverified proposals until exact host-authorized checks support them.
4. Optimize large-domain graph/cleanup queries only after measured latency or memory pressure. Preserve safety/counterexamples rather than introducing destructive LRU/TTL eviction, a new database or speculative GraphRAG/GNN.

The prior P0–P3 goals below remain a conceptual dependency order, not a mandate to start new work.

## P0 — Correctness and unified semantics

Purpose: make every cognitive object safe to interpret and use.

- Shared scope, time, evidence, provenance and applicability semantics.
- Explicit lifecycle and state transitions.
- Freshness at read/use time.
- Privacy and consumer/origin ownership boundaries.
- Transactional revision, revocation and recovery.
- One authoritative Go persistence and governance boundary.

The completion candidate closes the shared Scope/Time/Evidence, CAS/dependency, lifecycle and reader-privacy implementation paths. It is still unmerged; exact engineering/independent-review status is in the completion report.

## P1 — Conditional experience reuse and correction

Purpose: turn task episodes into reusable but bounded experience.

- Separate observation, interpretation, hypothesis and procedure.
- Extract candidate experience from real task outcomes.
- Abstract preconditions, strategy, expected outcome and exclusions.
- Validate against host-approved checks and counterexamples.
- Reuse only when the current task satisfies preconditions.
- Record consumer-specific adoption and outcome.
- Revise, narrow, invalidate or forget experience after feedback.

Ordinary Agent proposal → terminal extraction/validation → independent adoption/application → outcome feedback/revision is implemented. Exact-action conditional reuse is the deliberate authority boundary. Arbitrary parameterized transfer and measured longitudinal task improvement remain evidence/product work, not claims made by this implementation.

## P2 — Evidence-driven reasoning and metacognition

Purpose: let the system recognize what it knows, what it does not know, and what to do next.

- Explicit unknowns, conflicting claims and evidence gaps.
- Decision between acting, gathering evidence, asking, deferring or stopping.
- Track assumptions and their dependencies.
- Detect when a prior experience does not fit the current context.
- Revisit conclusions when supporting evidence changes.
- Keep model proposals advisory; framework owns authority and durable transitions.

`assess_cognition` now records exact-revision, evidence-aware decisions; conflicting propositions and unresolved premises cannot be silently treated as facts. The model chooses hypotheses and gather/ask/defer/stop; framework evidence does not replace general reasoning or establish causality.

## P3 — Scale and long-horizon memory management

Purpose: keep cognition useful as history and knowledge grow.

- Task-relevant retrieval and evidence coverage.
- Episodic summarization and semantic consolidation.
- Frequency, recency and observed utility as separate signals.
- Evidence-aware retention, archival and capacity controls.
- Large graph/experience query optimization.
- Physical storage tiers only if a measured product need justifies them.

The candidate implements protected logical retention, owned active-capacity maintenance, equivalent-evidence canonicalization, reader-local accesses and separate observed outcomes. Physical storage tiers, very large selected-domain query optimization and causal benefit estimation require measured need. Do not create a second memory manager or a speculative benchmark now.

## Architecture guardrails

- Do not add GraphRAG, GNN, ANN, new databases, or extra Agent loops merely to make the diagram look complete.
- Prefer extending the shared Go contracts and existing domains.
- Node.js research remains historical/reference material, not production implementation.
- A design target must not be described as implemented.
- Mechanism tests must not be presented as proof of long-term cognitive improvement.
