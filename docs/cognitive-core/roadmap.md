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

The expanded P0 candidate has been implemented and reviewed in several local candidates. It is not part of Core main merely by virtue of those local results; use the current release/merge record for availability.

## P1 — Conditional experience reuse and correction

Purpose: turn task episodes into reusable but bounded experience.

- Separate observation, interpretation, hypothesis and procedure.
- Extract candidate experience from real task outcomes.
- Abstract preconditions, strategy, expected outcome and exclusions.
- Validate against host-approved checks and counterexamples.
- Reuse only when the current task satisfies preconditions.
- Record consumer-specific adoption and outcome.
- Revise, narrow, invalidate or forget experience after feedback.

The open architectural issue is generalization: moving from one concrete episode to a reusable method without losing conditions or overclaiming universality.

## P2 — Evidence-driven reasoning and metacognition

Purpose: let the system recognize what it knows, what it does not know, and what to do next.

- Explicit unknowns, conflicting claims and evidence gaps.
- Decision between acting, gathering evidence, asking, deferring or stopping.
- Track assumptions and their dependencies.
- Detect when a prior experience does not fit the current context.
- Revisit conclusions when supporting evidence changes.
- Keep model proposals advisory; framework owns authority and durable transitions.

Metacognition is a cross-cutting responsibility, not necessarily a separate service or another Agent loop.

## P3 — Scale and long-horizon memory management

Purpose: keep cognition useful as history and knowledge grow.

- Task-relevant retrieval and evidence coverage.
- Episodic summarization and semantic consolidation.
- Frequency, recency and observed utility as separate signals.
- Evidence-aware retention, archival and capacity controls.
- Large graph/experience query optimization.
- Physical storage tiers only if a measured product need justifies them.

## Architecture guardrails

- Do not add GraphRAG, GNN, ANN, new databases, or extra Agent loops merely to make the diagram look complete.
- Prefer extending the shared Go contracts and existing domains.
- Node.js research remains historical/reference material, not production implementation.
- A design target must not be described as implemented.
- Mechanism tests must not be presented as proof of long-term cognitive improvement.
