# Cognitive Capability and Evidence Matrix

**Snapshot:** Core main 883c63e (2026-11-12 documentation worktree base). Expanded Cognitive Core candidates are tracked separately.

Status meanings:

- **Implemented in main** — Go production code exists on the stated main baseline.
- **Candidate** — implementation exists on an unmerged branch/commit; not available as main capability.
- **Partial** — only a bounded subset is implemented.
- **Design target** — documented direction without a complete production mechanism.
- **Evidence gap** — code may exist, but the stated behavior or effect has not been established.

| Domain / capability | Current Go main | Candidate / historical evidence | What evidence supports | Remaining boundary |
|---|---|---|---|---|
| Durable sessions and task history | Implemented | Earlier continuity work | Persistence, resume, WAL/recovery tests | Durable history alone is not learning |
| Notes and bounded recall | Implemented | Context continuity reports | Recall ranking and recency tests | Not a unified short/long-term memory manager |
| Context Graph / knowledge foundations | Implemented baseline | Cognitive Core P0/finalization candidates | Go store, provenance and graph tests | Expanded candidate semantics not merged |
| Scope / Time / Evidence contracts | Partial baseline | P0 and finalization candidates | Candidate-level schema, tests and review records | Candidate is not main; broad semantics still need closure |
| Attention Light | Implemented baseline | Astra architecture / Node research | Go lexical/symbol candidate retrieval and bounded propagation | Node FTS/PPR/embedding/RRF results are not Go evidence |
| Task-specific working set | Partial | Attention target | Context budget and bounded context selection | Unified attention policy and coverage/unknown detection remain incomplete |
| General evidence-driven reasoning | Partial | Cognitive roadmap | Model reasoning, Work Units, evidence and decision routing | No general-purpose proof of evidence sufficiency or causal reasoning |
| Experience extraction | Candidate | Cognitive Core P0 | Candidate extraction and persistence tests | Not a universal experience abstraction mechanism |
| Experience validation and adoption | Candidate | Cognitive Core P0/finalization | Host-bound checks, conditions, adoption and privacy tests | Not general transfer across arbitrary tasks |
| Outcome feedback and revision | Candidate | Cognitive Core P0/finalization | Feedback and revision state-transition tests | Long-term behavior improvement not established |
| Belief conflict / freshness / invalidation | Partial baseline; expanded candidate | Fable/Astra review records | Specific conflict, stale, revocation and revision regressions | General belief logic and all cross-domain transitions not complete |
| Governor Analyze / Plan / Validate | Implemented, read-only | Cognitive P0 candidate extends semantics | Go shared-store tests and no-mutation tests | Apply/Curator execution is not claimed in main |
| Verification and provenance | Implemented baseline | P0 candidate extends scope/time/evidence | Host-approved checks, source identity, transactional evidence | Validity is bounded by each check's actual meaning |
| Metacognitive gap detection | Design target / partial signals | Astra roadmap | Unknown/stale/conflict and progress signals | No unified metacognitive control loop |
| Experience generalization / transfer | Design target / partial conditional reuse | Astra Learning pipeline | Preconditions and constrained adoption foundations | Abstraction, counterexamples and robust transfer remain incomplete |
| Memory consolidation / semantic compression | Design target | Astra capability roadmap | Context selection and bounded storage only | No unified episodic-to-semantic consolidation pipeline |
| Frequency / utility-based memory management | Partial signals | Memory lifecycle proposal | Some usage/recency and bounded retention mechanisms | No unified evidence-aware frequency/utility policy |
| Long-term cognitive evolution | Not established | Longitudinal direction | Maintenance and revision foundations | No basis to claim sustained autonomous improvement |

## Evidence interpretation

- Passing Go tests establish tested mechanisms and invariants, not general cognitive competence.
- Historical Node.js algorithms and tests are reference material only. They do not establish Go production capabilities.
- Unmerged Cognitive Core candidates may be discussed as candidate work, never as current main behavior.
- No current evidence in this matrix proves that LC autonomously improves its future task success over time.
