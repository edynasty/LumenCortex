# Architecture

## 1. Design objective

ModelWeave is not another autonomous coding agent. It is a **cognitive runtime** that sits underneath an agent harness such as OpenCode.

The problem it targets is long-horizon context degradation:

- repeated repository exploration,
- destructive compaction,
- stale summaries,
- sub-agent aggregation overhead,
- inability to explain why a fact entered the current context,
- model-generated beliefs silently becoming "truth".

## 2. Seven-layer model

```text
┌──────────────────────────────────────┐
│ 7. Goal / Intent                     │
├──────────────────────────────────────┤
│ 6. Attention / Light                 │
├──────────────────────────────────────┤
│ 5. Belief Graph                      │
├──────────────────────────────────────┤
│ 4. Evidence Graph                    │
├──────────────────────────────────────┤
│ 3. Reality Snapshot                  │
│    code / DB / runtime / docs        │
├──────────────────────────────────────┤
│ 2. Version / History                 │
│    commit / branch / merge / revert  │
├──────────────────────────────────────┤
│ 1. Execution                         │
│    LLM / tools / actions             │
└──────────────────────────────────────┘
```

The invariant is:

```text
Reality -> Evidence -> Belief
```

A belief never becomes evidence merely because an LLM stated it confidently.

## 3. Attention Light

The complete graph can grow much larger than a model context window. A Light selects a bounded Active Subgraph.

Each node receives an activation score from:

- lexical task relevance,
- graph-edge propagation,
- relation type,
- evidence/trust quality,
- status and staleness,
- propagation distance,
- token cost.

Attention and truth are separate dimensions. Truth quality influences attention but cannot reduce a relevant hypothesis to zero visibility.

### Multi-light mode

The runtime exposes four policies:

- **Exploit** — follow the strongest current path.
- **Explore** — widen through weak/related edges and alternative seeds.
- **Contrarian** — prioritize contradicting/invalidating relations.
- **Anomaly** — prioritize stale or anomaly-tagged evidence.

This is designed to reduce self-reinforcing attention loops.

## 4. Promotion instead of destructive compaction

When a local area becomes too detailed, ModelWeave can create an `abstraction` node:

```text
Abstraction A
  ├─ abstracts -> Detail 1
  ├─ abstracts -> Detail 2
  └─ abstracts -> Detail 3
```

The abstraction is an index and working summary. Child nodes remain intact and can be re-activated later.

Increasing future model context sizes only increases how many descendants can be illuminated at once; the persistence model remains valid.

## 5. Git-like cognition

`.modelweave/` stores:

```text
.modelweave/
├── HEAD
├── graph.json
├── journal.jsonl
├── config.json
├── refs/heads/*
└── commits/*.json
```

Every commit contains:

- parent commit(s),
- graph diff,
- full snapshot for reliable v0.1 checkout,
- graph hash,
- metadata.

Branches represent competing hypotheses or task paths. Three-way merge works at node/edge object granularity. Conflicting edits are surfaced explicitly.

A future storage backend can replace snapshot-heavy commits with packed deltas without changing the public model.

## 6. Repository ingestion

Ingestion converts a code repository into a reality/evidence graph:

```text
Directory abstraction
  -> File entity
       -> Evidence chunks
```

Stable IDs are derived from repository-relative paths and line ranges. Re-ingestion therefore updates existing nodes rather than creating new random copies.

Current static dependency hints:

- JS/TS relative imports,
- Java imports resolvable inside the scanned source tree.

When chunk content changes, beliefs/abstractions that cite the changed evidence are marked `stale` instead of silently trusted.

## 7. Atomic worker runtime

`ModelWeaveRuntime.execute()` gives a worker an Active Subgraph and accepts structured operations.

The runtime:

1. creates a task node,
2. illuminates context,
3. calls the worker,
4. applies proposed graph operations in memory,
5. validates evidence integrity,
6. writes state,
7. commits cognition,
8. rolls back completely when validation fails.

This prevents a model hallucination from partially corrupting persistent cognition.

## 8. Safety / trust zones

Nodes record a trust zone:

```text
system_verified
repo_trusted
runtime_verified
user_provided
external_untrusted
model_inferred
```

Evidence is forbidden from using `model_inferred`. This is the first protection against persistent cognitive poisoning.

External content should enter as `external_untrusted` evidence and must not be silently promoted to a verified belief.

## 9. What v0.1 deliberately does not pretend to solve

- optimal active-subgraph routing,
- semantic embeddings,
- full static call/data-flow analysis,
- distributed multi-agent scheduling,
- automatic canonical graph GC,
- persistent latent model state.

These are benchmark targets, not assumptions hidden behind marketing language.
