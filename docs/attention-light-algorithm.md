# Attention Light algorithm specification

This document is the canonical algorithm reference for LumenCortex cognitive retrieval and Attention Light.

It describes the behavior implemented by the current Node.js reference runtime on `main`. It does not promote planned retrieval techniques to implemented status.

Related documents:

- [Architecture](architecture.md) — subsystem boundaries and cognitive design.
- [Execution flow](execution-flow.md) — where retrieval and Attention Light run during an Agent step.
- [Agent runtime](agent-runtime.md) — runtime integration and persistent search-index behavior.
- [Data model](data-model.md) — graph node, edge, evidence-grade, and trust-zone structures.
- [Cognitive control plane](cognitive-control-plane.md) — planned adaptive policy above the deterministic Light engine.

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
| Embedding/vector retrieval | **Planned / optional** |
| Personalized PageRank | **Not implemented** |
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
queue = seed states

while queue is not empty:
    sort queue by score descending
    current = remove highest score

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

When changed evidence is detected, current ingestion marks directly dependent nodes stale when a `belief`, `negative`, or `abstraction` node references changed evidence through:

- `evidenceIds`, or
- `childIds`.

The stale node receives:

```text
status = stale
metadata.staleReason = ingested-source-changed
```

This stale state then reduces future attention through the reliability penalty.

Current limitation: ingestion invalidation is a direct reference check, not a general transitive graph invalidation algorithm.

## 12. Complexity characteristics

Let:

- `V` = graph nodes,
- `E` = graph edges,
- `C` = indexed candidate count,
- `R` = nodes reached during bounded propagation.

Current characteristics:

- persistent FTS5/symbol retrieval avoids the normal O(V) full-graph seed scan,
- building the in-memory adjacency map for a new `AttentionEngine` is O(E),
- seed scoring is normally O(C),
- propagation is bounded by `maxHops`, `minScore`, and reachable graph structure,
- the current queue is re-sorted each iteration rather than using a binary heap, so queue management is not asymptotically optimal,
- token selection is greedy after utility ranking.

Cached adjacency is still planned and is expected to remove repeated adjacency construction from the hot path.

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

Embedding retrieval           PLANNED / OPTIONAL
Personalized PageRank         NOT IMPLEMENTED
Global PageRank               NOT IMPLEMENTED
Dijkstra / A* retrieval       NOT IMPLEMENTED
Graph neural network ranking  NOT IMPLEMENTED
```

Embedding retrieval is intended as an additional semantic candidate-recall channel, not as a replacement for the Context Graph or Attention Light.

If a PageRank-family algorithm is introduced later, it should be documented as a separate architectural change rather than retroactively describing the current algorithm as PageRank.

## 15. Implementation map

The current reference implementation is primarily in:

| File | Responsibility |
|---|---|
| `src/attention.js` | seed scoring, reliability, weighted propagation, multi-light policies, token-cost ranking |
| `src/constants.js` | edge, evidence-grade, and trust-zone weights |
| `src/search-index.js` | searchable text, symbol extraction, query tokenization |
| `src/database.js` | FTS5/symbol persistence, BM25 lookup, incremental index synchronization |
| `src/runtime.js` | indexed candidate generation and Attention integration |
| `src/promotion-controller.js` | automatic Promotion trigger heuristic |
| `src/promotion.js` | non-destructive abstraction creation |
| `src/ingest.js` | repository dependency extraction and source-change stale invalidation |
| `src/graph.js` | graph mutation, adjacency-facing node/edge semantics, cut/restore/graft |

The current deterministic algorithm remains a first-class execution path. A future Light Controller may select retrieval modes, budgets, or propagation policy, but it must not replace the deterministic engine or directly mutate graph state. Deployments without a decision model remain valid. See [Cognitive control plane](cognitive-control-plane.md).

When any formula, default parameter, relation weight, or Light policy changes in code, this document should change in the same commit.
