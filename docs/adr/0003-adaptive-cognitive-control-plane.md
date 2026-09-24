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
- ordered Category model chains for task-specific generative-model preferences,
- failure-aware cognitive routing,
- long-horizon pruning, branching, promotion, canonicalization, and graph versioning.

Putting all of this inside one planner or one reasoning model would mix responsibilities across very different time scales and would give model outputs too much direct control over durable state.

## Decision

Adopt a five-part cognitive control architecture:

1. **Cognitive Kernel / Router** — deterministic final authority for routing, budgets, legality, Think selection, and reasoning effort.
2. **Decision Layer** — bounded advisory judgments through pluggable `DecisionProvider` implementations such as algorithmic rules, Jev, or Laya.
3. **Category model chains** — user-configurable ordered generative-model preferences for kinds of work.
4. **Think** — framework-selected deliberate reasoning using the selected Category model and dynamic reasoning effort.
5. **Graph Governor** — long-horizon Context Graph maintenance across tasks and Sessions.

The current deterministic Attention Light remains the graph-selection engine.

Jev, Laya, heuristic logic, or other future decision models may implement `DecisionProvider`. They are advisory decision/judgment providers and never appear in Category generative-model chains.

Think produces strategies and Work Units and does not own global graph maintenance.

The Graph Governor produces validated graph-mutation plans. Its model component cannot directly mutate the durable graph.

The framework owns final routing: it chooses Category, decides whether Think is needed, and chooses Think effort. Category configuration supplies an ordered generative-model chain. Runtime model competition is not part of this architecture.

## Invariants

- Models propose; deterministic runtime code validates and executes.
- Workflow and permission policy remain authoritative over legal execution.
- Attention Light remains usable without any adaptive decision model.
- Decision Layer providers are advisory and may be algorithmic or model-backed.
- Jev/Laya do not execute task Work Units and do not directly command the runtime to enter Think.
- Semantic uncertainty is not treated as provider failure; it is one input to framework routing.
- Decision providers never directly write Context Graph state.
- Think is task-local and does not own long-horizon Cortex structure.
- Graph Governor is cross-session/global and does not own the current task plan.
- Category selection, Think selection, and Think effort are framework concerns.
- Category configuration contains ordered generative-model chains and does not require a `mode` field.
- Work Units do not directly choose model identities.
- Changing the ordered models inside a Category is configuration, not a framework-architecture change.
- Think reasoning intensity is a framework-owned runtime decision.
- Runtime latency/speed is observed by LumenCortex rather than manually classified.
- Durable graph maintenance preserves provenance and is auditable through Cognitive Git.
- Destructive deletion is exceptional; archival/tiering/canonicalization are preferred.
- Planned components must not be documented as implemented until code and validation exist.

## Framework routing

The Cognitive Kernel owns final routing.

It consumes:

- deterministic runtime state,
- Progress Monitor state,
- Graph/Light state,
- Workflow constraints,
- provider health,
- Decision Layer judgments.

The Decision Layer may provide typed distributions such as likely Category, evidence sufficiency, stuck/progress, or retrieval direction.

The framework then decides:

```text
Category
Think? yes/no
Think effort
```

Jev/Laya are never the executor of the selected Category.

A target value-of-computation rule for Think is:

```text
Think if:

E[deliberation_gain | state]
  > compute_cost
    + latency_cost
    + transition_cost
```

Decision-provider entropy and top-two margin may contribute to that state, but do not have authority by themselves.

## Category model chains

A Category is an ordered generative-model preference chain.

Example:

```text
visual-engineering:
    Model A
    Model B
    Model C

deep:
    Model D
    Model A
```

If the first model is eligible and healthy, it is used. If not, resolution proceeds in list order.

There is no runtime scoring tournament between healthy entries.

Category configuration does not require a `mode` field.

Jev/Laya are configured under the Decision Layer, not under Category chains.

## Dynamic Think effort

The framework owns two independent decisions:

```text
1. cognitive mode: Algorithm / Fast / Think
2. Think effort: low / medium / high / max
```

The concrete generative model is resolved from the framework-selected Category's ordered chain; Think effort is decided separately by the framework.

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

## Configuration shape

The Cognitive Profile keeps two model concerns separate:

```text
decision:
    algorithm / Jev / Laya / compatible DecisionProvider

categories:
    ordered generative-model chains
```

Example:

```yaml
decision:
  providers:
    - laya
    - jev

categories:
  general:
    default: true
    models:
      - provider/general
      - provider/strong

  visual-engineering:
    models:
      - provider/visual-specialist
      - provider/general

  deep:
    models:
      - provider/strong
      - provider/general
```

The framework may use Decision Layer signals to classify the Category, but final Category/Think/effort decisions remain framework-owned.

Runtime speed is observed automatically. Explicit Category list order remains the default preference order.

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

### Runtime model competition inside a Category

Rejected because Category list order should express explicit user preference. Healthy entries are not continuously re-ranked against each other.

### Put Jev/Laya inside Category execution chains

Rejected because decision/judgment models and generative execution models have different contracts. Jev/Laya belong to the Decision Layer.

### Let Graph Governor directly rewrite graph state

Rejected because semantic proposals require deterministic validation, provenance preservation, rollback, and Cognitive Git auditability.

## Consequences

### Positive

- preserves an independently usable deterministic decision path,
- separates fast judgment from deliberate reasoning,
- prevents long-term memory maintenance from being biased by the current task,
- supports future decision models without coupling the runtime to one provider,
- keeps Category model preference explicit and configuration-driven,
- keeps Jev/Laya decision semantics separate from generative execution semantics,
- gives graph maintenance an explicit lifecycle and rollback boundary.

### Negative / trade-offs

- introduces additional contracts and runtime state,
- requires progress/failure instrumentation before adaptive routing is useful,
- Category definitions require sensible defaults/descriptions and clear override precedence,
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
- Decision Layer configuration and Category model chains,
- Governor plans,
- Cortex Epoch metadata.

Any schema addition requires a separate migration with backward compatibility.

## Validation plan

Before adaptive control can be called validated:

1. deterministic decision-path behavior remains green,
2. DecisionProvider outputs are advisory and cannot directly execute task Work Units,
3. configurations with no Jev/Laya remain functional through algorithmic decision signals,
4. low-confidence decision outputs remain visible to framework routing,
6. Progress Monitor correctly groups repeated failures,
6. routing tests prove framework-owned Think selection and dynamic effort,
7. Category-chain tests prove ordered resolution and default Category behavior,
8. tests prove Jev/Laya never appear as Category execution providers,
9. speed telemetry is learned from runtime calls and does not silently reorder healthy Category chains,
10. Think effort changes dynamically for low/high-complexity states,
11. graph plans cannot bypass Graph Validator,
12. Governor operations preserve provenance and support rollback,
13. benchmark framework routing and Think-effort allocation against fixed baselines for quality and latency.

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
