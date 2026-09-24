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
- failure-aware cognitive routing,
- long-horizon pruning, branching, promotion, canonicalization, and graph versioning.

Putting all of this inside one planner or one reasoning model would mix responsibilities across very different time scales and would give model outputs too much direct control over durable state.

## Decision

Adopt a five-part cognitive control architecture:

1. **Cognitive Kernel** — deterministic control, budgets, legality, policy application, persistence, and audit.
2. **Light Controller** — fast typed local decisions through a pluggable `DecisionProvider`.
3. **Think** — a first-class task-local deliberate reasoning route, selectable directly or after later progress/failure signals.
4. **Cognitive Profile** — configuration that binds Fast, Think, execution, and Governor roles to concrete providers/models and constrains allowed Think-effort ranges.
5. **Graph Governor** — long-horizon Context Graph maintenance across tasks and Sessions.

The current deterministic Attention Light remains the graph-selection engine.

Jev, Laya, heuristic logic, or other future decision models may implement `DecisionProvider`. They are not part of the core architecture contract.

Think produces strategies and Work Units and does not own global graph maintenance.

The Graph Governor produces validated graph-mutation plans. Its model component cannot directly mutate the durable graph.

The framework chooses cognitive mode; configuration chooses the concrete provider/model for that mode. Runtime model competition is not part of this architecture.

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
- Cognitive mode selection and concrete model configuration are separate concerns.
- Work Units do not choose models.
- Fast/Think/Governor providers are configured explicitly.
- Changing GPT to Claude for Think is a configuration change, not a runtime routing decision.
- Think reasoning intensity is a framework-owned runtime decision within configured bounds.
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

Decision uncertainty may use normalized entropy and top-two probability margin as cognitive-route selection signals.


## Cognitive-route selection

Cognition is routed among peer routes:

```text
Cognitive Request
    -> deterministic Algorithm Route
    -> model-backed Decision Route
    -> Think Route
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

Think may be selected directly for a complex or high-risk task, or later when new evidence shows that deliberate reasoning has become worthwhile. Once Think is selected, the framework independently chooses its reasoning intensity.

Low confidence, high entropy, conflicting judgments, or a small top-two margin are route-selection signals, not availability failures.

## Dynamic Think effort

The framework owns two independent decisions:

```text
1. cognitive mode: Algorithm / Fast / Think
2. Think effort: low / medium / high / max
```

The concrete Think model remains configuration-bound.

A target metareasoning objective is:

```text
effort* = argmax_e [
    E[deliberation_gain | state, e]
    - lambda * compute_cost(e)
    - mu * latency(e)
]
```

The profile defines allowed bounds; the runtime chooses the value per invocation.

Provider adapters map the abstract effort to provider-supported reasoning controls or, where necessary, orchestration controls such as reasoning budget, passes, context, subagents, and verification depth.

## Configured model bindings

The framework does not dynamically rank GPT, Claude, DeepSeek, Laya, Jev, or other models against one another at runtime.

A Cognitive Profile binds roles to concrete implementations and bounds their runtime effort, for example:

```text
Fast     -> Laya
Think    -> Claude, effort range low..high
Governor -> configured reasoning model
```

The same architecture remains valid if the user changes those bindings. Route selection remains framework-owned; provider/model choice remains configuration-owned.

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

### Runtime model competition inside cognitive routing

Rejected because the framework should decide the cognitive mode, while users/projects explicitly configure which model implements each mode. This keeps behavior predictable and avoids turning task planning into model scheduling.

### Let Think choose concrete models

Rejected because Think should reason about the task, not rewrite its own provider configuration.

### Let Graph Governor directly rewrite graph state

Rejected because semantic proposals require deterministic validation, provenance preservation, rollback, and Cognitive Git auditability.

## Consequences

### Positive

- preserves an independently usable deterministic decision path,
- separates fast judgment from deliberate reasoning,
- prevents long-term memory maintenance from being biased by the current task,
- supports future decision models without coupling the runtime to one provider,
- keeps model choice explicit and configuration-driven,
- allows GPT/Claude/Jev/Laya/local models to be swapped without changing architecture,
- gives graph maintenance an explicit lifecycle and rollback boundary.

### Negative / trade-offs

- introduces additional contracts and runtime state,
- requires progress/failure instrumentation before adaptive routing is useful,
- configured profiles require sensible defaults and clear override precedence,
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
- cognitive profile / mode bindings,
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
8. profile-resolution tests prove explicit run override > project profile > user profile > default,
9. changing Think from one configured reasoning provider to another does not change routing semantics,
10. Think effort changes dynamically for low/high-complexity states while remaining within configured bounds,
11. graph plans cannot bypass Graph Validator,
12. Governor operations preserve provenance and support rollback,
13. benchmark adaptive routing and Think-effort allocation against fixed baselines for quality, cost, and latency.

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
