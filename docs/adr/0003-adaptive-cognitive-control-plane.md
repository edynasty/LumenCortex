# ADR-0003: Separate adaptive cognitive control from task reasoning and graph governance

- **Status:** Accepted
- **Date:** 2026-09-24
- **Owners:** project maintainers
- **Supersedes:** none
- **Superseded by:** none

## Context

LumenCortex already has a persistent Context Graph, deterministic Attention Light, Active Promotion, Cognitive Git, a multi-turn Agent Loop, Workflow Contracts, and generative model providers.

The next architecture stage needs to support:

- fast model-based local decisions,
- deliberate reasoning only when additional computation is justified,
- heterogeneous model selection by capability/cost/latency,
- failure-aware escalation,
- long-horizon pruning, branching, promotion, canonicalization, and graph versioning.

Putting all of this inside one planner or one reasoning model would mix responsibilities across very different time scales and would give model outputs too much direct control over durable state.

## Decision

Adopt a five-part cognitive control architecture:

1. **Cognitive Kernel** — deterministic control, budgets, legality, policy application, persistence, and audit.
2. **Light Controller** — fast typed local decisions through a pluggable `DecisionProvider`.
3. **Think** — a first-class task-local deliberate reasoning route, selectable directly or after later progress/failure signals.
4. **Model Broker** — capability/cost/latency-aware model selection for Work Units.
5. **Graph Governor** — long-horizon Context Graph maintenance across tasks and Sessions.

The current deterministic Attention Light remains the graph-selection engine.

Jev, Laya, heuristic logic, or other future decision models may implement `DecisionProvider`. They are not part of the core architecture contract.

Think produces strategies and Work Units. It does not directly choose concrete model names and does not own global graph maintenance.

The Graph Governor produces validated graph-mutation plans. Its model component cannot directly mutate the durable graph.

The Model Broker selects models from capability requirements and verified runtime outcomes. Cognitive roles and task categories do not bind directly to model identities.

## Invariants

- Models propose; deterministic runtime code validates and executes.
- Workflow and permission policy remain authoritative over legal execution.
- Attention Light remains usable without any adaptive decision model.
- Fast control supports multiple peer Decision Paths: model-backed paths and a deterministic Algorithm Path.
- Decision-path selection is based on applicability, availability, cost, latency, locality, and semantic requirements rather than a fixed downgrade order.
- Semantic uncertainty is not treated as provider failure; it is one signal that may route cognition to Think.
- Fast decision models never directly write Context Graph state.
- Think is task-local and does not own long-horizon Cortex structure.
- Graph Governor is cross-session/global and does not own the current task plan.
- Model Broker does not perform task decomposition.
- Work Units describe required capability, not provider/model names.
- Model capability profiles are version-specific and evidence-driven.
- Model switching uses hysteresis/stickiness rather than switching on small utility differences.
- Durable graph maintenance preserves provenance and is auditable through Cognitive Git.
- Destructive deletion is exceptional; archival/tiering/canonicalization are preferred.
- Planned components must not be documented as implemented until code and validation exist.

## Cognitive-mode selection

The target decision principle is value-of-computation:

```text
V(m | s)
  = E[task_utility | m, s]
    - lambda * compute_cost(m)
    - mu     * latency(m)
    - rho    * switching_cost(m)
```

Cognitive routing selects the route with the best expected value after cost, latency, risk, and switching cost. Think may be selected immediately; it is not restricted to post-failure escalation.

Decision uncertainty may use normalized entropy and top-two probability margin as escalation signals.


## Cognitive-route selection

Cognition is routed among peer routes:

```text
Cognitive Request
    -> deterministic Algorithm Route
    -> model-backed Decision Route
    -> Think Route
    -> optional Deep-Think Route
```

There is no requirement that a fast route run before Think.

The Cognitive Kernel selects an eligible route using:

- task semantics and complexity,
- provider/model availability,
- cost and latency,
- privacy/locality,
- operational health,
- whether deterministic runtime state is sufficient,
- semantic uncertainty,
- action risk and reversibility,
- expected value of additional computation.

A deterministic route may be selected first when the decision is mechanically derivable.

A model-backed decision route may be selected when a fast semantic judgment is sufficient.

Think may be selected directly for a complex or high-risk task, or later when new evidence shows that deliberate reasoning has become worthwhile.

Low confidence, high entropy, conflicting judgments, or a small top-two margin are route-selection signals, not availability failures.

## Model selection

A Work Unit produces a capability contract.

The broker first excludes models that fail hard requirements, then scores remaining models using:

```text
U(m, w)
  = P(success | m, w) * value(w)
    - lambda * expected_cost(m, w)
    - mu     * expected_latency(m, w)
    - nu     * risk(m, w)
```

A simple initial evidence model may use a Beta posterior per model/version and capability family.

## Graph governance

The Graph Governor is decomposed into:

```text
Analyzer -> Curator -> Planner -> Validator -> Executor
```

The Analyzer uses deterministic graph metrics to generate candidates.

The Curator/Planner may use a model to propose:

- tier/archive candidates,
- branches,
- promotions,
- canonicalization,
- relation reweighting,
- global summaries,
- Cortex Epoch transitions.

The Validator enforces graph invariants before the Executor applies changes through existing graph/Cognitive-Git mechanisms.

## Alternatives considered

### One model owns fast decisions, planning, model routing, and graph maintenance

Rejected because task-local optimization and long-horizon memory maintenance have different objectives and because direct model ownership of persistent mutation weakens determinism and auditability.

### Put adaptive decisions directly inside AttentionEngine

Rejected because Attention Light should remain an independently usable deterministic selection engine. Adaptive policy belongs above it.

### Hard-code model selection by task role

Rejected because capability, cost, latency, and model quality change over time. The durable contract should be capability-based and evidence-driven.

### Let Think choose concrete models

Rejected because task reasoning and resource scheduling are separate concerns. Think should state capability requirements, while Model Broker owns resource selection.

### Let Graph Governor directly rewrite graph state

Rejected because semantic proposals require deterministic validation, provenance preservation, rollback, and Cognitive Git auditability.

## Consequences

### Positive

- preserves an independently usable deterministic decision path,
- separates fast judgment from deliberate reasoning,
- prevents long-term memory maintenance from being biased by the current task,
- supports future decision models without coupling the runtime to one provider,
- supports heterogeneous model capabilities without permanent role mappings,
- enables verified outcome learning for model selection,
- gives graph maintenance an explicit lifecycle and rollback boundary.

### Negative / trade-offs

- introduces additional contracts and runtime state,
- requires progress/failure instrumentation before adaptive routing is useful,
- capability learning needs enough verified outcomes to become informative,
- Graph Governor adds validation and migration complexity,
- cost/latency utility functions require calibration.

## Security and trust impact

No model output gains direct authority.

The Cognitive Kernel, Workflow, permission policy, and Graph Validator remain deterministic enforcement boundaries.

Graph Governor proposals must preserve provenance and remain reversible.

External decision/generative providers are subject to the same data-boundary and credential policies as other model providers.

## Persistence / compatibility impact

The decision itself does not require an immediate schema change.

Future implementation may add persisted:

- failure signatures,
- Work Units,
- capability profiles,
- broker outcomes,
- Governor plans,
- Cortex Epoch metadata.

Any schema addition requires a separate migration with backward compatibility.

## Validation plan

Before adaptive control can be called validated:

1. deterministic decision-path behavior remains green,
2. Cognitive Router selects an eligible alternate route when one provider is unavailable,
3. Algorithm Decision Path is exercised as a first-class path when deterministic state is sufficient,
4. configurations with no decision model remain functional,
5. low-confidence decisions remain visible to route selection rather than being hidden by provider switching,
6. Progress Monitor correctly groups repeated failures,
7. routing tests prove direct Think selection and fast -> Think -> fast transitions,
8. broker tests prove hard-capability filtering and switch hysteresis,
9. verified outcomes update capability profiles deterministically,
10. graph plans cannot bypass Graph Validator,
11. Governor operations preserve provenance and support rollback,
12. benchmark adaptive routing against fixed deterministic baselines for quality, cost, and latency.

## Documentation impact

Canonical design:

- `docs/cognitive-control-plane.md`

Related documents:

- `docs/architecture.md`
- `docs/attention-light-algorithm.md`
- `docs/system-overview.md`
- `docs/standalone-readiness.md`
- `docs/documentation-guide.md`
- `docs/adr/README.md`
