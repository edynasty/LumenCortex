# ModelWeave

**ModelWeave is a versioned cognitive-context runtime for long-running coding agents.**

Instead of treating an ever-growing conversation as the agent's memory, ModelWeave treats a persistent graph as the cognitive state and gives each LLM invocation only a bounded, task-specific **Active Subgraph**.

The design is built around three ideas:

1. **Graph is memory** — code evidence, beliefs, abstractions, tasks and relations live in a persistent graph.
2. **Light is attention** — a bounded attention light selects the small part of the graph that should enter the current model context.
3. **History is Git-like** — cognition changes are commits with branches, merge conflicts, revert and blame-friendly provenance.

> Current status: `v0.1.0` is a working core runtime and OpenCode integration, intended for architecture validation and real-repository experiments. It is not presented as a finished replacement for mature IDE/agent products.

## Why

Typical coding-agent loops eventually degrade into:

```text
conversation
  + tool output
  + repeated file reads
  + sub-agent summaries
  + compaction
  + more summaries
```

ModelWeave changes the unit of persistence:

```text
Reality -> Evidence -> Belief -> Context Graph
                              |
                         Attention Light
                              |
                        Active Subgraph
                              |
                             LLM
                              |
                         Graph Diff
                              |
                    Verify -> Commit
```

The LLM is an executor over cognitive state; it is not the cognitive state itself.

## Features

- Persistent `Context Graph`
- `Evidence` / `Belief` separation
- Evidence grades instead of fake numeric confidence
- Git-like cognitive commits, branches, merge conflicts and revert
- Bounded Attention Light with activation propagation
- Separate exploit / explore / contrarian / anomaly lights
- Context `Promotion`: create an abstraction while preserving all child detail
- Incremental repository ingestion
- Stable file/chunk IDs across re-ingestion
- Static JS/TS and Java import dependency hints
- Source-change invalidation of dependent beliefs
- TTL-based evidence staleness
- Atomic worker execution with rollback on invalid cognition
- OpenCode custom tools
- Zero runtime dependencies

## Requirements

- Node.js 20+
- Git is optional but recommended; when available, ingestion records the repository HEAD as `sourceVersion`.

## Install from GitHub

```bash
npm install -g github:edynasty/ModelWeave
```

During local development:

```bash
npm link
```

## Quick start

Inside a code repository:

```bash
modelweave init
modelweave ingest .
modelweave commit "ingest repository baseline"
```

Ask the attention engine what should be considered for a task:

```bash
modelweave light "why can duplicate acceptance cause inventory inconsistency" --budget 32000
```

Run all four attention policies:

```bash
modelweave light "inventory acceptance race condition" --multi --json
```

Inspect state:

```bash
modelweave status
modelweave log
modelweave show
```

Create a cognitive branch for a hypothesis:

```bash
modelweave branch hypothesis/inventory-race
modelweave checkout hypothesis/inventory-race
```

Add a belief backed by evidence:

```bash
modelweave node add belief "Acceptance can race" \
  "Two requests can enter the acceptance path concurrently" \
  --grade static \
  --evidence chunk_xxxxx

modelweave commit "record inventory race hypothesis"
```

Merge it back:

```bash
modelweave checkout main
modelweave merge hypothesis/inventory-race
```

If two branches mutate the same cognitive object differently, ModelWeave reports a merge conflict instead of silently summarizing both claims into one.

## Context Promotion

Promotion preserves detail instead of destructive compaction:

```bash
modelweave promote "Inventory consistency" node_a node_b node_c
modelweave commit "promote inventory context"
```

The new abstraction has `abstracts` edges to the original nodes. The original context remains available for drill-down.

## OpenCode integration

Install ModelWeave custom tools into the current project:

```bash
modelweave install-opencode .
```

This installs:

- `modelweave_context` — returns a bounded active subgraph for a concrete coding goal.
- `modelweave_ingest` — incrementally refreshes the repository graph.
- `modelweave_state` — status / verify / cognitive commit.

Recommended agent policy:

```text
1. For a non-trivial task, call modelweave_context before broad exploration.
2. Use normal OpenCode read/LSP/bash/vision tools to execute the task.
3. Re-ingest after meaningful source changes.
4. Commit only cognition worth preserving.
5. If current evidence is insufficient, widen the light before spawning more agents.
```

This keeps ModelWeave as the **cognitive layer** and OpenCode as the **execution harness**.

## CLI

```text
modelweave init [dir]
modelweave install-opencode [dir]
modelweave ingest [dir] [--chunk-lines 160] [--max-bytes 524288]
modelweave status
modelweave commit <message>
modelweave log [limit]
modelweave branch [name]
modelweave checkout <branch>
modelweave merge <branch>
modelweave revert <commit>
modelweave node add <kind> <title> [body] ...
modelweave node update <id> ...
modelweave node rm <id>
modelweave edge add <from> <type> <to> [weight]
modelweave edge rm <id>
modelweave show [id]
modelweave light <goal> [--budget 32000] [--multi] [--json]
modelweave promote <title> <nodeId> [nodeId...]
modelweave verify
```

## Core model

### Node kinds

- `entity` — code/module/domain entity
- `evidence` — observable source material
- `belief` — derived cognition that must cite evidence once promoted above hypothesis
- `negative` — scoped negative finding ("this path has been ruled out")
- `abstraction` — higher-level index/summary that points to detail
- `task` — transient or persistent task state

### Evidence grades

```text
hypothesis < static < tested < runtime < reproduced
```

The runtime deliberately separates **truth strength** from **attention strength**. A low-confidence hypothesis may still deserve a bright light when it is relevant to the current problem.

### Graph mutation principles

ModelWeave maps the cognitive operations discussed during design into a small set of primitives:

```text
ACTIVATE   -> Attention Light
PROPAGATE  -> graph traversal with decay
PRUNE      -> lower activation / archive, not destructive deletion
GRAFT      -> add relation edges
SPLIT      -> create finer nodes
MERGE      -> canonicalize or Git-like branch merge
PROMOTE    -> create abstraction without destroying detail
REVERT     -> restore prior cognitive state
```

## Development

```bash
npm test
npm run demo
npm run benchmark
```

The project intentionally has **zero runtime dependencies** so the graph/runtime can be embedded into other agent harnesses without pulling in another framework.

## Architecture docs

- [`docs/architecture.md`](docs/architecture.md)
- [`docs/data-model.md`](docs/data-model.md)
- [`docs/opencode.md`](docs/opencode.md)

## License

MIT
