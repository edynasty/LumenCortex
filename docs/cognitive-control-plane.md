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
| Cognitive Profile | configuration | bind modes/roles to concrete providers and models |

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
3. A concrete model is selected by configuration, not by runtime model competition.
4. Jev/Laya are interchangeable Fast-mode implementations behind a typed `DecisionProvider`.
5. GPT/Claude/DeepSeek/local reasoning models are interchangeable Think-mode implementations behind a reasoning-provider contract.
6. Think reasoning intensity is selected dynamically by the framework, within configured bounds.
7. Graph Governor uses its own configured model binding and does not share task-local Think responsibility.
7. Models may propose judgments, strategies, or graph plans; deterministic runtime code owns legality, budgets, persistence, and execution.
8. Durable graph mutation must preserve provenance and remain auditable through Cognitive Git.

## 3. Two separate decisions

The architecture separates **mode routing** from **model configuration**.

```text
                     Cognitive Request
                            |
                            v
                  +--------------------+
                  | Framework Router   |
                  | deterministic      |
                  +---------+----------+
                            |
        +-------------------+-------------------+
        |                   |                   |
        v                   v                   v
   Algorithm Mode        Fast Mode          Think Mode
   deterministic       DecisionProvider    ReasoningProvider
                            |                   |
                     configured binding    configured binding
                            |                   |
                    Jev / Laya / ...      GPT / Claude / ...
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

The configured Cognitive Profile answers:

```text
"Which provider/model implements that mode in this deployment?"
```

## 4. Cognitive Profile

A Cognitive Profile is configuration, not learned routing policy.

Example:

```yaml
cognition:
  fast:
    enabled: true
    provider: laya
    model: convaiinnovations/laya

  think:
    enabled: true
    provider: anthropic
    model: claude-sonnet
    effort:
      policy: adaptive
      min: low
      max: high

  execution:
    provider: openai-compatible
    model: coding-model

  governor:
    enabled: true
    provider: openai
    model: reasoning-model
```

Equivalent profiles are valid:

```yaml
fast:
  provider: jev

think:
  provider: openai
  model: gpt-*
```

or:

```yaml
fast:
  enabled: false

think:
  provider: anthropic
  model: claude-*
```

No Jev/Laya installation is required for LumenCortex to function.

### 4.1 Configuration precedence

A future implementation should resolve bindings deterministically:

```text
explicit run override
    > project profile
    > user profile
    > built-in default
```

This is configuration resolution, not automatic model selection.

### 4.2 Route availability

A mode is eligible only when its required binding is configured and operational.

```text
Algorithm
    always available

Fast
    available if a DecisionProvider is configured and healthy

Think
    available if a ReasoningProvider is configured and healthy

Think effort range
    available when Think is configured; bounded by the profile
```

If one route is unavailable, the framework re-evaluates the remaining modes. It does not silently choose an unrelated model.

## 5. Cognitive Kernel / Router

The Cognitive Kernel is deterministic orchestration code.

It owns:

- cognitive-mode selection,
- Workflow and permission constraints,
- token, cost, and latency budgets,
- route availability,
- legal mode transitions,
- provider health visibility,
- validation of model-proposed policies,
- graph-mutation authorization,
- persistence and audit records.

It does not answer semantic questions itself.

The control invariant is:

```text
framework selects mode
    -> configured provider executes that mode
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
V(m | s, c)
  = E[task_utility_gain | m, s]
    - lambda * expected_compute_cost(m, c)
    - mu     * expected_latency(m, c)
    - rho    * transition_cost(m, s)
    - nu     * risk(m, s)
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
    -> DeepThink
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

The configured model remains fixed for the Think role. The framework chooses the effort for each Think invocation.

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

The Cognitive Profile supplies bounds, not the per-request value:

```yaml
think:
  provider: anthropic
  model: claude-sonnet
  effort:
    policy: adaptive
    min: low
    max: high
```

Provider adapters translate the framework's abstract effort into controls supported by that provider/model.

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
| Cognitive Profile / mode bindings | Planned |
| Framework Cognitive Router | Planned |
| DecisionProvider interface | Planned |
| Jev decision provider | Planned |
| Laya decision provider | Planned |
| Think provider contract | Planned |
| Dynamic Think effort policy | Planned |
| Progress Monitor / failure signatures | Planned |
| Work Unit structure | Planned |
| Provider health per configured role | Planned |
| Graph Governor Analyzer | Planned |
| Graph Governor Curator/Planner | Planned |
| Graph mutation validator | Planned |
| Hot/warm/cold graph tiers | Planned |
| Global canonicalization | Planned |
| Cortex Epochs | Planned |

Recommended implementation order:

```text
1. Cognitive Profile + deterministic config resolution
2. Progress Monitor + failure signatures
3. Framework Cognitive Router with Algorithm/Fast/Think modes
4. DecisionProvider interface
5. Laya/Jev DecisionProvider adapters
6. Think provider contract + configurable binding
7. Dynamic Think-effort policy + provider adapter mapping
8. Graph Governor Analyzer
9. GraphMutationPlan + Validator
10. Governor configured model integration
11. hot/warm/cold tiers + Cortex Epochs
```

The deterministic Algorithm and Attention paths must remain independently usable throughout the migration.
