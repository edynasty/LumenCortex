# LumenCortex Architecture — v0.6 SQLite cognitive runtime

LumenCortex is a versioned cognitive runtime and coding-agent loop.

The central invariant is:

```text
Reality -> Evidence -> Belief -> Cognitive Graph
                                  |
                           Retrieval / Light
                                  |
                           Active Subgraph
                                  |
                                 LLM
                                  |
                           Tool / Mutation
                                  |
                         Verify -> Commit
```

The graph is the durable cognitive subject. An LLM invocation is a temporary computation over a finite working set.

## Status legend

- **Implemented** — code + automated tests exist.
- **Partial** — useful implementation exists, but the discussed architecture is not complete.
- **Planned** — architecture target only; not represented as a finished feature.

## System architecture

```text
TUI / CLI / API
        |
        v
+------------------------------------------------------+
| Agent Runtime                                        |
| Goal -> Agent Loop -> Tool Working Set -> Executor  |
|        |             -> Verifier                     |
|        +-> Session Store (full durable history)      |
|        +-> Working-Set Pager (bounded recent rounds) |
+----------------------+-------------------------------+
                       |
                       v
+------------------------------------------------------+
| Cognitive Control                                    |
|                                                      |
| [BM25 + Symbol Retrieval] -> [Attention Light]        |
|                         |                            |
|                 Active Subgraph                      |
|                         |                            |
|              Active Promotion Controller             |
|                ^                 |                   |
|          drill-down            promote               |
+----------------------+-------------------------------+
                       |
                       v
+------------------------------------------------------+
| Versioned Cognitive Graph                            |
| Reality snapshot / Evidence / Belief / Negative      |
| Entity / Abstraction / Task                          |
|                                                      |
| Attention: activate / propagate / budget-cut         |
| Mutation: promote; graft via Cognitive Git            |
| split / generic prune / canonicalize*                 |
+----------------------+-------------------------------+
                       |
                       v
+------------------------------------------------------+
| Cognitive Git                                        |
| commit / branch / checkout / merge / conflict        |
| revert / blame / cherry-pick / rebase                |
+----------------------+-------------------------------+
                       |
                       v
+------------------------------------------------------+
| Storage                                              |
| SQLite WAL: graph / sessions / Cognitive Git / journal|
| FTS5 + symbol index / vector index* / GC*             |
+------------------------------------------------------+

* planned or partial
```

## Working principle

Each reasoning step is deliberately finite:

```text
1. Observe current goal + recent tool evidence
                   |
                   v
2. Recompute Attention Light
                   |
                   v
3. Build Active Subgraph under token budget
                   |
          +--------+---------+
          |                  |
          v                  v
   pressure/density low   pressure/density high
          |                  |
          |             Active Promotion
          |             create parent abstraction
          |             keep all child detail
          +--------+---------+
                   |
                   v
4. Build bounded LLM working set
   - system policy
   - current Active Subgraph
   - current user goal
   - only recent complete tool rounds
   - only task-relevant tool schemas when an allowlist is supplied
                   |
                   v
5. LLM reasoning -> tool_calls
                   |
                   v
6. Execute read/edit/shell/test/context tools
                   |
                   v
7. Workspace changed?
       | yes
       v
   re-ingest reality
   Evidence changes -> dependent Beliefs become stale
                   |
                   v
8. Move Light and iterate
                   |
             done / max steps
                   |
                   v
9. Persist full Session + optional Cognitive Commit
```

The full session can keep growing on disk, but it is not replayed wholesale into every model request.

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
| Persistent retrieval index | Implemented (FTS5 + symbols) |
| Cached adjacency / segmented index updates | Planned |
| LSP semantic tooling | Implemented (stdio JSON-RPC; Java/TS/Python defaults + custom config) |
| Graph canonicalization / GC / hot-warm-cold storage | Planned |
| Temporal valid_from/valid_to graph | Partial |
| Negative-evidence lifecycle | Partial |
| Cognitive branch <-> real Git worktree binding | Planned |
| Transactional rollback of workspace file edits | Planned |
| MCP client/tools | Implemented (2026 modern + legacy; stdio + HTTP) |
| Focused Subagents | Implemented |
| Multi-session parallel runner | Implemented (safe read parallel; write parallel explicit opt-in) |
| TUI | Implemented |
| Skills / vision / browser | Planned |
| Attention propagation | Implemented |
| Ephemeral attention cut under token budget | Implemented |
| Structural cut / restore | Implemented: edge remains durable but is excluded from propagation |
| Structural graft edge | Implemented |
| Cross-branch graft | Implemented through merge/cherry-pick |
| Automatic split/merge/canonicalization controller | Planned |

## Long-task design

The long-task invariant is:

> Durable history may be unbounded; active model attention must remain bounded.

The automated long-loop test executes 20 tool rounds followed by a final model turn and verifies:

- Attention is recomputed every reasoning step.
- Full tool history remains stored in the session.
- Only recent complete assistant/tool rounds are sent to the model.
- Tool messages are never kept without their matching assistant tool call.

This deterministic test proves bounded runtime mechanics. In addition, the strict VM test now proves the graph-memory path with a real Qwen3 4B model: four read observations across four reasoning turns are all present again in the write step Active Subgraph while recentRounds=1, followed by independent verifier success. Real-world coding quality on large repositories remains a separate benchmark dimension.
