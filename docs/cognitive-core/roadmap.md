# Cognitive Core Architecture Evolution

## Design baseline

This roadmap preserves Astra's existing P0–P3 ordering and Fable's implementation constraints. It does not introduce a competing architecture or assume that every proposed capability should become a new module.

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
