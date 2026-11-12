# Unified Cognitive Architecture

## Purpose

This document consolidates Astra's seven-domain target architecture with Fable's implementation and evidence constraints. The seven domains are cognitive responsibilities, not seven independent services, databases, Agents, or loops.

The intended cycle is:

    Current cognitive state
      -> understand task and context
      -> attend to relevant knowledge and evidence
      -> reason, identify unknowns and decide
      -> verify claims and authority
      -> act through the Agent runtime
      -> capture outcomes and experience
      -> revise beliefs and govern lifecycle
      -> updated cognitive state for the next task

The cycle is a target relationship. Not every transition is fully implemented in current Go main.

## Seven cognitive domains

| Domain | Responsibility | Inputs | Outputs / state changes | Current implementation boundary |
|---|---|---|---|---|
| **Memory** | Preserve grounded episodes, facts, relationships, evidence, and reusable experience | Sessions, tool observations, human input, verified checks, task outcomes | Durable records with source, scope, time, and lifecycle | Go main has Sessions, Notes, context/knowledge foundations and persistent stores. Expanded Cognitive Core candidate adds richer Scope/Time/Evidence and Experience semantics. |
| **Attention** | Select what is relevant to the current task under a finite context budget | Goal, current state, scope, query, graph links, evidence, freshness | Ranked, bounded working set with provenance and exclusions | Go main has lexical/symbol retrieval and Attention Light foundations. Advanced semantic/hybrid retrieval in historical Node references is not Go production capability. |
| **Reasoning** | Form hypotheses, compare explanations, infer consequences, and decide what information/action is needed | Working set, evidence, constraints, uncertainty, task objective | Hypotheses, decision proposals, questions, plans, or explicit unknown | Model reasoning and Go cognitive-control routing exist. A general evidence-sufficiency and causal reasoning framework is not established as a complete Core capability. |
| **Learning** | Turn task episodes into conditional, validated, reusable experience | Episode, actions, observations, outcome, context, counterexamples | Experience candidates, preconditions, strategies, limits, feedback and revised experience | Expanded candidate contains extraction, validation, adoption, feedback and revision foundations. General abstraction, transfer and demonstrated future-task improvement remain open. |
| **Belief Revision** | Update, qualify, invalidate, or retain beliefs as evidence and conditions change | New evidence, contradiction, source status, time, scope, failures | New version/status, conflict, stale/unknown state, dependency invalidation | Go knowledge semantics and candidate mechanisms cover parts of provenance, freshness, conflict, invalidation and revision. General-purpose logical revision is not claimed. |
| **Governance** | Enforce cognitive policy, lifecycle, resource bounds, privacy and safe state transitions | Proposed changes, graph state, evidence, policy, budgets, lifecycle events | Allowed/rejected plans, maintenance actions, durable transitions | Go main Graph Governor supports read-only Analyze / Plan / Validate. Curator Apply and autonomous graph mutation are not claimed in main. |
| **Verification** | Establish whether a claim or transition is supported by an authorized, relevant check | Claim, source, scope, time, check definition, host observation | Verified/failed/unknown evidence bound to source and version | Go has host-approved checks, provenance, transactional and freshness controls. Verification proves only what the check actually establishes; it does not prove arbitrary semantic truth. |

## Shared cognitive contracts

The domains must share a small set of semantics. These are cross-cutting contracts, not optional metadata that each subsystem can interpret differently.

### Identity and scope

Every durable claim or experience must identify what it is about and where it applies: project/repository, environment, component, task class, actor, or other relevant scope. A claim must not silently widen from one project or environment to another.

### Time and validity

Distinguish observation time, effective time, expiry/validity conditions, and the time at which a reader uses the claim. Recency is a ranking signal; it is not proof of validity. A recent claim can be wrong, and an old invariant can remain valid.

### Evidence and provenance

Record who or what produced the information, the source locator/version, the check or observation that supports it, and its derivation chain. A model-generated statement is a proposal, not independent evidence.

### Applicability and uncertainty

Represent preconditions, exclusions, confidence/evidence strength, conflicts, unknowns, and stale status explicitly. If applicability cannot be established, the system must not silently treat the item as current.

### Lifecycle and revision

Knowledge and experience need explicit transitions: proposed, checked, accepted within a scope, applied, observed, challenged, revised, invalidated, archived, or deleted. Exact state names belong to the implementation contract; the invariant is that state changes are explicit, attributable, and recoverable.

### Ownership, privacy, and dependency

Separate origin ownership, shared definition, consumer-specific adoption, and consumer-specific outcomes. A consumer deletion must not accidentally delete unrelated consumers' private or valid state; origin revocation must invalidate all dependent uses that rely on that origin. Reads must enforce the same privacy semantics as writes, replay, context projection, and purge.

## Roles and authority

| Actor | May do | Must not do |
|---|---|---|
| Model | Interpret context, propose hypotheses, candidate knowledge, experience, plans, and uncertainty | Self-authorize checks, fabricate evidence, promote its own proposal, bypass privacy or policy |
| Agent | Execute authorized work and produce observable outcomes | Turn its own assertion into verified evidence |
| Host / tools | Perform real checks and return observations under explicit authority | Claim a check proves more than it actually checks |
| Go Cognitive Core | Bind identity/scope/time/provenance, validate transitions, enforce privacy/freshness, persist and recover state | Treat unverified model text as authoritative knowledge |
| Governance | Analyze and validate permitted lifecycle plans | Apply mutations beyond its current implemented authority |

## What is not yet a completed theory

The seven-domain model is a useful responsibility decomposition, but it is not by itself a complete cognitive theory. The following relationships still require explicit design closure:

- a unified cognitive state transition model across facts, hypotheses, experiences, evidence, and decisions;
- a decision process that can choose between acting, gathering evidence, asking, deferring, or stopping;
- experience abstraction and conditional transfer, not only storage and exact matching;
- metacognitive monitoring that can detect knowledge gaps, failed reasoning assumptions, and experience mismatch;
- consumer-safe provenance and privacy semantics across every read and mutation path;
- memory consolidation, compression, retention, and capacity policy.

Do not create a new service for each gap by default. First determine whether the shared contracts and existing domains can express the required behavior.
