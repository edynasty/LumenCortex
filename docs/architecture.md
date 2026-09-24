# LumenCortex Architecture — v0.6 SQLite cognitive runtime

LumenCortex is a versioned cognitive runtime and coding-agent loop.

The architecture has three separate state domains:

```text
Workflow State
Facts / Action / Route / Outcome / Gate
        │
        │ constrains legal progress
        ▼
Execution State
Agent / LLM / Tools / Shell / LSP / MCP / Sessions
        ▲
        │ receives selected evidence
        │
Cognitive State
Evidence / Belief / Attention Light / Context Graph
```

The graph is the durable cognitive subject. Workflow is the deterministic task-control plane. An LLM invocation is a temporary computation over a finite working set.

For the complete visual map, see [Architecture diagrams](architecture-diagrams.md). For exact per-step semantics, see [Agent execution flow](execution-flow.md).

## Status legend

- **Implemented** — code + automated tests exist.
- **Partial** — useful implementation exists, but the discussed architecture is not complete.
- **Planned** — architecture target only; not represented as a finished feature.

## System architecture

The canonical system-level diagram is maintained in [Architecture diagrams — System architecture](architecture-diagrams.md#system-architecture).

The important subsystem boundaries are:

| Plane | Owns | Must not own |
|---|---|---|
| Workflow | task stage, deterministic Facts, legal tools, completion evidence, Gates | semantic repository memory |
| Cognitive | Evidence, Beliefs, relations, retrieval, Attention, Promotion | authorization or task legality |
| Execution | LLM requests, tools, shell/LSP/MCP, Sessions, cancellation | durable truth by model assertion alone |
| Persistence | SQLite WAL, Sessions, graph, search, Cognitive Git, journal | reasoning policy |

This separation lets Workflow constrain autonomous execution without turning the Context Graph into a state machine, and lets cognition preserve rich evidence without being trusted as an authorization engine.

## Working principle

Every reasoning step is deliberately finite.

```text
1. Load / resume Session and Workflow state
2. Stop immediately if a Human Gate is waiting
3. Derive current focus
4. Retrieve indexed cognitive candidates
5. Move Attention Light
6. Build finite Active Subgraph
7. Build bounded model working set
8. Compute current Workflow/Agent tool working set
9. LLM reasons
10. For every tool call:
      execution-time permission + Workflow check
      -> execute
      -> record observation
      -> evaluate Workflow Outcomes
      -> write Facts
      -> evaluate Routes / Gates
11. Re-ingest workspace reality after mutation
12. Persist Session step and Workflow snapshot
13. If model returns final:
      accept only if terminal Workflow completion is proven
14. Iterate, pause, complete, interrupt, or hit max steps
```

The full Session and Context Graph can keep growing on disk, but neither is replayed wholesale into every model request.

When no Workflow Contract is active, the same cognitive/execution loop runs without deterministic Action/Route/Gate constraints.

The detailed state machine is maintained in [Agent execution flow](execution-flow.md).

## Target cognitive control plane

The next architecture stage separates adaptive cognition into five roles:

| Role | Responsibility | Status |
|---|---|---|
| Cognitive Kernel | deterministic constraints, budgets, legal transitions, validation | Planned |
| Light Controller | fast typed decisions over compressed state | Planned |
| Think | first-class task-local deliberate reasoning route | Planned |
| Model Broker | capability/cost/latency-aware model selection | Planned |
| Graph Governor | long-horizon pruning, branching, promotion, canonicalization, summaries, and Cortex versioning | Planned |

These roles are intentionally separate from the current deterministic Attention Light and Agent Loop. The exact target contracts, cognitive-route selection formulas, model-selection utility, and Graph Governor lifecycle are defined in [Cognitive control plane](cognitive-control-plane.md).

The core boundary is:

```text
models propose judgment / strategy / graph plans
        |
        v
Cognitive Kernel / validators
        |
        v
Attention / Broker / Graph engines
        |
        v
verified execution + durable state
```

Think does not own global graph maintenance. Graph Governor does not own current-task strategy. Model Broker does not decompose tasks, and tasks do not bind directly to concrete model names.

## Attention Light

Implemented attention currently combines:

- lexical relevance,
- graph propagation,
- relation weights,
- trust/evidence quality,
- staleness,
- hop decay,
- token cost.

Policies:

- Exploit
- Explore
- Contrarian
- Anomaly

Important: truth strength and attention strength are separate values. A low-confidence hypothesis can still deserve attention.

The exact implemented formulas, default parameters, relation weights, traversal policy, token-cost utility, and Exploit / Explore / Contrarian / Anomaly behavior are specified in [Attention Light algorithm](attention-light-algorithm.md). This architecture document intentionally keeps only the subsystem-level summary.

### Retrieval pipeline

Seed generation now uses SQLite FTS5 + an exact symbol index. Full-graph lexical seed scanning remains only as a fallback when an index is unavailable. The current pipeline is:

```text
Goal
 |
 +-> Symbol index       [implemented]
 +-> FTS5 / lexical     [implemented]
 +-> Embedding retrieval [planned/optional]
 +-> Graph neighborhood [implemented]
 +-> History / recent evidence
         |
         v
     Candidate Set
         |
         v
   Attention Light
```

This removes the dominant per-query O(N) seed scan. Embeddings remain optional for semantic recall rather than a prerequisite for indexed navigation.

## Active Promotion

Promotion is non-destructive.

```text
Before

Detail A   Detail B   Detail C
  20K        18K        25K

After

        Parent Abstraction
           2-3K view
          /    |    \
         A     B     C
       full  full  full
```

The controller can trigger from:

- context pressure,
- selected-node density,
- unresolved-question density,
- repeated activation / reuse frequency,
- cooldown / duplicate prevention.

Promotion changes cognitive granularity; it is not merely an emergency action at 95% context usage.

## Cognitive Git

Implemented cognition history:

- `commit`
- `branch`
- `checkout`
- `merge`
- explicit merge conflicts
- `revert`
- `blame`
- `cherry-pick`
- `rebase`

The analogy is intentional:

| Git | LumenCortex |
|---|---|
| repository | Cognitive Graph |
| commit | cognitive mutation |
| branch | hypothesis / task path |
| merge | combine findings |
| conflict | incompatible cognition |
| revert | undo bad cognition |
| blame | provenance of a node/edge |
| cherry-pick | graft a useful finding |
| rebase | reinterpret a task branch on a newer cognitive base |

Current Cognitive Git versions the cognitive graph. Binding those branches transactionally to real Git worktrees is still planned.

## Reality / Evidence / Belief

A model statement does not become evidence.

Evidence grades:

```text
hypothesis < static < tested < runtime < reproduced
```

Trust zones include:

```text
system_verified
repo_trusted
runtime_verified
user_provided
external_untrusted
model_inferred
```

Repository re-ingestion uses content hashes. When source evidence changes, dependent beliefs/abstractions are marked stale.

## Agent providers

Implemented provider layer:

- OpenRouter
- Groq
- DeepSeek official
- generic OpenAI-compatible endpoints
- local Ollama/vLLM through generic mode

All providers use the same tool-calling Agent Loop.

## Implementation status

| Capability | Status |
|---|---|
| Persistent Context Graph | Implemented |
| Evidence / Belief separation | Implemented |
| Trust zones / evidence grades | Implemented |
| Source-change invalidation | Implemented |
| Attention propagation + budget | Implemented |
| Multi-light policies | Implemented |
| Light recomputed every agent step | Implemented |
| Bounded model working-set pager | Implemented |
| Full durable session history | Implemented |
| Manual Promotion | Implemented |
| Drill-down through abstraction edges + reseeding | Partial (works through Light; no dedicated API) |
| Active Promotion Controller | Implemented (pressure, density, unresolved, repeated activation, dedupe/cooldown) |
| Commit / Branch / Merge / Conflict / Revert | Implemented |
| Blame / Cherry-pick / Rebase | Implemented |
| Agent Loop / CLI / tools / resume | Implemented |
| Validated multi-file patch batches | Implemented (`apply_patch`; pre-validates exact update/create/delete batches) |
| Agent / shell cancellation | Implemented (AbortSignal propagation, resumable interrupted Session, process-tree termination) |
| Per-run Tool Working Set / schema allowlist | Implemented |
| Per-step tool-call fanout bound | Implemented |
| Provider retry / timeout / empty-turn recovery | Implemented |
| Real local OpenAI-compatible model integration | Implemented; real VM tool-call smoke passed |
| Real-model long-task validation | Implemented and VM-proven with Qwen3 4B; paged-out observations reactivated from graph |
| DeepSeek official/OpenRouter adapters | Implemented |
| Real DeepSeek V4 execution | Not yet verified in CI; no-key HF endpoint was paused and OpenRouter key is absent |
| Persistent SQLite FTS5 + symbol retrieval | Implemented |
| Embedding retrieval | Planned / optional |
| SQLite WAL persistence | Implemented |
| Sparse Cognitive Git checkpoints | Implemented (~50 first-parent commits) |
| Explicit storage connection lifecycle | Implemented |
| Persistent retrieval index | Implemented (FTS5 + symbols) |
| Incremental dirty-node FTS/symbol updates | Implemented |
| Optimistic graph revision conflict detection | Implemented |
| Shared harness SessionStore lifecycle | Implemented |
| Cached adjacency | Planned |
| LSP semantic tooling | Implemented (stdio JSON-RPC; Java/TS/Python defaults + custom config) |
| Graph canonicalization / GC / hot-warm-cold storage | Planned |
| Temporal valid_from/valid_to graph | Partial |
| Negative-evidence lifecycle | Partial |
| Cognitive branch <-> real Git worktree binding | Planned |
| Transactional rollback of workspace file edits | Planned |
| MCP client/tools | Implemented (2026 modern + legacy; stdio + HTTP) |
| Focused Subagents | Implemented |
| Multi-session parallel runner | Implemented (safe read parallel; write parallel explicit opt-in) |
| TUI | Implemented (direct launch, Session switching, live events, Ctrl+C active-run cancellation) |
| Skills / vision / browser | Planned |
| Attention propagation | Implemented |
| Ephemeral attention cut under token budget | Implemented |
| Structural cut / restore | Implemented: edge remains durable but is excluded from propagation |
| Structural graft edge | Implemented |
| Cross-branch graft | Implemented through merge/cherry-pick |
| Automatic split/merge/canonicalization controller | Planned |
| DecisionProvider / Light Controller | Planned |
| Progress Monitor + cognitive route transitions | Planned |
| Work Unit / Capability Contract | Planned |
| Capability-aware Model Broker | Planned |
| Verified model capability learning | Planned |
| Graph Governor | Planned |
| Cortex Epochs | Planned |

## SQLite consistency model

The workspace database is opened in WAL mode. Graph writes use an optimistic revision check:

```text
read graph revision N
        |
mutate cached graph
        |
BEGIN IMMEDIATE
        |
expected revision == current revision?
   | yes                    | no
   v                        v
upsert changed rows      reject stale writer
mark changed nodes dirty
increment revision
        |
incrementally refresh FTS5/symbol rows
```

The Repository caches the current graph snapshot behind the SQLite revision, so repeated reads do not reconstruct the whole graph unless another writer advances the revision. Agent/TUI/Subagent execution inside one harness shares a single SessionStore connection; independent processes can still use WAL with the optimistic graph revision guard.

## Long-task design

The long-task invariant is:

> Durable history may be unbounded; active model attention must remain bounded.

The automated long-loop test executes 20 tool rounds followed by a final model turn and verifies:

- Attention is recomputed every reasoning step.
- Full tool history remains stored in the session.
- Only recent complete assistant/tool rounds are sent to the model.
- Tool messages are never kept without their matching assistant tool call.

This deterministic test proves bounded runtime mechanics. In addition, the strict VM test now proves the graph-memory path with a real Qwen3 4B model: four read observations across four reasoning turns are all present again in the write step Active Subgraph while recentRounds=1, followed by independent verifier success. Real-world coding quality on large repositories remains a separate benchmark dimension.
