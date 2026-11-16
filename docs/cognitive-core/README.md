# Cognitive Core

**Cognitive Core v1.0 integration, 2026-11-16.** Protected main `00344b5`; selected input `8c30f75`; isolated branch `integ/cognitive-core-v1-20261116`. Not merged or released. The [integration report](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.md) binds engineering results and exact commits. [中文](README.zh-CN.md).

**Review closure:** Astra completion, Fable FFR-001–017, Carbon ICRA-001–004 and Opus ICRA-V-001–003/V-T1 are included. Independent acceptance comes from the subsequent reviewers and the independent architecture review, not from this integration owner signing their own work. The [Finding Ledger](finding-ledger.md) records exact scopes and accepted limits without rewriting historical reports.

LumenCortex's cognitive objective is to help an Agent understand its current task, use grounded knowledge and experience, reason under uncertainty, act, learn from outcomes, correct outdated beliefs, and improve future behavior. The Core must not equate storage, summarization, retrieval, or reflection text with cognition.

## Start here

- [Unified cognitive architecture](architecture.md) — seven cognitive domains, their responsibilities, shared contracts, and boundaries.
- [Capability and evidence matrix](capabilities.md) — what exists in Go main, what exists only in a candidate, what is partial, and what remains a design goal.
- [Memory lifecycle](memory-lifecycle.md) — working, episodic, semantic, and procedural memory; consolidation, context compression, recency, frequency, utility, retention, and deletion.
- [v1.1 and Cognitive Evolution roadmap](roadmap.md) — future activation, dual-record-family and evidence/scale work; no authorization for migration 0018 or paid experiments.
- [Implementation](COGNITIVE_CORE_IMPLEMENTATION.md), [public API](COGNITIVE_CORE_API.md), [data model](COGNITIVE_CORE_DATA_MODEL.md), [migration](COGNITIVE_P0_STORAGE_DECISION.md) and [Finding Ledger](finding-ledger.md).
- [ADR-0023](../adr/0023-unified-cognitive-lifecycle.md) — current lifecycle, authority and exact-action learning decision.
- [v1.0 final integration](../reports/FINAL_COGNITIVE_CORE_V1_REPORT.md) — engineering gates, platform limits, migration and Git delivery.

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
- Experience is a Verified Procedural Macro; native belief/negative/abstraction writers and Governor lifecycle remain host SDK capabilities. Start advertises maintenance candidates, not Experience. V-D1 shared-owner withdrawal and V-002/V-003 fail-closed limits remain accepted.
- The two record families share evidence/source/privacy authority but retain different lifecycles and read paths. Cognitive Core and Context Space remain orthogonal, within one Go module and composed only by `runtime`.

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
