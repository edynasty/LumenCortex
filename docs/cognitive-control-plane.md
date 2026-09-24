# Cognitive control plane

This document defines the target architecture for adaptive cognitive-mode routing, configurable model bindings, failure-aware reasoning, and long-horizon Context Graph governance in LumenCortex.

> **Status:** Planned architecture. The current runtime already implements the Context Graph, Attention Light, Active Promotion, Agent Loop, Workflow Contracts, provider abstraction, verification, and Cognitive Git. The control-plane components below are not yet claimed as implemented.

Related documents:

- [Architecture](architecture.md)
- [Attention Light algorithm](attention-light-algorithm.md)
- [Agent runtime](agent-runtime.md)
- [Execution flow](execution-flow.md)
- [ADR-0003 — Adaptive cognitive control plane](adr/0003-adaptive-cognitive-control-plane.md)

## 1. Design goal

LumenCortex needs three different kinds of cognition:

1. **Fast local judgment** — routing hints, retrieval policy, budget, stop/continue, confidence, and bounded classification.
2. **Deliberative task reasoning** — diagnosis, hypothesis revision, planning, unknowns, repeated failure, and high-risk decisions.
3. **Long-horizon Cortex governance** — pruning, branching, promotion, canonicalization, global summarization, tiering, and cognitive versioning.

These operate on different time scales and must remain separate.

The framework therefore separates:

| Component | Time scale | Main responsibility |
|---|---|---|
| Cognitive Kernel / Router | every step | deterministic mode selection, constraints, budgets, legality, validation |
| Light Controller | fast | bounded typed judgments inside Fast mode |
| Think | when selected | task-local deliberate reasoning and strategy revision |
| Graph Governor | periodic / global | maintain the long-term structure of the Context Graph |
| Cognitive Profile / Model Catalog | configuration | declare available models, task specialties, defaults, and hard capabilities |

## 2. Core invariants

```text
Graph is Memory.
Light is Attention.
Fast is Bounded Judgment.
Think is Deliberation.
Governor is Cognitive Maintenance.
Agent is Execution.
```

The runtime follows these rules:

1. **The framework chooses the cognitive mode.**
2. Models do not decide whether the runtime should be in Fast or Think.
3. The framework computes cognitive mode and abstract effort; it does not directly choose vendor/model identity.
4. Cognitive Profile / Model Catalog deterministically resolves `(mode, TaskProfile, constraints)` to a configured provider/model binding.
5. Jev/Laya are interchangeable Fast-mode implementations behind a typed `DecisionProvider`.
6. GPT/Claude/DeepSeek/local reasoning models are interchangeable Think-mode implementations behind a reasoning-provider contract.
7. Different models may declare different task specialties; reasoning effort remains framework-controlled.
8. Graph Governor uses its own configured model binding/catalog entry and does not share task-local Think responsibility.
9. Models may propose judgments, strategies, or graph plans; deterministic runtime code owns legality, budgets, persistence, and execution.
10. Durable graph mutation must preserve provenance and remain auditable through Cognitive Git.

## 3. Two separate decisions

The architecture separates **framework cognition** from **declarative model policy**.

The framework computes:

```text
(mode, TaskProfile, constraints)
```

The profile resolver computes:

```text
binding = resolve(catalog, mode, TaskProfile, constraints)
```

This resolver is deterministic configuration logic, not an autonomous model broker.

```text
                     Cognitive Request
                            |
                            v
                  +--------------------+
                  | Framework Router   |
                  | deterministic      |
                  +---------+----------+
                            |
                     (mode, effort)
                            |
                            v
                  +--------------------+
                  | Profile Resolver   |
                  | deterministic      |
                  +---------+----------+
                            |
        +-------------------+-------------------+
        |                   |                   |
        v                   v                   v
   Algorithm Mode        Fast Mode          Think Mode
   deterministic       DecisionProvider    ReasoningProvider
                            |                   |
                    configured route      configured tier
                            |                   |
                    Jev / Laya / ...      Model A / B / ...
```

`DeepThink` is not a separate architecture mode. It is represented as Think with a higher framework-selected reasoning intensity.

The framework never performs:

```text
"Which model should I dynamically pick for this task?"
```

Instead it performs:

```text
"Which cognitive mode should run now?"
```

The configured Cognitive Profile / Model Catalog answers:

```text
"Given this mode, Task Profile and runtime constraints, which configured model applies?"
```

This is intentionally lighter than a learned model scheduler. It is explicit, inspectable, and deterministic.

## 4. Cognitive Profile and lightweight Model Catalog

The user-facing configuration should stay simple.

Users should not need to estimate numeric suitability scores, effort ranges, cost classes, or latency classes.

A model entry declares only:

- provider/model identity,
- which cognitive modes it may serve,
- optional task specialties,
- optional hard capabilities,
- list order.

Runtime speed is measured by LumenCortex.

### 4.1 Minimal configuration

```yaml
cognition:
  models:
    - id: laya-local
      provider: laya
      model: decision-model
      modes: [fast]

    - id: general-think
      provider: anthropic
      model: reasoning-model
      modes: [think]
      default: true

    - id: frontend-specialist
      provider: provider-x
      model: model-x
      modes: [think]
      specialties:
        - frontend
        - ui
        - visual

    - id: debug-specialist
      provider: provider-y
      model: model-y
      modes: [think]
      specialties:
        - debugging
        - architecture
        - planning
```

This is intentionally qualitative.

The user says:

```text
"this model is especially good at frontend/UI"
```

rather than:

```text
frontend = 0.93
effort_range = [0.42, 0.81]
latency_class = medium
```

### 4.2 Task Profile

The framework derives a small set of task tags from the current state.

Example:

```text
TaskProfile:
  debugging
  repository
  architecture
```

Typical built-in tags may include:

```text
debugging
architecture
planning
frontend
ui
visual
research
code_generation
code_review
repository_navigation
testing
refactoring
documentation
```

The taxonomy should stay small and extensible. Custom tags may be allowed, but the built-in set should cover normal routing without configuration work.

### 4.3 Deterministic matching

For a selected cognitive mode:

1. filter models that do not support the mode or required hard capabilities,
2. prefer models whose `specialties` intersect the current Task Profile,
3. if several models match equally, prefer earlier list order,
4. if no specialist matches, use the configured default for that mode,
5. if the default is unavailable, use the first eligible model.

This is deterministic configuration resolution, not learned model competition.

A specialist is a preference, not an exclusive binding.

For example:

```text
TaskProfile = [frontend, visual]
    -> frontend-specialist

TaskProfile = [debugging, architecture]
    -> debug-specialist

TaskProfile = [general coding]
    -> general-think
```

### 4.4 Runtime speed telemetry

Users should not configure model speed.

LumenCortex measures it from actual calls.

Useful runtime observations include:

- time to first token,
- total response latency,
- generated tokens per second,
- tool-round latency,
- timeout/error rate.

The runtime keeps an EWMA or rolling percentile per concrete provider/model.

Speed is only a tie-breaker or policy input after task suitability and hard requirements. A model marked as a specialist should not lose its intended task merely because a generic model is slightly faster.

### 4.5 Dynamic Think effort

Task/model matching and reasoning effort are separate decisions.

The framework computes Think effort dynamically:

```text
low
medium
high
max
```

The selected model adapter maps that abstract effort to whatever the provider supports:

- native reasoning-effort parameter,
- reasoning/token budget,
- number of deliberate passes,
- hypothesis count,
- context budget,
- Contrarian review,
- focused Subagents,
- verification depth.

Users do not configure an `effort_range` per model.

If a provider exposes hard limits, those are adapter capabilities, not user-authored suitability scores.

### 4.6 Built-in defaults

LumenCortex should ship a small built-in catalog so the system works without custom routing configuration.

The built-in catalog should provide only a few roles:

```text
Fast default
Think default
Governor default
```

Optional built-in specialist hints may be shipped for a small number of well-known models, but they must remain overridable by user/project configuration.

The product should avoid maintaining a large permanent model leaderboard.

### 4.7 Configuration precedence

Configuration resolves deterministically:

```text
explicit run override
    > project catalog/profile
    > user catalog/profile
    > built-in default catalog
```

### 4.8 Route availability

A cognitive mode is eligible only when at least one configured model for that mode is operational.

```text
Algorithm
    always available

Fast
    available if at least one DecisionProvider model is available

Think
    available if at least one ReasoningProvider model is available
```

If a preferred specialist is unavailable, the resolver moves to the next matching entry or mode default.

## 5. Cognitive Kernel / Router

The Cognitive Kernel is deterministic orchestration code.

It owns:

- cognitive-mode selection,
- Workflow and permission constraints,
- token, cost, and latency budgets,
- route availability,
- deterministic Cognitive Profile resolution,
- legal mode transitions,
- provider health visibility,
- validation of model-proposed policies,
- graph-mutation authorization,
- persistence and audit records.

It does not answer semantic questions itself.

The control invariant is:

```text
framework selects mode and Think effort
    -> Task Profiler derives task tags
        -> catalog resolver selects configured model
        -> configured provider executes
            -> kernel validates output
                -> engine executes
                    -> verifier records outcome
```

## 6. Framework cognitive-mode routing

The framework chooses from modes, not model identities.

```text
M = {
  Algorithm,
  Fast,
  Think
}
```

Let `A(c)` be the set of modes available under configuration/health state `c`.

```text
mode* = argmax_{m in A(c)} V(m | s, c)
```

A target value function is:

```text
V(m | s)
  = E[task_utility_gain | m, s]
    - rho * transition_cost(m, s)
    - nu  * risk(m, s)
```

The first implementation may approximate this with deterministic thresholds and state features.

### 6.1 Typical routing signals

Framework signals may include:

- task complexity,
- repeated equivalent failure,
- progress delta,
- unknown count,
- contradiction density,
- evidence sufficiency,
- retrieval exhaustion,
- requested operation risk,
- irreversibility,
- token pressure,
- active Workflow constraints,
- Fast-provider uncertainty,
- mode availability.

Examples:

```text
retry-after header is known
    -> Algorithm

token budget crossed a hard threshold
    -> Algorithm

bounded semantic routing question
    -> Fast

complex multi-file architectural change
    -> Think

same verified failure after multiple distinct attempts
    -> Think

Think failed and unresolved uncertainty remains high
    -> Think again with higher framework-selected effort
```

Think can be selected on the first step. It is not only a failure escalation.

## 7. Algorithm Mode

Algorithm Mode handles decisions that are truly derivable from runtime state.

Examples:

- hard token-budget enforcement,
- retry/backoff policy,
- permission and Workflow gates,
- known failure-count thresholds,
- bounded graph-depth limits,
- provider health state,
- deterministic route exclusions.

It must not imitate semantic judgment it cannot perform.

If runtime state is insufficient, the framework selects Fast or Think according to routing policy and availability.

## 8. Fast Mode and DecisionProvider

Fast Mode performs high-frequency bounded semantic decisions.

Typical uses:

- choose retrieval policy,
- judge evidence sufficiency,
- classify likely failure type,
- decide whether Light should expand,
- score whether a contradiction is important,
- estimate whether the current local action still has progress potential.

### 8.1 Typed provider interface

```ts
interface DecisionProvider {
  decide(input: {
    state: DecisionState
    questions: TypedQuestion[]
    signal?: AbortSignal
  }): Promise<DecisionResult>
}
```

Possible configured implementations:

```text
DecisionProvider
├── JevDecisionProvider
├── LayaDecisionProvider
├── SmallLLMDecisionProvider
└── other compatible provider
```

Jev and Laya are implementations, not architecture requirements.

Conceptually:

```text
D : State -> ProbabilityDistribution(Actions)
```

The result should preserve distributions.

Example:

```json
{
  "retrieval_mode": {
    "code": 0.12,
    "causal": 0.61,
    "dependency": 0.19,
    "historical": 0.08
  },
  "evidence_sufficient": 0.34,
  "stuck": 0.77
}
```

### 8.2 Fast uncertainty

For a probability distribution `p` over `K` actions:

```text
normalized_entropy(p)
  = - sum_i p_i * log(p_i) / log(K)
```

Also track:

```text
margin(p) = p_top1 - p_top2
```

High entropy, small margin, or conflicting Fast judgments are returned to the framework.

The Fast model does **not** decide to enter Think.

The framework consumes these signals and may choose:

- remain in Fast,
- gather more evidence,
- switch to Algorithm,
- enter Think,
- remain in Think with a higher effort tier.

## 9. Think Mode

Think is task-local deliberate reasoning selected by the framework.

The concrete reasoning model is configuration.

Examples:

```text
Think Mode
├── OpenAI GPT reasoning model
├── Anthropic Claude
├── DeepSeek reasoning model
├── local reasoning model
└── other compatible generative provider
```

Changing:

```text
Think = GPT
```

to:

```text
Think = Claude
```

is a configuration change, not an architecture change and not a runtime model-selection decision.

### 9.1 Think responsibilities

Think is appropriate for:

- root-cause diagnosis,
- hypothesis generation/revision,
- multi-step planning,
- architecture trade-offs,
- unresolved unknowns,
- contradictory evidence,
- repeated verified failures,
- strategy reformulation.

Think should normally return structured strategy/state:

```yaml
diagnosis:
  hypotheses:
    - transaction lifetime is wrong
    - a test double hides the production path

missing_evidence:
  - production call path
  - transaction boundary

work_units:
  - goal: inspect caller chain
  - goal: reproduce the transaction boundary

light_policy:
  mode: causal
  max_hops: 6
  emphasize:
    - calls
    - causes
    - depends_on

stop_conditions:
  - root cause reproduced
  - hypothesis falsified
```

The framework may return to Fast after Think has produced a usable strategy.

## 10. Dynamic Think intensity

Think is one cognitive mode with a dynamic reasoning intensity.

The framework chooses the effort for each Think invocation. Model selection is resolved separately from task specialties and configured defaults.

A normalized target can be represented as:

```text
e_t in [0, 1]
```

or as implementation tiers:

```text
low
medium
high
max
```

A target objective is:

```text
effort* = argmax_e [
    E[deliberation_gain | state, e]
    - lambda * compute_cost(e)
    - mu * latency(e)
]
```

The first implementation may estimate an effort score from observable state:

```text
z
  = w1 * task_complexity
  + w2 * semantic_uncertainty
  + w3 * repeated_failure
  + w4 * contradiction_density
  + w5 * unknown_density
  + w6 * action_risk
  + w7 * retrieval_exhaustion

effort = clamp(sigmoid(z), configured_min, configured_max)
```

Users do not configure numeric effort ranges for individual models. The framework computes effort, while the selected provider adapter maps that effort to supported reasoning controls.

After catalog resolution, the selected provider adapter translates the framework's abstract effort into controls supported by that provider/model.

Possible mappings include:

- native reasoning-effort controls when available,
- reasoning/token budget,
- context budget,
- number of deliberate passes,
- hypothesis count,
- Contrarian review,
- focused Subagents,
- verification depth.

The provider/model does not choose its own effort tier. The framework computes it from runtime state.

## 11. Progress Monitor and route transitions

Failure count alone is insufficient.

The Progress Monitor records:

- normalized failure signatures,
- repeated failure count,
- attempted strategies,
- verification outcomes,
- progress delta,
- retrieval coverage,
- unknowns,
- contradictions,
- repeated tool/action loops.

Example:

```json
{
  "signature": "hash(error + failing_test + stack_shape)",
  "count": 3,
  "first_step": 4,
  "last_step": 8,
  "attempted_strategies": [
    "edit foo.js",
    "edit bar.js"
  ]
}
```

Possible framework interpretation:

```text
execution_failure
    -> Algorithm / retry

retrieval_failure
    -> Fast + expanded Light

reasoning_failure
    -> Think

environment_failure
    -> Algorithm or targeted Think

unknown
    -> evidence gathering or Think
```

## 12. Work Units

Think may decompose a task into Work Units, but Work Units do not select models.

```yaml
work_unit:
  goal: repair transaction bug
  risk: medium
  required_evidence:
    - production call path
  verification:
    - focused_reproduction
    - unit_test
```

The execution model remains whatever is configured for the execution role/profile unless the user explicitly overrides configuration.

This avoids turning task planning into dynamic model scheduling.

## 13. Provider health

Provider health is operational state, not cognitive competence.

For each configured model-backed role:

```text
healthy
  -> repeated transport/protocol failures
temporarily unavailable
  -> cooldown / bounded probe
healthy again when probe succeeds
```

Operational failures include:

- timeout,
- provider unavailable,
- authentication failure,
- malformed protocol response,
- incompatible schema.

Semantic uncertainty is different.

A healthy Fast provider that returns uncertain probabilities is still healthy.

## 14. Graph Governor

### 14.1 Role

Graph Governor is a separate long-horizon subsystem.

Think optimizes the current task.

Graph Governor optimizes the durable Cortex across tasks and sessions.

Its configured model may be different from Think.

A conceptual objective is:

```text
maximize:
    retrievability
  + consistency
  + compression
  + future_utility
  + provenance_preservation

minimize:
    maintenance_cost
  + duplication
  + stale_structure
  + retrieval_noise
```

### 14.2 Internal decomposition

```text
Graph Governor
├── Analyzer
│   deterministic metrics and candidate generation
├── Curator
│   configured semantic model
├── Planner
│   GraphMutationPlan generation
├── Validator
│   deterministic invariant checks
└── Executor
    graph + Cognitive Git operations
```

The Governor model never writes durable graph state directly.

### 14.3 Governance operations

The Governor may propose:

- **prune/tier** — hot -> warm -> cold -> archive,
- **branch** — preserve competing hypotheses,
- **promote** — create higher-level abstractions,
- **merge/canonicalize** — identify aliases/duplicates without erasing provenance,
- **reweight** — propose relation-weight changes,
- **summarize** — create structured global Cortex summaries,
- **epoch/version** — create a cognitive version boundary after major reorganization.

Deletion should be rare. Archival, tiering, and provenance-preserving canonicalization are preferred.

## 15. Global Cortex summary

Governor summaries should be structured graph state, not only prose.

```yaml
cortex_state:
  major_concepts: []
  active_projects: []
  current_architecture: []
  unresolved_conflicts: []
  stale_regions: []
  high_value_nodes: []
  redundant_clusters: []
  emerging_patterns: []
  candidate_promotions: []
```

These summaries may become high-level graph nodes so Attention Light can retrieve global structure cheaply.

## 16. Graph metrics for governance

The Analyzer should narrow the search space before semantic model judgment.

Useful signals:

- activation frequency,
- retrieval contribution,
- recency,
- evidence grade,
- trust zone,
- structural centrality,
- contradiction density,
- cluster density,
- duplicate similarity,
- stale ratio,
- abstraction coverage,
- retrieval hit rate.

A simple node-value model:

```text
Value(node)
  = a * activation_frequency
  + b * retrieval_contribution
  + c * structural_centrality
  + d * evidence_quality
  - e * staleness
```

Low-value nodes become prune/tier candidates, not automatic deletion targets.

## 17. Branching and conflict

For two graph regions `C1` and `C2`:

```text
Conflict(C1, C2)
  = weighted_contradiction_edges(C1, C2)
    / weighted_cross_edges(C1, C2)
```

A high score means “consider a cognitive branch”, not “branch automatically”.

The Governor should distinguish:

- genuinely competing hypotheses,
- historical phase changes,
- alias/duplicate confusion,
- bad ingestion,
- temporary stale evidence.

## 18. Promotion and compression

Local Active Promotion remains task-local.

Governor adds global promotion across sessions.

An MDL-style target:

```text
PromotionGain
  = Cost(children)
    - (
        Cost(abstraction)
        + Cost(references)
        + InformationLoss
      )
```

Promotion is attractive when it reduces cognitive description cost without destroying drill-down to evidence.

## 19. Cortex Epochs

Cognitive Git versions ordinary graph mutations.

A **Cortex Epoch** represents larger semantic reorganization.

Possible triggers:

- large-scale stale abstractions,
- major architecture changes,
- accumulated canonicalization,
- persistent retrieval degradation,
- graph density beyond configured limits,
- hot/warm/cold restructuring.

An epoch records:

- source Cognitive Git commit,
- Governor plan,
- validation report,
- structural metrics before/after,
- resulting root abstractions,
- rollback target.

Epoch creation must be reversible.

## 20. Relationship to Attention Light

Attention Light remains a deterministic graph-selection engine.

In Fast mode, a configured DecisionProvider may produce a policy:

```text
pi_t = DecisionProvider(compressed_state_t)
```

Attention propagation may then become policy-conditioned:

```text
activation_next
  = activation_current
    * edge_weight(pi_t)
    * direction_weight
    * decay(pi_t)
    * reliability
    * relevance
```

The framework chooses the mode.

The configured Fast provider supplies bounded policy judgment.

Attention Light performs the actual graph search.

Deployments without Fast mode remain valid because the deterministic Attention engine remains independently usable.

## 21. Status

| Capability | Status |
|---|---|
| Current deterministic Attention Light | Implemented |
| Current Active Promotion | Implemented |
| Cognitive Git | Implemented |
| Cognitive Profile / lightweight Model Catalog | Planned |
| Framework Cognitive Router | Planned |
| DecisionProvider interface | Planned |
| Jev decision provider | Planned |
| Laya decision provider | Planned |
| Think provider contract | Planned |
| Dynamic Think effort policy | Planned |
| Progress Monitor / failure signatures | Planned |
| Work Unit structure | Planned |
| Task Profiler + deterministic catalog resolver | Planned |
| Automatic model-speed telemetry | Planned |
| Provider health per configured route | Planned |
| Graph Governor Analyzer | Planned |
| Graph Governor Curator/Planner | Planned |
| Graph mutation validator | Planned |
| Hot/warm/cold graph tiers | Planned |
| Global canonicalization | Planned |
| Cortex Epochs | Planned |

Recommended implementation order:

```text
1. Cognitive Profile / Model Catalog schema
2. Task Profiler + deterministic catalog resolver
3. automatic model-speed telemetry
4. Progress Monitor + failure signatures
5. Framework Cognitive Router with Algorithm/Fast/Think modes
6. DecisionProvider interface
7. Laya/Jev DecisionProvider adapters
8. Think provider contract
9. Dynamic Think-effort policy + provider adapter mapping
10. Graph Governor Analyzer
11. GraphMutationPlan + Validator
12. Governor configured model integration
13. hot/warm/cold tiers + Cortex Epochs
```

The deterministic Algorithm and Attention paths must remain independently usable throughout the migration.
