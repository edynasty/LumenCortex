# Cognitive Memory Lifecycle

## Purpose

This document defines the memory concepts that the Cognitive Core must distinguish. These are logical memory roles, not a requirement to create four databases or four independent services.

The current Go main has durable Sessions, Notes, context/knowledge foundations, bounded context selection and recency-aware recall. It does **not** yet provide one unified memory lifecycle that performs episodic consolidation, semantic compression, utility-aware retention and safe long-term capacity management.

## Four logical memory roles

| Role | Meaning | Typical content | Lifetime |
|---|---|---|---|
| **Working memory** | The bounded state needed for the current reasoning/action cycle | Current goal, constraints, active evidence, open questions, selected context, pending decisions | Current Run / task window; durable task state may survive restart |
| **Episodic memory** | A record of what happened in a particular task or interaction | Session events, tool observations, decisions, failures, checks, outcomes and provenance | Retained as history subject to privacy and retention policy |
| **Semantic memory** | Generalized claims about the world, project, system, or domain | Facts, relationships, concepts, conditions, evidence, validity and conflicts | Long-lived, but must be revised or invalidated when evidence/conditions change |
| **Procedural memory** | Reusable methods for achieving outcomes under stated conditions | Strategies, workflows, diagnostic steps, code patterns, failure-avoidance methods | Long-lived only while preconditions and supporting evidence remain applicable |

Short-term and long-term describe retention and lifecycle policy, not necessarily separate physical stores:

- Short-term information is recent, task-local, provisional, or awaiting consolidation.
- Long-term information is retained because it has durable value and sufficient provenance, not merely because it is old or frequently accessed.
- An episode can remain as historical evidence even after a semantic summary is created.
- A procedural experience must retain its preconditions, exclusions, origin and validation history.

## Consolidation: episode to reusable knowledge

A target consolidation path is:

    Episode / task outcome
      -> extract candidate patterns
      -> separate observation from interpretation
      -> abstract preconditions, strategy, expected result and exclusions
      -> bind source and evidence
      -> check applicability and counterexamples
      -> accept only within supported scope
      -> expose as a candidate for future retrieval/use
      -> record consumer-specific use and outcome
      -> revise, narrow, invalidate, archive or retain

This is not an automatic summarization step. A summary can reduce words while losing the conditions that made an action successful. Consolidation must preserve:

- what was observed versus inferred;
- the source episode and exact evidence;
- the context and preconditions;
- what action was taken and what outcome was observed;
- what did not work and known counterexamples;
- where the experience must not be applied;
- which consumers used it and their separate outcomes.

A single successful episode supports an instance-specific observation. It does not automatically establish a general rule.

## Compression: three different operations

Do not conflate these operations:

1. **Context-window selection/compression** — choose a bounded subset of relevant information for the next model request. This is a per-request projection; it must not silently rewrite durable memory.
2. **Episodic summarization** — create a derived summary of a task episode while retaining the original episode and provenance.
3. **Semantic consolidation** — abstract multiple observations into a conditional knowledge/experience object, preserve counterexamples and evidence, then validate it before broader reuse.

Lossy truncation is not semantic compression. If a derived summary cannot be traced back to its source, it must not replace the source as authoritative evidence.

## Frequency, recency, utility and confidence

These are distinct signals:

| Signal | Question answered | What it must not mean |
|---|---|---|
| Recency | When was this item last observed or used? | Recent does not mean true or important |
| Frequency | How often has it been independently accessed or applied? | Repeated access does not mean independent confirmation |
| Utility | Did using it improve an outcome or reduce cost/risk? | A successful outcome may have other causes |
| Evidence strength | What supports this claim and how directly? | Model confidence is not evidence |
| Applicability | Do current conditions satisfy its preconditions? | Similar wording is not sufficient |
| Stability | How likely is the underlying fact to change? | Old does not automatically mean stale |
| Dependency | Which decisions or experiences rely on it? | Deleting a consumer must not erase unrelated consumers |

A future retrieval policy may combine these signals, but it must keep their meanings inspectable. Repeated reuse should be deduplicated by independent Run/episode identity; retries or repeated reads of the same event must not inflate frequency.

Current Go thread recall contains a recency factor in ranking (the existing implementation uses a 30-day half-life component). That is a retrieval weight, not an expiry rule, correctness score, or general memory-decay policy. The current code does not provide a unified, evidence-aware frequency/utility manager for all cognitive memory.

## Retention, decay, archive and deletion

Never apply plain LRU/TTL deletion to authoritative cognitive knowledge.

A retention decision should consider:

- evidence quality and source availability;
- scope and time validity;
- stability and expiry conditions;
- independent use and observed utility;
- safety, legal, privacy and owner-preservation requirements;
- dependency graph and downstream consumers;
- whether the item is a unique historical failure or counterexample.

Low retrieval frequency is not enough to delete an invariant, safety rule, rare failure mode, or unique counterexample. A high-frequency item can still be wrong.

Distinguish:

- **deprioritize** — lower retrieval priority without changing validity;
- **expire/stale** — no longer current under its time or source rule;
- **invalidate/revoke** — no longer usable due to source, privacy or correctness event;
- **archive** — remove from the active working set while retaining governed history;
- **delete/purge** — remove data under an explicit privacy/retention contract and clean dependent material appropriately.

## Capacity management

Capacity management should first bound projections and work per operation, then govern durable storage separately.

Required design properties:

- bounded working set and per-request token budget;
- bounded recall candidate count and graph traversal;
- bounded consolidation batches;
- explicit limits for history, artifacts and derived summaries;
- source-preserving summaries and deterministic recovery;
- observable pressure and refusal reasons;
- no silent truncation of evidence or silent loss of unacknowledged data.

Physical hot/warm/cold stores and destructive cognitive garbage collection are not current Go main claims. Existing hot/warm/cold metadata is logical metadata, not proof of separate physical tiers.

## Current implementation boundary

| Mechanism | Current Go main status |
|---|---|
| Session / episodic history | Implemented |
| Notes and durable context | Implemented baseline |
| Bounded active context / token budget | Implemented baseline |
| Recency-aware thread recall | Implemented, limited ranking signal |
| Structured knowledge / evidence | Partial baseline; richer Scope/Time/Evidence is in the unmerged candidate |
| Experience extraction and adoption | Candidate, not current main |
| Episodic-to-semantic consolidation | Not complete |
| Semantic compression with source-preserving abstraction | Not complete |
| Unified frequency + utility policy | Not complete |
| Evidence-aware memory decay and retention | Not complete |
| Separate physical storage tiers / cognitive GC | Not claimed |

## Design principle

**Keep the original episode, derive reusable knowledge from it, and never confuse frequency, recency, similarity, or compression with truth.**
