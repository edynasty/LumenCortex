# Attention Light algorithm specification

This document is the canonical algorithm reference for LumenCortex cognitive retrieval and Attention Light.

It describes the behavior implemented by the current Node.js reference runtime on `main`. It does not promote planned retrieval techniques to implemented status.

Related documents:

- [Architecture](architecture.md) — subsystem boundaries and cognitive design.
- [Execution flow](execution-flow.md) — where retrieval and Attention Light run during an Agent step.
- [Agent runtime](agent-runtime.md) — runtime integration and persistent search-index behavior.
- [Data model](data-model.md) — graph node, edge, evidence-grade, and trust-zone structures.
- [Cognitive control plane](cognitive-control-plane.md) — implemented baseline adaptive policy above the deterministic Light engine.

## Status

| Capability | Status |
|---|---|
| SQLite FTS5 candidate retrieval | **Implemented** |
| Exact symbol candidate retrieval | **Implemented** |
| Lexical seed scoring | **Implemented** |
| Weighted multi-hop graph propagation | **Implemented** |
| Evidence/trust-aware attention reliability | **Implemented** |
| Token-cost-aware context selection | **Implemented** |
| Exploit / Explore / Contrarian / Anomaly lights | **Implemented** |
| Active Promotion heuristic | **Implemented** |
| Source-change stale invalidation | **Implemented** |
| Persistent embedding retrieval (exact cosine) | **Implemented / optional** |
| Personalized PageRank | **Implemented / optional associative Light** |
| Global PageRank | **Not implemented** |
| Dijkstra/A* shortest-path retrieval | **Not implemented** |
| GNN-based retrieval/ranking | **Not implemented** |

The current algorithm is best described as a **cost-aware, multi-perspective, weighted attention propagation search**. It is a LumenCortex-specific composition rather than one textbook graph algorithm.

## 1. End-to-end retrieval pipeline

The normal runtime path is:

```text
Goal / current focus
        |
        v
+-----------------------+
| Indexed candidate set |
|                       |
| exact symbol lookup   |
| SQLite FTS5 / BM25    |
+-----------------------+
        |
        | default candidateLimit = 64
        v
+-----------------------+
| Lexical seed scoring  |
| + reliability         |
| + node-kind boost     |
+-----------------------+
        |
        | default seedLimit = 8
        v
+-------------------------------+
| Weighted Attention propagation|
| priority / best-first style   |
| bounded by hop + score        |
+-------------------------------+
        |
        v
+-----------------------+
| Activation ranking    |
| / token-cost penalty  |
+-----------------------+
        |
        | finite token budget
        v
Active Subgraph -> model working set
```

The search index is used for **candidate generation**. Its scores do not directly become final graph activation scores. Candidate nodes are re-scored by Attention Light before propagation.

If the persistent index is unavailable, the attention implementation can fall back to lexical scoring across the graph. The normal indexed runtime path avoids that full-graph seed scan.

## 2. Indexed candidate retrieval

### 2.1 Exact symbol lookup

Code-like identifiers are extracted into the persistent `symbols` table.

For each identifier term in the query:

- exact symbol match contributes `+8`,
- case-insensitive symbol match contributes `+5`.

Contributions are additive per node.

This channel is intentionally strong for exact code navigation, such as class, function, method, or variable names.

### 2.2 SQLite FTS5 retrieval

Natural-language and code-token recall uses SQLite FTS5.

The current FTS5 BM25 column weights are:

| FTS column | Weight |
|---|---:|
| `node_id` | 0.0 |
| `title` | 3.0 |
| `body` | 1.0 |
| `path` | 2.0 |
| `tags` | 0.5 |

The query is tokenized, deduplicated, limited to at most 24 FTS terms, and joined using `OR`.

The implementation converts the FTS5 BM25 rank into a positive score contribution and adds it to any symbol-match contribution for the same node.

The final indexed list is sorted by accumulated candidate score and limited by the runtime candidate limit, currently 64 by default.

### 2.3 Incremental index maintenance

Graph mutations mark affected nodes dirty. When the graph revision advances, only dirty FTS5/symbol rows are replaced during normal synchronization.

A full index rebuild is required for an empty/new index, not for every graph mutation.

This candidate stage exists to keep Attention seed generation bounded on large graphs.

### 2.3 Optional persistent embedding retrieval

The Node reference runtime supports an opt-in persistent embedding cache in SQLite table `node_embeddings`.

For each non-archived, non-invalid searchable node, the embedding document is the same `searchableText(node)` used by the lexical index. A content hash of that text is stored with the vector, model name, dimension, and update time.

Embedding synchronization is incremental:

- unchanged `model + contentHash` rows are reused,
- changed/missing nodes are re-embedded in bounded batches,
- removed/archived nodes are removed from the active model cache,
- embedding model revision is tracked independently,
- a dimension change for the same configured model forces a clean model-cache rebuild instead of mixing incompatible vectors.

The baseline semantic search is **exact cosine scan**, not ANN:

```text
cos(q, d) =
       q · d
    -------------
    ||q|| ||d||
```

All stored vectors for the configured model with matching dimension are scored, sorted by cosine similarity, and limited. This is deliberately a correctness-first baseline. HNSW/IVF/native ANN indexing is **not implemented**.

The provider wire contract is OpenAI-compatible:

```text
POST <baseURL>/embeddings
{
  "model": "...",
  "input": ["...", "..."]
}
```

Embedding retrieval is disabled unless `retrieval.embeddings.enabled=true` and an explicit embedding model is configured.

### 2.4 Hybrid lexical + semantic fusion

Hybrid retrieval combines existing symbol/FTS ranks with embedding ranks using Reciprocal Rank Fusion:

```text
RRF(d) =
    sum over channels c of
        weight_c / (k + rank_c(d))
```

Current default:

```text
k = 60
lexicalWeight = 1
semanticWeight = 1
```

The hybrid candidate list is ranked by fused RRF score. Before entering Attention, fused scores are normalized by the top fused candidate into `[0,1]` and supplied as an external retrieval prior:

```text
retrievalPrior(d) =
    fusedScore(d) / maxFusedScore

seedRelevance(d) =
    max(
        lexicalScore(goal, d),
        retrievalPrior(d)
    )

seedScore(d) =
    seedRelevance(d)
    * attentionReliability(d)
    * kindBoost(d)
```

This is important: semantic retrieval does not bypass graph/evidence controls. Hybrid recall only supplies candidate IDs and bounded seed priors. The normal reliability weighting, graph propagation, token-cost ranking, MMR option, structural cuts, and context budget still apply.

Runtime APIs:

```js
await runtime.semanticSearch(query)
await runtime.hybridSearch(query)
await runtime.contextHybrid(goal)
await runtime.contextAsync(goal, { retrievalMode: 'hybrid' })
```

The synchronous `runtime.context()` API remains unchanged. Agent Loop uses the async path only when the next-turn cognitive retrieval policy requests `hybrid`.

## 3. Lexical similarity

Attention Light uses a token-set cosine-like lexical score:

```text
A = unique tokens from the goal
B = unique tokens from the node text

lexicalScore(A, B) =
    |A intersection B|
    ------------------
    sqrt(|A| * |B|)
```

Equivalently:

```text
lexicalScore = intersection / sqrt(leftTokenCount * rightTokenCount)
```

This is binary token-overlap cosine similarity: token frequency is not used after tokenization.

The searchable node text used by Attention includes:

- title,
- body,
- tags,
- source URI,
- serialized node metadata.

## 4. Seed scoring

Indexed candidates are converted into Attention seeds using:

```text
seedScore =
    clamp(
      lexicalScore(goal, node)
      * attentionReliability(node)
      * kindBoost(node),
      0,
      1
    )
```

Current node-kind boosts:

| Node kind | Boost |
|---|---:|
| `task` | 1.10 |
| `abstraction` | 1.05 |
| other kinds | 1.00 |

Nodes with status `archived` or `invalid` are excluded from normal lexical seed generation.

Explicit seed node IDs bypass lexical seed scoring and enter with activation score `1.0`.

After explicit and lexical seeds are deduplicated, the highest-scoring seeds are retained.

Current default:

```text
seedLimit = 8
```

## 5. Evidence/trust reliability

Truth/evidence quality influences attention but does not equal attention.

The first reliability layer is:

```text
truthReliability =
    gradeWeight
    * trustZoneWeight
    * lifecycle penalties
```

### 5.1 Evidence-grade weights

| Grade | Weight |
|---|---:|
| `hypothesis` | 0.45 |
| `static` | 0.65 |
| `tested` | 0.82 |
| `runtime` | 0.92 |
| `reproduced` | 1.00 |

### 5.2 Trust-zone weights

| Trust zone | Weight |
|---|---:|
| `system_verified` | 1.00 |
| `repo_trusted` | 0.95 |
| `runtime_verified` | 0.95 |
| `user_provided` | 0.85 |
| `external_untrusted` | 0.55 |
| `model_inferred` | 0.50 |

### 5.3 Lifecycle penalties

Current defaults:

| Condition | Multiplier |
|---|---:|
| stale | 0.45 |
| archived | 0.20 |
| dormant | 0.75 |
| TTL expired | 0.45 |
| invalid | truth reliability becomes 0.01 |

The resulting truth reliability is clamped to `[0.01, 1]`.

Attention intentionally softens this truth signal:

```text
attentionReliability =
    0.55 + 0.45 * truthReliability
```

This is an architectural invariant:

> Low confidence must reduce attention, but must not make a lexically or causally important node invisible.

Truth confidence and attention relevance are therefore separate dimensions.

## 6. Weighted graph propagation

The graph is converted into an adjacency map containing both outgoing and incoming traversals for every participating edge.

Edges marked with:

```text
metadata.attentionCut = true
```

do not participate in propagation.

### 6.1 Default propagation configuration

| Parameter | Default |
|---|---:|
| `budgetTokens` | 32,000 |
| `maxHops` | 4 |
| `minScore` | 0.08 |
| `seedLimit` | 8 |
| `decay` | 0.72 |
| `incomingPenalty` | 0.88 |
| `stalePenalty` | 0.45 |
| `archivedPenalty` | 0.20 |
| `dormantPenalty` | 0.75 |
| `costPenalty` | 0.08 |

### 6.2 Relation weights

| Edge type | Weight |
|---|---:|
| `causes` | 1.00 |
| `verifies` | 0.95 |
| `depends_on` | 0.90 |
| `calls` | 0.85 |
| `derived_from` | 0.85 |
| `canonicalizes` | 0.92 |
| `affects` | 0.80 |
| `abstracts` | 0.75 |
| `contradicts` | 0.70 |
| `supersedes` | 0.65 |
| `invalidates` | 0.65 |
| `relates_to` | 0.45 |

Unknown edge types fall back to `0.35`.

### 6.3 Propagation formula

For a traversal from the current node to a neighbor:

```text
directionWeight =
    1.0               for outgoing traversal
    incomingPenalty   for incoming traversal

relevance =
    0.5 + 0.5 * lexicalScore(goal, targetNode)

nextScore =
    currentScore
    * edgeWeight
    * directionWeight
    * decay
    * attentionReliability(targetNode)
    * relevance
```

The relevance floor of `0.5` is deliberate. A structurally important neighbor can still receive attention even when it has weak direct lexical overlap with the goal.

A propagated state is discarded when:

- `nextScore < minScore`, or
- the current path has already reached `maxHops`.

## 7. Traversal policy

The current traversal is priority / best-first in style:

```text
queue = stable max-heap(seed states)

while queue is not empty:
    current = pop highest score
    # equal scores preserve insertion order

    if this node was already reached with >= current score:
        skip

    if current score < minScore:
        skip

    record current as best activation for node

    if hop == maxHops:
        do not expand

    otherwise:
        score adjacent nodes
        append surviving propagated states
```

The runtime stores only the best score seen for a node. A later path replaces an earlier path only when it yields a higher activation.

This is not plain BFS because nodes are not expanded strictly by hop depth.

It is not Dijkstra/A* because the objective is not shortest-path distance.

It is not PageRank because activation is goal-conditioned, seeded per request, bounded by hops, and affected by evidence quality, lexical relevance, direction, and token budget.

## 8. Token-cost-aware context selection

Propagation can produce more relevant nodes than the model should receive.

For each reached node:

```text
utility =
    activation
    ----------------------------------------
    1 + costPenalty * log2(tokenCost + 1)
```

Current default:

```text
costPenalty = 0.08
```

Nodes are ranked by:

1. utility descending,
2. activation descending as tie-breaker.

Selection is greedy under the configured token budget:

```text
usedTokens = 0

for node in rankedNodes:
    if usedTokens + node.tokenCost <= budgetTokens:
        select node
        usedTokens += node.tokenCost
```

The returned Active Subgraph contains:

- selected nodes,
- participating edges whose endpoints are both selected,
- omitted node IDs,
- propagation trace,
- seed list,
- used and available token budget.

The current selector is a deterministic greedy heuristic; it is not a knapsack optimizer.

### 8.1 Optional associative Personalized PageRank

The default runtime path remains the weighted best-path Attention Light described above. A separate explicit `associative` mode is implemented for tasks where indirect graph association is more useful than a single strongest propagation path.

It starts from the same indexed/explicit seed set, then builds a bounded neighborhood using:

- `maxHops`,
- `associativeNodeLimit = 512` by default,
- structural cuts via `metadata.attentionCut`.

Inside that bounded neighborhood, LumenCortex runs a personalized restart diffusion:

```text
r_(t+1) = alpha * s + (1 - alpha) * P^T * r_t
```

Current defaults:

```text
alpha = pprRestart = 0.20
iterations <= 12
tolerance = 0.00001
minimum retained rank = 0.001
```

The restart distribution `s` is the normalized seed activation distribution. Transition weights are not uniform; each edge transition is weighted by the same structural/reliability semantics used by Attention:

```text
transitionWeight =
    edgeWeight(type)
    * directionWeight
    * attentionReliability(target)
    * (0.5 + 0.5 * lexicalScore(goal, target))
```

Outgoing transition weights are normalized per source node. Dangling probability mass is redistributed back to the personalized seed distribution.

After convergence or the iteration cap, nodes use the same token-cost utility rule as the normal Light:

```text
utility = pprRank / (1 + costPenalty * log2(tokenCost + 1))
```

Selection remains greedy under `budgetTokens`.

Runtime activation is explicit:

```js
runtime.context(goal, { retrievalMode: 'associative' })
```

Without that mode, `runtime.context()` remains weighted propagation. The cognitive Decision Layer exposes `associative` as a routing choice; Agent Loop applies a selected retrieval policy on the next reasoning turn rather than re-running retrieval inside the same turn.

### 8.2 Optional MMR diversity selection

Both weighted and associative Light share the same final token-budget selector. The default remains the existing greedy utility order:

```text
diversityLambda = 1.0
```

When a caller explicitly sets `diversityLambda < 1`, the first `diversityCandidateLimit` ranked candidates (default `256`) are selected iteratively with a Maximal Marginal Relevance-style score:

```text
relevance_i = utility_i / maxUtility

redundancy_i =
    max lexicalScore(node_i, selected_node_j)

MMR_i =
    lambda * relevance_i
    - (1 - lambda) * redundancy_i
```

The highest MMR candidate that still fits the remaining token budget is selected next. Ties fall back to the original utility/activation/id ordering. Candidates outside the bounded diversity pool fall back to normal greedy filling.

This uses the existing deterministic token-vector lexical similarity; it is not an embedding-based semantic diversity metric.

### 8.3 Deterministic retrieval profiles

The Runtime recognizes these retrieval modes:

| Mode | Execution | Deterministic profile |
|---|---|---|
| `weighted` | weighted Attention | existing default behavior |
| `lexical` | weighted Attention | same propagation defaults; label preserves Router intent |
| `dependency` | weighted Attention | `maxHops=5`; emphasizes `depends_on=1.0`, `calls=0.95`, `abstracts=0.85`, `derived_from=0.80`; reduces `relates_to=0.25` |
| `causal` | weighted Attention | `maxHops=5`; emphasizes `causes=1.0`, `affects=0.95`, `derived_from=0.95`, `depends_on=0.90`; reduces `relates_to=0.25` |
| `historical` | weighted Attention | `maxHops=5`; emphasizes `supersedes=1.0`, `invalidates=0.95`, `derived_from=0.90`, `contradicts=0.80`; permits archived seeds with bounded lifecycle penalties |
| `associative` | bounded PPR Light | personalized restart diffusion described above |

For historical mode:

```text
includeArchivedSeeds = true
archivedPenalty = 0.55
stalePenalty = 0.70
dormantPenalty = 0.85
```

Explicit caller options override profile defaults. Unknown retrieval modes fail soft to `weighted`.

## 9. Multi-Light policies

`illuminateMulti()` produces four related views.

### 9.1 Exploit

Exploit is the normal Attention Light result for the current goal.

It favors the strongest currently known indexed and graph-connected context.

### 9.2 Explore

Explore first finds up to three non-archived nodes outside the Exploit selection using:

```text
lexicalScore(goal, node) * attentionReliability(node)
```

Those nodes become explicit exploration seeds.

Explore then changes propagation behavior:

- `maxHops` increases by 1,
- `decay` increases by 0.08, capped at 0.86,
- `relates_to` weight becomes 0.75,
- `affects` weight becomes 0.90.

The purpose is to widen recall beyond the strongest local context.

### 9.3 Contrarian

Contrarian seeds are taken from `contradicts` and `invalidates` edges touching the Exploit subgraph.

Its relation weights emphasize:

```text
contradicts = 1.00
invalidates = 1.00
relates_to  = 0.55
```

The purpose is to surface evidence or beliefs that challenge the currently active explanation.

### 9.4 Anomaly

Anomaly seeds are up to five nodes where either:

- the node has tag `anomaly`, or
- status is `stale`.

Its relation weights emphasize:

```text
affects = 0.95
causes  = 1.00
```

The purpose is to keep unusual or invalidated context visible during reasoning.

### 9.5 Current fusion limitation

The four lights are separate Attention results. The system does not currently implement a learned fusion model or a global probabilistic reconciliation step across the four views.

## 10. Active Promotion

Promotion changes cognitive granularity without deleting source detail.

The current automatic controller evaluates:

```text
pressure = usedTokens / budgetTokens
unresolved = total unresolved items across selected nodes
eligible = selected non-task, non-abstraction nodes
reused = eligible nodes activated often enough
```

Current defaults:

| Parameter | Default |
|---|---:|
| pressure threshold | 0.72 |
| selected-node threshold | 18 |
| unresolved threshold | 6 |
| maximum children | 8 |
| minimum children | 3 |
| cooldown steps | 4 |
| reuse threshold per node | 3 |
| reused-node threshold | 4 |

Promotion can be triggered by one or more of:

- context pressure,
- node density,
- unresolved density,
- repeated activation.

A promotion is allowed only when cooldown is satisfied and at least three eligible child nodes exist.

Repeatedly activated nodes are preferred when at least four qualify; otherwise the most highly activated eligible nodes are used.

The promoted abstraction is deduplicated using a hash of:

- goal,
- sorted child IDs.

Promotion therefore creates a reusable parent view while leaving child nodes and detail intact.

## 11. Source-change invalidation

Repository ingestion tracks source content changes.

Changed-source and evidence-lifecycle invalidation now share one bounded transitive dependency propagation algorithm.

A reverse dependency map is built from only relations with explicit dependency semantics:

- node `evidenceIds`,
- node `childIds`,
- `derived_from` edges,
- `depends_on` edges,
- `abstracts` edges.

Starting from changed/stale evidence roots, breadth-first propagation follows dependency -> dependent links transitively. Only `belief`, `negative`, and `abstraction` nodes are automatically marked stale; entity/evidence nodes may be traversed as structural intermediates but are not rewritten by the propagator. Existing `archived`, `invalid`, and already-`stale` states are not overwritten.

Relations such as `relates_to`, `affects`, `causes`, `contradicts`, and `canonicalizes` deliberately do not trigger automatic stale propagation because they are not treated as truth-dependency declarations.

A newly dirtied node records provenance:

```text
status = stale
metadata.staleReason = <source-specific reason>
metadata.staleSourceIds = [root source ids]
metadata.staleDepth = minimum dependency distance
metadata.staleVia = [bounded propagation links]
metadata.stalePropagation = true
```

The same propagator is used by repository re-ingestion, TTL evidence audit, and evidence-content refresh.

## 12. Complexity characteristics

Let:

- `V` = graph nodes,
- `E` = graph edges,
- `C` = indexed candidate count,
- `R` = nodes reached during bounded propagation.

Current characteristics:

- persistent FTS5/symbol retrieval avoids the normal O(V) full-graph seed scan,
- building the in-memory adjacency map for a new graph revision is O(E),
- the Runtime caches the `AttentionEngine` and adjacency by repository graph revision, so repeated queries on an unchanged graph reuse the same adjacency,
- seed scoring is normally O(C),
- propagation is bounded by `maxHops`, `minScore`, and reachable graph structure,
- the propagation frontier is a stable binary max-heap: push/pop are O(log Q), where `Q` is frontier size, while equal-score states preserve insertion order,
- token selection is greedy after utility ranking,
- dependency invalidation builds a reverse dependency view in O(V + E) and then visits only reachable dependency paths from changed roots.

## 13. Why these algorithms are separate

LumenCortex deliberately separates three questions:

```text
Candidate retrieval:
    "Which nodes are worth considering at all?"

Graph attention:
    "Which structurally connected nodes matter for this goal?"

Context budgeting:
    "Which of those nodes are worth spending model tokens on?"
```

Accordingly:

- FTS5/symbol search provides recall and scale,
- weighted propagation provides structural relevance,
- trust/evidence modifies but does not dictate attention,
- token utility bounds model context.

No single score is treated as truth probability.

## 14. Implemented vs planned retrieval

The current production/reference behavior is:

```text
Exact symbol retrieval        IMPLEMENTED
SQLite FTS5 / BM25            IMPLEMENTED
Lexical token cosine          IMPLEMENTED
Weighted graph propagation    IMPLEMENTED
Multi-Light policies          IMPLEMENTED
Token-budget ranking          IMPLEMENTED
Optional MMR diversity        IMPLEMENTED

Embedding exact cosine        IMPLEMENTED / OPTIONAL
Hybrid RRF fusion             IMPLEMENTED / OPTIONAL
Approximate NN (HNSW/IVF)     NOT IMPLEMENTED
Personalized PageRank         IMPLEMENTED / OPTIONAL ASSOCIATIVE LIGHT
Global PageRank               NOT IMPLEMENTED
Dijkstra / A* retrieval       NOT IMPLEMENTED
Graph neural network ranking  NOT IMPLEMENTED
```

Embedding retrieval is an optional semantic candidate-recall channel, not a replacement for the Context Graph or Attention Light. The implemented baseline is persistent exact cosine; approximate nearest-neighbor indexing remains planned.

The PageRank-family path is intentionally separate from the default weighted propagation algorithm. It must not be used to retroactively describe the default Attention Light as PageRank.

## 15. Implementation map

The current reference implementation is primarily in:

| File | Responsibility |
|---|---|
| `src/attention.js` | seed scoring, reliability, weighted propagation, optional bounded associative PPR, multi-light policies, token-cost ranking |
| `src/constants.js` | edge, evidence-grade, and trust-zone weights |
| `src/search-index.js` | searchable text, symbol extraction, query tokenization |
| `src/embedding-index.js` | OpenAI-compatible embeddings, incremental vector cache, exact cosine, RRF fusion |
| `src/database.js` | FTS5/symbol persistence, BM25 lookup, incremental index synchronization |
| `src/runtime.js` | indexed candidate generation and Attention integration |
| `src/promotion-controller.js` | automatic Promotion trigger heuristic |
| `src/promotion.js` | non-destructive abstraction creation |
| `src/ingest.js` | repository dependency extraction and source-change stale invalidation |
| `src/graph.js` | graph mutation, adjacency-facing node/edge semantics, cut/restore/graft |

The current deterministic algorithm remains a first-class execution path. A future Light Controller may select retrieval modes, budgets, or propagation policy, but it must not replace the deterministic engine or directly mutate graph state. Deployments without a decision model remain valid. See [Cognitive control plane](cognitive-control-plane.md).

When any formula, default parameter, relation weight, or Light policy changes in code, this document should change in the same commit.
