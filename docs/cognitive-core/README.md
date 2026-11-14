# Cognitive Core

**Status snapshot: 2026-11-13.** Main is `00344b5`; this unmerged completion candidate integrates the committed `91af861` work and the final lifecycle/learning fixes. Availability, self-verification and independent review are separate; see the [completion report](../reports/cognitive-core-final-completion-2026-11-13.md). [中文](README.zh-CN.md).

**Reviewed 2026-11-14.** An independent full review of that candidate (`c1d3229`) reproduced 17 defects, four of them P1, and fixed them on `review/cognitive-core-fable-full-review-20261114`. The fixes are self-verified and await a reviewer who did not write them. See the [review report](../reports/cognitive-core-fable-full-review-2026-11-14.md) and the [finding ledger](finding-ledger.md#fable-full-review-2026-11-14-ffr).

LumenCortex's cognitive objective is to help an Agent understand its current task, use grounded knowledge and experience, reason under uncertainty, act, learn from outcomes, correct outdated beliefs, and improve future behavior. The Core must not equate storage, summarization, retrieval, or reflection text with cognition.

## Start here

- [Unified cognitive architecture](architecture.md) — seven cognitive domains, their responsibilities, shared contracts, and boundaries.
- [Capability and evidence matrix](capabilities.md) — what exists in Go main, what exists only in a candidate, what is partial, and what remains a design goal.
- [Memory lifecycle](memory-lifecycle.md) — working, episodic, semantic, and procedural memory; consolidation, context compression, recency, frequency, utility, retention, and deletion.
- [Architecture evolution](roadmap.md) — Astra's existing roadmap reconciled with Fable's implementation review and current owner direction.
- [Implementation](COGNITIVE_CORE_IMPLEMENTATION.md), [public API](COGNITIVE_CORE_API.md), [data model](COGNITIVE_CORE_DATA_MODEL.md), [migration](COGNITIVE_P0_STORAGE_DECISION.md) and [Finding Ledger](finding-ledger.md).
- [ADR-0023](../adr/0023-unified-cognitive-lifecycle.md) — current lifecycle, authority and exact-action learning decision.

## Authoritative design sources

This design synthesizes existing project work rather than replacing it with an unrelated architecture:

- Astra: workspace evidence / cognitive-capability-roadmap-20261110 / COGNITIVE_ARCHITECTURE_TARGET.md
- Astra: workspace evidence / cognitive-capability-roadmap-20261110 / COGNITIVE_CAPABILITY_MATRIX.md
- Astra: workspace evidence / cognitive-capability-roadmap-20261110 / COGNITIVE_ROADMAP.md
- Astra: workspace evidence / cognitive-capability-roadmap-20261110 / FINAL_COGNITIVE_CAPABILITY_AUDIT.md
- Fable: workspace evidence / cognitive-core-fable-final-review-20261112 / COGNITIVE_CAPABILITY_REVIEW.md
- Current candidate and review records under the workspace cognitive-core evidence directories.

The workspace evidence directory is not automatically part of the Core Git repository. This directory contains maintained, version-controlled design entry points; detailed historical evidence remains in its original evidence locations.

## Current baseline and claim boundary

- Verified Core main baseline is `00344b5`; it contains the prior Go Harness code at `208ad6b`.
- Main has cognitive-control, context/knowledge, Attention Light and read-only Graph Governor foundations.
- This candidate adds Scope/Time/Evidence, normal SQLite native graph and Experience, ordinary Agent learning tools, evidence assessment, dependency revisions, governed retention/consolidation and recovery. None is a new main/release claim.
- Final Astra/Fable/Gemini review candidates and their fixes must not be described as merged or released merely because their local branches have test evidence.
- Current implementation is not proof of general intelligence, autonomous learning, universal reasoning, or measured improvement on future tasks.

## Design invariant

One coherent cognitive system, not a pile of independent memory/reasoning/learning services:

- one authoritative Go runtime and persistence boundary;
- shared scope, time, evidence, provenance, applicability, and lifecycle semantics;
- models may propose interpretations, hypotheses, and experience candidates;
- the framework owns authorization, validation, durable state transitions, privacy, and recovery;
- actions and observed outcomes feed future cognition;
- unknown, conflicting, stale, or unsupported claims remain explicit rather than being forced into certainty.

## What counts as evidence

Every capability statement must distinguish:

1. **Source evidence** — the Go implementation and its real call path exist.
2. **Mechanism evidence** — tests establish the relevant state transition or invariant.
3. **Runtime evidence** — the same production path has been exercised in the stated environment.
4. **Cognitive-effect evidence** — there is evidence that the mechanism changes future understanding or behavior.

The first three do not automatically establish the fourth. Persisting an Experience proves storage; retrieving it proves recall; neither alone proves learning or transfer.
