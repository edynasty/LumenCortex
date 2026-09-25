# Cognitive control plane

This document defines the target architecture for adaptive cognitive-mode routing, configurable model bindings, failure-aware reasoning, and long-horizon Context Graph governance in LumenCortex.

> **Status:** Implemented baseline with remaining scale/parity work. The Node.js reference runtime now implements the Decision Layer, ordered Category model chains, framework-owned Think routing, provider-specific reasoning-effort mapping, Progress Monitor, provider circuit breakers, persistent Work Units, semantic Graph Governor planning, provenance-preserving canonicalization/branch/promotion, reversible Cortex Epochs, and indexed hot/warm/cold storage metadata with safe derived-cache compaction. Live Jev/Laya calibration and physical data-plane tier separation/GC remain incomplete. The Go runtime now has a control-plane parity baseline for routing, Decision providers, Category chains, Think effort, circuit breakers, persistent Work Units, shared cognition-profile loading, and read-only Graph Governor analysis/planning/validation; Graph Governor mutation/execution remains owned by the Node reference runtime.

Related documents:

- [Architecture](architecture.md)
- [Attention Light algorithm](attention-light-algorithm.md)
- [Agent runtime](agent-runtime.md)
- [Execution flow](execution-flow.md)
- [ADR-0003 — Adaptive cognitive control plane](adr/0003-adaptive-cognitive-control-plane.md)

## 1. Design goal

LumenCortex separates four concerns that must not be collapsed into one model:

1. **Decision judgment** — fast bounded judgments such as uncertainty, evidence sufficiency, likely task category, progress, and retrieval direction.
2. **Framework routing** — deterministic runtime authority that decides what the system does next, including whether deliberate Think is needed and how much reasoning effort to spend.
3. **Category model chains** — user-configurable ordered lists of generative models for different kinds of work.
4. **Long-horizon Cortex governance** — pruning, branching, promotion, canonicalization, summarization, tiering, and cognitive versioning.

The target components are:

| Component | Responsibility |
|---|---|
| Cognitive Kernel / Router | final deterministic routing, budgets, legality, Think decision, reasoning effort |
| Decision Layer | advisory fast judgments from algorithm/Jev/Laya-compatible providers |
| Category Resolver | choose an ordered generative-model chain from the selected Category |
| Think | deliberate reasoning using the selected generative model and framework-selected effort |
| Graph Governor | long-horizon Context Graph maintenance |
| Cognitive Profile | configure Decision providers and Category model chains |

## 2. Core invariants

```text
Graph is Memory.
Light is Attention.
Decision is Judgment.
Think is Deliberation.
Category is Model Preference.
Governor is Cognitive Maintenance.
Agent is Execution.
```

The runtime follows these rules:

1. **The framework owns the final decision.**
2. Jev/Laya do not execute the task and do not directly decide that the runtime must Think.
3. Jev/Laya are Decision Layer implementations that return bounded judgments/signals to the framework.
4. Category configuration contains ordered **generative model chains** only; it does not need a `mode` field.
5. Category selection and Think effort are separate framework decisions.
6. Think effort is dynamic and framework-controlled.
7. A Category may contain multiple models. List order is explicit user preference.
8. Category models generate/reason/execute through the Agent runtime; Decision Layer models do not.
9. Models may propose judgments, strategies, or graph plans; deterministic runtime code owns legality, budgets, persistence, and durable mutation.
10. Durable graph mutation must preserve provenance and remain auditable through Cognitive Git.

## 3. Target control flow

```text
                        Current State
                             |
              +--------------+--------------+
              |                             |
              v                             v
       Runtime Metrics                Decision Layer
       deterministic              algorithm / Jev / Laya
              |                             |
              +--------------+--------------+
                             |
                             v
                    Cognitive Kernel
                    Framework Router
                             |
                +------------+------------+
                |                         |
                v                         v
          Category choice           Think decision
                                      + effort
                |                         |
                +------------+------------+
                             |
                             v
                    Category Resolver
                             |
                             v
                 Ordered Model Chain
                 [Model A, B, C, ...]
                             |
                             v
                 first eligible/healthy
                     generative model
                             |
                             v
                    Agent / Think / Tools
```

The Decision Layer is advisory.

The Cognitive Kernel consumes:

- deterministic runtime state,
- Decision Layer judgments,
- Progress Monitor state,
- Graph/Light state,
- Workflow constraints,
- provider health,
- current budgets.

It then makes the final routing decision.

## 4. Decision Layer

Decision Layer models are not task-execution models.

Typical questions are bounded and typed:

- what Category best describes the current work,
- whether evidence is sufficient,
- whether the current strategy is stuck,
- whether retrieval should become causal/historical/dependency-oriented,
- whether ambiguity is high,
- whether observed progress is meaningful.

### 4.1 DecisionProvider

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
├── AlgorithmDecisionProvider
├── JevDecisionProvider
├── LayaDecisionProvider
└── other compatible decision model
```

Jev and Laya are interchangeable implementations of this layer, not entries in Category model chains.

Conceptually:

```text
D : compressed_state -> probability distributions / typed judgments
```

Example:

```json
{
  "category": {
    "general": 0.11,
    "deep": 0.38,
    "visual-engineering": 0.04,
    "research": 0.07,
    "ultrabrain": 0.40
  },
  "evidence_sufficient": 0.22,
  "stuck": 0.81,
  "retrieval": {
    "causal": 0.64,
    "historical": 0.12,
    "dependency": 0.24
  }
}
```

The framework may accept, override, or ignore these judgments.

### 4.2 Decision uncertainty

For a probability distribution `p` over `K` answers:

```text
normalized_entropy(p)
  = - sum_i p_i * log(p_i) / log(K)

margin(p)
  = p_top1 - p_top2
```

High entropy, low margin, conflicting answers, or unstable repeated judgments are themselves useful framework signals.

They are not task execution and are not direct commands to enter Think.

### 4.3 No Decision model is required

A deployment may use:

- algorithm only,
- Jev only,
- Laya only,
- multiple DecisionProviders,
- another compatible model.

The deterministic framework remains valid without Jev/Laya.

## 5. Category model chains

A Category describes a kind of generative work and provides an ordered model preference chain.

It does **not** contain `mode: fast/think`.

A minimal configuration is:

```yaml
cognition:
  decision:
    providers:
      - laya
      - jev

  categories:
    quick:
      description: small, bounded tasks with obvious local scope
      models:
        - provider/fast-general
        - provider/general

    general:
      default: true
      description: normal coding and reasoning work
      models:
        - provider/general
        - provider/strong

    deep:
      description: difficult multi-step reasoning and debugging
      models:
        - provider/strong
        - provider/general

    ultrabrain:
      description: exceptionally difficult reasoning or repeated failed strategies
      models:
        - provider/best-reasoning
        - provider/strong

    visual-engineering:
      description: UI, layout, visual implementation and interaction work
      models:
        - provider/visual-specialist
        - provider/general

    research:
      description: broad investigation, evidence gathering and synthesis
      models:
        - provider/research-specialist
        - provider/general
```

Jev and Laya are intentionally absent from `categories.*.models`.

## 6. Category selection

The framework selects a Category from current task/state.

Inputs may include:

- user request,
- current Goal,
- active Work Unit,
- repository context,
- failure history,
- Graph/Light evidence,
- Decision Layer Category distribution,
- previous Category and transition cost.

Built-in Category names may take inspiration from concise work-oriented taxonomies such as:

```text
quick
general
deep
ultrabrain
visual-engineering
research
writing
```

The built-in set should remain small. Users may add custom Categories with a short description and ordered model chain.

### 6.1 Multiple models in one Category

Multiple models are not a scoring problem.

For:

```yaml
deep:
  models:
    - provider/model-a
    - provider/model-b
    - provider/model-c
```

resolution is:

```text
model-a eligible and healthy
    -> model-a

model-a unavailable/incompatible
    -> model-b

model-b unavailable/incompatible
    -> model-c
```

If multiple entries are healthy, the first entry wins.

This makes model choice predictable and lets the user explicitly express preference.

### 6.2 Default Category

If Category classification is uncertain and no stronger framework rule applies:

```text
use category.default == true
```

There must be at most one effective default after configuration precedence is resolved.

### 6.3 Configuration precedence

```text
explicit run/project override
    > project configuration
    > user configuration
    > built-in defaults
```

User-defined Category chains may override built-in chains without changing framework code.

## 7. Runtime model telemetry

Users should not manually describe speed classes.

LumenCortex observes concrete provider/model behavior:

- time to first token,
- total latency,
- output tokens/second,
- tool-round latency,
- timeout/error rate.

A rolling EWMA or percentile summary may be retained.

By default, telemetry does **not** reorder a healthy Category chain. Explicit list order remains authoritative.

Telemetry is useful for:

- timeout selection,
- observability,
- detecting unhealthy routes,
- enforcing an explicit latency budget when one exists,
- future opt-in routing policies.

## 8. Framework Think decision

Think is a framework behavior, not a Category property and not a Jev/Laya command.

The Cognitive Kernel decides whether deliberate reasoning is warranted from runtime state.

Typical signals:

- task complexity,
- important unknowns,
- repeated equivalent failures,
- low progress,
- contradictory evidence,
- retrieval exhaustion,
- high-risk or hard-to-reverse actions,
- Decision Layer uncertainty,
- failed verification after plausible execution.

A target value-of-computation rule is:

```text
Think if:

E[deliberation_gain | state]
  > compute_cost
    + latency_cost
    + transition_cost
```

The first implementation may use deterministic thresholds and bounded heuristics.

A Category can influence model preference, but it does not itself force or disable Think.

## 9. Dynamic Think intensity

Once the framework chooses Think, it independently selects reasoning intensity.

```text
effort in {
  low,
  medium,
  high,
  max
}
```

or internally:

```text
e in [0, 1]
```

A target objective is:

```text
effort* = argmax_e [
    E[deliberation_gain | state, e]
    - lambda * compute_cost(e)
    - mu * latency(e)
]
```

A first implementation may estimate effort from:

```text
z
  = w1 * task_complexity
  + w2 * semantic_uncertainty
  + w3 * repeated_failure
  + w4 * contradiction_density
  + w5 * unknown_density
  + w6 * action_risk
  + w7 * retrieval_exhaustion

effort = discretize(sigmoid(z))
```

The selected generative-model adapter maps the abstract effort to what that provider supports:

- native reasoning-effort controls,
- reasoning/token budget,
- deliberate passes,
- hypothesis count,
- context budget,
- Contrarian review,
- focused Subagents,
- verification depth.

The Category does not need an effort range.

## 10. Think and execution models

Category chains contain normal generative models.

Those models may be used by:

- ordinary Agent execution,
- deliberate Think,
- planning,
- coding,
- review,
- research,
- other generative Work Units.

The distinction is runtime behavior:

```text
same configured model
    + ordinary Agent step
        -> normal generation/execution

same configured model
    + Think selected
        -> deliberate prompt/contract
        -> dynamic effort
        -> structured strategy/hypotheses
```

This keeps Category configuration simple while allowing the same model to serve different cognitive behaviors.

Think should normally return structured strategy/state rather than directly owning persistent graph mutation.

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

Think may decompose a task into persistent Work Units, but Work Units never select models, providers, Categories, or reasoning effort.

```yaml
work_unit:
  id: repair-transaction
  goal: repair transaction bug
  risk: medium
  required_evidence:
    - production call path
  verification:
    - focused_reproduction
    - unit_test
```

The implemented Work Unit runtime provides:

- dependency ordering with cycle detection,
- `pending / active / blocked / verifying / completed / failed / cancelled` state,
- required-evidence gates,
- passed-verification gates,
- persistent Session storage,
- `work_unit_list / work_unit_create / work_unit_update` Agent tools,
- a final-answer gate while non-terminal Work Units remain.

The active Work Unit is included in cognitive routing state, while model selection remains entirely outside the Work Unit contract.

## 13. Provider health

Provider health is operational state, not cognitive competence.

For each configured model-backed route, the implemented shared health registry uses a lightweight circuit breaker:

```text
healthy
  -> repeated operational failures
  -> threshold reached
circuit open
  -> cooldown
  -> bounded probe becomes eligible
successful probe
  -> healthy
```

Current defaults are three consecutive failures and a 30-second cooldown; both are configurable.

Operational failures include:

- timeout,
- provider unavailable,
- authentication failure,
- malformed protocol response,
- incompatible schema.

Semantic uncertainty is different.

A healthy DecisionProvider that returns uncertain probabilities is still healthy. Semantic uncertainty never increments the operational failure circuit.

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

The Governor model never writes durable graph state directly. The implemented Curator receives a bounded candidate summary and returns JSON `GraphMutationPlan`; deterministic validation remains the mandatory gate before execution.

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

The implemented semantic executor is opt-in (`lcx governor apply ... --semantic`):

- canonicalization keeps every alias node, adds `canonicalNodeId`, and creates a `canonicalizes` edge,
- branch creates an abstraction that preserves both competing nodes rather than selecting a winner,
- global promotion creates a higher-level abstraction and `abstracts` edges while retaining all children,
- safe apply without `--semantic` defers these semantic operations.

### 14.4 Storage tiers and safe compaction

SQLite schema v2 adds `graph_node_storage` with indexed:

- `hot / warm / cold` tier,
- last-access time,
- tier-change time,
- archive time,
- derived-cache compaction time,
- access count.

Attention selections update access telemetry without changing the graph revision. Tier changes remain normal graph metadata mutations and therefore remain Cognitive-Git versioned.

`lcx governor compact` only removes derived FTS5/symbol/search rows for retained cold+archived candidates. It does **not** delete Context Graph nodes. Reproduced/runtime evidence is excluded from GC candidates. Cognitive deletion, if introduced later, must remain an explicit graph mutation rather than an out-of-band SQL delete.

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

Epoch creation is implemented as a normal Cognitive Git mutation. An epoch creates a durable `cortex-epoch` abstraction recording source commit, rollback target, reasons, and structural metrics before/after. The same governance mutations and epoch marker are committed together, so `lcx revert <epoch-commit>` reverses the epoch through the existing Cognitive Git diff mechanism.

## 20. Relationship to Attention Light

Attention Light remains a deterministic graph-selection engine.

A configured DecisionProvider in the Decision Layer may produce a bounded Attention policy signal:

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

The framework decides whether and how to use that signal.

The configured DecisionProvider supplies bounded policy judgment only.

Attention Light performs the actual graph search.

Deployments without Jev/Laya or any model-backed DecisionProvider remain valid because deterministic Attention and algorithmic decision paths remain independently usable.

## 21. Status

| Capability | Status |
|---|---|
| Current deterministic Attention Light | Implemented |
| Current Active Promotion | Implemented |
| Cognitive Git | Implemented |
| Cognitive Profile / Category model chains | Implemented baseline |
| Framework Cognitive Router | Implemented baseline |
| DecisionProvider interface | Implemented |
| Jev decision provider | Implemented HTTP adapter; live credentialed Jev validation still needed |
| Laya decision provider | Implemented Jev-compatible HTTP adapter; local live validation still needed |
| Think provider contract | Implemented baseline; existing generative provider contract plus cognitive policy prompt |
| Provider-specific Think-effort adapters | Implemented baseline for OpenRouter, Groq and DeepSeek; generic endpoints remain provider-default |
| Dynamic Think effort policy | Implemented baseline |
| Progress Monitor / failure signatures | Implemented |
| Work Unit structure/runtime | Implemented baseline |
| Category classifier + deterministic chain resolver | Implemented baseline |
| Automatic model-speed telemetry | Implemented baseline (EWMA total latency/failure counts) |
| Provider health / circuit breaker | Implemented baseline for Decision and Category routes |
| Graph Governor Analyzer | Implemented baseline |
| Graph Governor Curator/Planner | Implemented baseline with separately configured generative provider |
| Graph mutation validator | Implemented baseline |
| Governor branch/global promotion | Implemented baseline, provenance-preserving |
| Hot/warm/cold graph tiers | Partial: indexed SQLite storage metadata/access telemetry + derived-cache compaction implemented; separate physical stores are not |
| Global canonicalization | Implemented baseline with alias preservation and `canonicalizes` relation |
| Cortex Epochs | Implemented baseline and reversible through Cognitive Git |
| Go control-plane parity | Implemented baseline: Router, System One Decision Layer, Category chains, Think effort, circuit breaker, Work Units, shared profile; Governor Analyze/Plan/Validate is read-only |
| Go Governor graph mutation/executor | Planned; Node remains the sole mutation authority |

Implemented baseline sequence:

```text
1. DecisionProvider + algorithmic decision path
2. Laya/Jev-compatible System One HTTP adapters
3. Cognitive Profile / ordered Category model chains
4. Category classifier + deterministic chain resolver
5. Progress Monitor + failure signatures
6. Framework Think decision + dynamic Think effort
7. Session-level model latency telemetry
8. Graph Governor Analyzer
9. Graph Governor plan validation + safe tier/archive executor
```

Next control-plane work:

```text
1. live Jev/Laya validation, calibration, and cognitive-routing benchmarks
2. broader provider-specific reasoning-control adapters
3. use storage access/recency telemetry in Governor tier recommendations
4. physical data-plane hot/warm/cold separation only if profiling justifies it
5. explicit cognitive deletion/retention policy built on Cognitive Git, never raw SQL deletion
6. Go Graph Governor executor / Cognitive Graph mutation parity after a single shared transaction authority is defined
```

The deterministic Algorithm and Attention paths must remain independently usable throughout the migration.
