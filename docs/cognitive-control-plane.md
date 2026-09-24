# Cognitive control plane

This document defines the target architecture for adaptive cognition, model selection, failure escalation, and long-horizon Context Graph governance in LumenCortex.

> **Status:** Planned architecture. The current runtime already implements the Context Graph, Attention Light, Active Promotion, Agent Loop, Workflow Contracts, provider abstraction, verification, and Cognitive Git. The control-plane components below are not yet claimed as implemented.

Related documents:

- [Architecture](architecture.md)
- [Attention Light algorithm](attention-light-algorithm.md)
- [Agent runtime](agent-runtime.md)
- [Execution flow](execution-flow.md)
- [ADR-0003 — Adaptive cognitive control plane](adr/0003-adaptive-cognitive-control-plane.md)

## 1. Design goal

LumenCortex needs three different kinds of cognition:

1. **Fast local judgment** — routing, retrieval policy, budget, stop/continue, confidence, and simple classification.
2. **Deliberative task reasoning** — diagnosis, hypothesis revision, planning, and recovery from unknowns or repeated failure.
3. **Long-horizon Cortex governance** — pruning, branching, promotion, canonicalization, global summarization, tiering, and cognitive versioning.

These operate on different time scales and must remain separate.

The architecture therefore uses five roles:

| Component | Time scale | Main responsibility |
|---|---|---|
| Cognitive Kernel | every step | deterministic control, constraints, budgets, validation |
| Light Controller | fast | typed local decisions over compressed state |
| Think | on escalation | task-local deliberate reasoning and strategy revision |
| Model Broker | per Work Unit / switch event | select an execution model from capability requirements |
| Graph Governor | periodic / global | maintain the long-term structure of the Context Graph |

## 2. Core invariants

```text
Graph is Memory.
Light is Attention.
System One is Fast Judgment.
Think is Deliberation.
Governor is Cognitive Maintenance.
Agent is Execution.
```

The runtime follows these rules:

1. Models may propose judgments, strategies, or graph plans.
2. Deterministic runtime code owns legality, budgets, permissions, persistence, and execution.
3. Think does not own long-term graph maintenance.
4. Graph Governor does not own the current task strategy.
5. The Model Broker does not decompose tasks.
6. Tasks and cognitive roles do not bind directly to model names.
7. Durable graph mutation must remain provenance-preserving and auditable through Cognitive Git.

## 3. Target topology

```text
                         User Goal
                            |
                            v
                    +------------------+
                    | Cognitive Kernel |
                    +---------+--------+
                              |
         +--------------------+--------------------+
         |                    |                    |
         v                    v                    v
 +----------------+   +----------------+   +----------------+
 | Light Controller|  | Think          |   | Graph Governor |
 | System One      |  | System Two     |   | Cortex Curator |
 +-------+---------+   +-------+--------+   +-------+--------+
         |                     |                    |
         | LightPolicy         | Strategy           | GraphPlan
         | Work requirements   | WorkUnits          |
         +----------+----------+                    |
                    v                               |
             +--------------+                      |
             | Model Broker |                      |
             +------+-------+                      |
                    |                              |
                    v                              |
             Execution Model                       |
                    |                              |
                    v                              |
               Agent Loop                          |
                    |                              |
                    v                              |
            Tools / Workspace                      |
                    |                              |
                    v                              |
              Verification                         |
                    |                              |
          +---------+----------+                   |
          |                    |                   |
          v                    v                   v
   Progress Monitor      Outcome Learner      Graph Validator
          |                    |                   |
          +---------+----------+-------------------+
                    |
                    v
               Context Graph
                    |
           +--------+--------+
           |        |        |
          Hot      Warm     Cold
                    |
                    v
               Cognitive Git
```

## 4. Cognitive Kernel

The Cognitive Kernel is deterministic orchestration code.

It owns:

- Workflow and permission constraints,
- token, cost, and latency budgets,
- legal cognitive-mode transitions,
- model/provider availability,
- model-switch hysteresis,
- validation of model-proposed policies,
- graph-mutation authorization,
- persistence and audit records.

It does not answer semantic questions.

The control invariant is:

```text
model proposes
    -> kernel validates
        -> engine executes
            -> verifier records outcome
```

## 5. Light Controller

### 5.1 Role

The Light Controller performs high-frequency bounded decisions over compressed runtime state.

Typical decisions:

- which retrieval mode to use,
- whether current evidence is sufficient,
- whether Light should expand,
- whether Contrarian or Anomaly Light should run,
- whether the agent is making progress,
- whether a failure is local or reasoning-level,
- whether to remain fast or escalate to Think,
- which budget class is appropriate.

It does not produce long free-form plans.

### 5.2 DecisionProvider

The intelligence may be model-based, but the role is modular through a typed interface.

```ts
interface DecisionProvider {
  decide(input: {
    state: DecisionState
    questions: TypedQuestion[]
    signal?: AbortSignal
  }): Promise<DecisionResult>
}
```

Possible implementations:

```text
DecisionProvider
├── HeuristicDecisionProvider
├── JevDecisionProvider
├── LayaDecisionProvider
└── LLMDecisionProvider
```

Jev and Laya are treated as candidate **decision-model implementations**, not as generative Agent providers.

Conceptually:

```text
D : State -> ProbabilityDistribution(Actions)
```

A result should retain distributions instead of immediately discarding them into one argmax.

Example:

```json
{
  "cognitive_mode": {
    "fast": 0.72,
    "expand_light": 0.18,
    "think": 0.09,
    "deep_think": 0.01
  },
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

The Cognitive Kernel owns thresholding and final legal transitions.


### 5.3 Decision paths

Fast decision is not a strict model hierarchy and is not defined as a downgrade chain.

The Cognitive Kernel selects among multiple **decision paths** that implement the same control contract:

```text
                    Decision Request
                           |
                           v
                   +----------------+
                   | Decision Router|
                   +-------+--------+
                           |
          +----------------+----------------+
          |                |                |
          v                v                v
   Decision Model A  Decision Model B  Algorithm Path
      Jev/Laya        other model      deterministic
          |                |                |
          +----------------+----------------+
                           |
                           v
                     DecisionResult
```

A deployment may have:

- Jev only,
- Laya only,
- both Jev and Laya,
- another compatible decision model,
- no decision model at all,
- deterministic algorithm only.

All are valid configurations.

The router considers:

- provider/model availability,
- decision type,
- expected latency,
- expected cost,
- privacy/locality requirements,
- need for semantic judgment,
- whether the state can be decided reliably from deterministic runtime signals,
- recent provider health.

An algorithm path is therefore not necessarily a last resort. For decisions that are already determined by runtime state, the algorithm path should be preferred even when a model is available.

Examples:

```text
HTTP 429 retry window known
    -> algorithm

token pressure above hard ceiling
    -> algorithm

choose causal vs historical retrieval from ambiguous semantic state
    -> decision model

provider unavailable but local decision model exists
    -> another decision-model path

no decision model configured
    -> algorithm where sufficient
    -> Think where semantic judgment is required
```

### 5.4 Availability and semantic uncertainty

Availability and uncertainty are separate routing signals.

```text
provider unavailable
    -> choose another available Decision Path

decision can be made deterministically
    -> choose Algorithm Path

semantic uncertainty is high
    -> Think / deeper evidence gathering
```

Low confidence, high normalized entropy, a small top-two margin, or conflicting judgments are not provider failures.

They are information about the task state and should remain visible to the Cognitive Kernel.

### 5.5 Decision-path health

The Cognitive Kernel maintains operational health independently for each model-backed decision path.

A model-backed path may be temporarily excluded after repeated operational failures:

```text
healthy
  -> repeated operational failures
temporarily unavailable
  -> cooldown / bounded probe
healthy again when probe succeeds
```

Health affects path eligibility, not semantic capability estimates.

### 5.6 Algorithm decision path

The deterministic Decision Path uses observable runtime state such as:

- repeated-failure count,
- progress delta,
- retrieval coverage,
- contradiction count,
- evidence sufficiency,
- token pressure,
- Workflow/action constraints,
- hard safety and permission rules.

Its purpose is to make decisions that are truly derivable from runtime state.

It must not fabricate semantic judgment. If a decision requires meaning that the algorithm cannot derive, the Kernel selects a model-backed path or Think.

## 6. Think

Think is task-local deliberate reasoning.

Typical triggers:

- repeated equivalent failure,
- low progress over multiple steps,
- important unknowns,
- contradictory evidence,
- retrieval exhaustion,
- high-risk mutation,
- high Light-Controller uncertainty,
- strategy churn,
- failed verification after plausible execution.

Think should return a structured strategy rather than take over the runtime indefinitely.

Conceptual output:

```yaml
diagnosis:
  hypotheses:
    - transaction lifetime is wrong
    - the current test double hides the real path

missing_evidence:
  - production call path
  - transaction boundary

work_units:
  - goal: inspect the transaction caller chain
    requirements:
      reasoning: high
      repository_navigation: high
      tool_calling: required

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

The preferred flow is:

```text
Fast -> Think -> new strategy / policy -> Fast
```

rather than keeping all later steps permanently in the expensive reasoning mode.

## 7. Progress Monitor and escalation

Failure count alone is not enough. The runtime should classify failure and progress.

### 7.1 Failure classes

```text
execution_failure
    -> retry or model-switch candidate

retrieval_failure
    -> expand/change Light

reasoning_failure
    -> Think

environment_failure
    -> diagnose runtime/environment

unknown
    -> Think or targeted evidence gathering
```

Equivalent failures should be grouped by a normalized signature.

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

### 7.2 Value of computation

Cognitive-mode selection is a metareasoning problem.

For mode `m` in state `s`:

```text
V(m | s)
  = E[task_utility | m, s]
    - lambda * compute_cost(m)
    - mu     * latency(m)
    - rho    * switching_cost(m)
```

Escalate when the expected marginal value justifies additional computation:

```text
Think if:

V(Think | s) > V(Fast | s) + escalation_margin
```

This is the target principle even if the first implementation uses simpler thresholds.

### 7.3 Decision uncertainty

For a System-One distribution `p` over `K` actions:

```text
normalized_entropy(p)
  = - sum_i p_i * log(p_i) / log(K)
```

Also track the top-two margin:

```text
margin(p) = p_top1 - p_top2
```

High entropy or a small margin means the fast controller itself is uncertain and is therefore an escalation signal.

## 8. Work Units and Capability Contracts

Think, user intent, or a fast controller may produce Work Units.

A Work Unit specifies what is needed without naming a concrete model.

```yaml
work_unit:
  goal: repair the transaction bug

  requirements:
    tool_calling: required
    code_edit: true
    reasoning: high
    repository_navigation: high
    context: medium

  risk: medium

  verification:
    - focused_reproduction
    - unit_test
```

The invariant is:

```text
task -> Work Unit -> Capability Contract -> Model Broker -> model
```

not:

```text
task -> hard-coded model
```

## 9. Model Broker

### 9.1 Hard filtering

A model must first satisfy hard requirements:

- tool calling,
- structured output,
- vision when required,
- context capacity,
- privacy/local-only constraints,
- provider availability,
- allowed data boundary.

Models that fail a hard constraint are excluded.

### 9.2 Learned capability profile

Soft capability should be learned from verified outcomes.

For model `m` and capability/task family `c`, a simple baseline is:

```text
p(m, c) ~ Beta(alpha_m,c, beta_m,c)

verified success:
    alpha <- alpha + 1

verified failure:
    beta  <- beta + 1

expected_success
    = alpha / (alpha + beta)
```

Profiles should be keyed by concrete model/version so new versions do not inherit unjustified confidence.

### 9.3 Broker utility

For model `m` and Work Unit `w`:

```text
U(m, w)
  = P(success | m, w) * value(w)
    - lambda * expected_cost(m, w)
    - mu     * expected_latency(m, w)
    - nu     * risk(m, w)
```

Select:

```text
m* = argmax_m U(m, w)
```

### 9.4 Model stickiness

Switching models has cost.

If `m0` is current and `m1` is a candidate:

```text
switch only if:

U(m1, w) - U(m0, w) > switch_margin
```

This prevents model churn when another model is only marginally better.

## 10. Graph Governor

### 10.1 Role

The Graph Governor is a separate long-horizon cognitive subsystem.

Think optimizes the current task.

Graph Governor optimizes the durable Cortex across tasks and sessions.

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

### 10.2 Internal decomposition

```text
Graph Governor
├── Analyzer
│   deterministic metrics and candidate generation
├── Curator
│   semantic/model judgment
├── Planner
│   GraphMutationPlan generation
├── Validator
│   deterministic invariant checks
└── Executor
    graph + Cognitive Git operations
```

The Governor model never writes durable graph state directly.

### 10.3 Governance operations

The Governor may propose:

- **prune/tier** — hot -> warm -> cold -> archive,
- **branch** — preserve competing hypotheses or incompatible interpretations,
- **promote** — create higher-level abstractions,
- **merge/canonicalize** — identify aliases or duplicate concepts without erasing provenance,
- **reweight** — propose relation-weight changes subject to policy,
- **summarize** — create structured global Cortex summaries,
- **epoch/version** — create a major cognitive version boundary after structural reorganization.

Deletion should be rare. Archival, tiering, and provenance-preserving canonicalization are preferred.

### 10.4 Global summary

A Governor summary should be structured graph state, not only prose.

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

These summaries may themselves become high-level graph nodes so Attention Light can retrieve global structure cheaply.

## 11. Graph metrics for Governor candidate generation

The Analyzer should narrow the search space before model judgment.

Useful signals include:

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

A simple node-value model may be:

```text
Value(node)
  = a * activation_frequency
  + b * retrieval_contribution
  + c * structural_centrality
  + d * evidence_quality
  - e * staleness
```

Low-value nodes become prune/tier candidates, not automatic deletion targets.

## 12. Branching and conflict

For two graph regions `C1` and `C2`, a conflict candidate can be estimated by:

```text
Conflict(C1, C2)
  = weighted_contradiction_edges(C1, C2)
    / weighted_cross_edges(C1, C2)
```

A high score means “consider a cognitive branch”, not “branch automatically”.

The Curator should distinguish:

- genuinely competing hypotheses,
- historical phase change,
- alias/duplicate confusion,
- bad ingestion,
- temporary stale evidence.

## 13. Promotion and compression

Local Active Promotion remains useful during a task.

The Governor adds global promotion across sessions.

A useful target criterion is an MDL-style gain:

```text
PromotionGain
  = Cost(children)
    - (
        Cost(abstraction)
        + Cost(references)
        + InformationLoss
      )
```

Promotion is attractive when it reduces cognitive description cost without destroying the ability to drill back to evidence.

The first implementation does not need exact information-theoretic optimality; this formula defines the design direction.

## 14. Cognitive epochs

Cognitive Git versions ordinary graph mutations.

A **Cortex Epoch** represents a larger semantic reorganization.

Possible triggers:

- large-scale stale abstractions,
- major architecture changes,
- accumulated canonicalization,
- persistent retrieval degradation,
- graph density beyond configured limits,
- hot/warm/cold restructuring.

An epoch should record:

- source Cognitive Git commit,
- Governor plan,
- validation report,
- structural metrics before/after,
- resulting root abstractions,
- rollback target.

Epoch creation must be reversible.

## 15. Relationship to Attention Light

Attention Light remains a deterministic graph-selection engine.

The Light Controller may produce a policy:

```text
pi_t = DecisionProvider(compressed_state_t)
```

Attention propagation can then become policy-conditioned:

```text
activation_next
  = activation_current
    * edge_weight(pi_t)
    * direction_weight
    * decay(pi_t)
    * reliability
    * relevance
```

The controller chooses how to search.

Attention Light performs the actual search.

The current fixed Attention algorithm remains an independently valid deterministic path when no adaptive controller is configured.

## 16. Status and implementation sequence

| Capability | Status |
|---|---|
| Current deterministic Attention Light | Implemented |
| Current Active Promotion | Implemented |
| Cognitive Git | Implemented |
| DecisionProvider interface | Planned |
| Multiple model-backed Decision Paths | Planned |
| Deterministic Algorithm Decision Path | Planned |
| Decision-path availability / health routing | Planned |
| Jev decision provider | Planned |
| Laya decision provider | Planned |
| Progress Monitor / failure signatures | Planned |
| Think escalation contract | Planned |
| Work Unit / Capability Contract | Planned |
| Model Broker | Planned |
| Verified capability learner | Planned |
| Model stickiness | Planned |
| Graph Governor Analyzer | Planned |
| Graph Governor Curator/Planner | Planned |
| Graph mutation validator | Planned |
| Hot/warm/cold graph tiers | Planned |
| Global canonicalization | Planned |
| Cortex Epochs | Planned |

Recommended implementation order:

```text
1. Progress Monitor + failure signatures
2. Work Unit + Capability Contract
3. Model Broker with static hard capabilities
4. DecisionProvider + Algorithm Decision Path
5. Decision Router + path availability/health
6. Laya/Jev DecisionProvider adapters
7. Think escalation contract
8. Outcome-based capability learning
9. Graph Governor Analyzer
10. GraphMutationPlan + Validator
11. Governor model integration
12. hot/warm/cold tiers + Cortex Epochs
```

The deterministic Algorithm Decision Path must remain independently usable throughout the migration.
