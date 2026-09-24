# System overview

## What LumenCortex is

LumenCortex is a standalone persistent cognitive coding agent.

It is not only an LLM wrapper, not only a workflow engine, and not only a graph-memory plugin. The runtime combines:

- a durable cognitive graph,
- bounded attention and working context,
- a multi-turn coding Agent loop,
- deterministic Workflow Contracts,
- workspace tools, shell, LSP, MCP, and Subagents,
- persistent resumable Sessions,
- SQLite WAL storage,
- evidence-driven validation.

The central design goal is to allow long coding tasks to retain useful knowledge without replaying the entire history into every model request.

## System architecture

<p align="center">
  <img src="assets/architecture/system-architecture.webp" alt="LumenCortex system architecture" width="100%">
</p>

_Maintainable topology source: [`diagrams/system-architecture.mmd`](diagrams/system-architecture.mmd)._

Source: `docs/diagrams/system-architecture.mmd`.

### Diagram legend

- dark blue: entry surface,
- indigo: workflow / control-plane constraints,
- cyan: cognitive retrieval and attention,
- green: active execution,
- orange: external/runtime integration,
- purple: durable persistence,
- dashed gray: planned or incomplete.

## Three state domains

### 1. Workflow state

Workflow state answers:

> What stage is the task in, what may happen next, and what evidence is required before progress is accepted?

It contains:

- Facts,
- current Action,
- Routes,
- Outcomes,
- condition or human Gates,
- per-Action allowed tools.

Workflow state is deterministic runtime state. It is not model opinion.

### 2. Cognitive state

Cognitive state answers:

> What has the system observed, inferred, learned, contradicted, promoted, or needs to attend to now?

It contains:

- Evidence,
- Beliefs / hypotheses,
- negative knowledge,
- entities and abstractions,
- tool observations,
- Context Graph relations,
- Attention Light state,
- Cognitive Git history.

The persistent graph may grow while each individual model working set remains finite.

### 3. Execution state

Execution state performs work:

- Agent Loop,
- provider requests,
- tool calls,
- workspace reads and edits,
- shell/test processes,
- LSP,
- MCP,
- Subagents,
- Session persistence,
- cancellation and resume.

Workflow constrains execution. Cognitive state supplies relevant context to execution.

## Component responsibilities

| Component | Responsibility | Durable? |
|---|---|---:|
| TUI / CLI | user entry, events, session control | no |
| Agent Loop | reasoning/tool iteration and lifecycle | Session-backed |
| Workflow Contract | legal progress and completion evidence | yes, in Session metadata |
| Permission policy | scope-level tool authorization | configuration/runtime |
| Working-Set Pager | bound recent chat/tool rounds | no |
| Search index | exact/symbol candidate generation | yes |
| Attention Light | select task-relevant graph nodes | recomputed |
| Context Graph | durable cognitive memory | yes |
| Active Promotion | create abstractions without deleting detail | yes |
| Cognitive Git | version cognitive mutations | yes |
| Tool Registry | workspace/LSP/MCP/shell operations | no |
| Session Store | messages, steps, status, usage, workflow snapshot | yes |
| SQLite | canonical workspace persistence | yes |

## The core invariant

```text
Reality
   ↓
Evidence
   ↓
Persistent Context Graph
   ↓
Indexed retrieval
   ↓
Attention Light
   ↓
Finite Active Subgraph
   ↓
Bounded model working set
   ↓
LLM → tools → new observations
   ↓
Reality / Evidence updated
```

The LLM does not own durable truth. It performs temporary computation over selected evidence.

The target control architecture adds a deterministic Cognitive Kernel above the existing runtime, with separate Fast, Think, configured cognitive-model bindings, and Graph-Governor roles. This is a planned control plane; the current implementation remains the deterministic Attention Light + Agent Loop described elsewhere. See [Cognitive control plane](cognitive-control-plane.md).

## Workflow and cognition are complementary

A common architectural mistake would be to merge Workflow state into the cognitive graph or make the cognitive graph decide task legality.

LumenCortex keeps them separate:

- a Workflow Fact such as `tests.passed=true` is deterministic progress state,
- a cognitive Evidence node may preserve the actual test output and provenance,
- a Workflow Action can prohibit edit tools,
- the cognitive graph can still remember an edit strategy,
- the graph cannot override the Workflow tool boundary,
- the Workflow does not decide which code evidence is relevant enough to attend to.

See [Workflow and Cognitive Graph dual control plane](architecture-diagrams.md#workflow-and-cognitive-graph-dual-control-plane).

## Persistence boundary

The canonical workspace state is:

```text
.lumencortex/
├── lumencortex.db
├── lumencortex.db-wal
├── lumencortex.db-shm
├── lsp.json          optional
└── mcp.json          optional
```

SQLite stores:

- graph nodes/edges,
- Cognitive Git commits/refs,
- Sessions/messages/steps,
- Workflow snapshots through Session metadata,
- runtime journal,
- symbols and FTS5 documents.

## Current boundaries

Implemented architecture should not be confused with future target architecture.

Not yet represented as complete:

- transactional real Git worktree isolation,
- whole-run rollback after arbitrary shell mutations,
- embeddings/vector retrieval,
- reusable Skills,
- vision/browser tooling,
- automatic graph GC/hot-warm-cold tiers,
- full editor-grade TUI UX,
- adaptive cognitive control plane (framework routing, Fast/Think modes, configurable model bindings),
- Graph Governor for global pruning/branching/promotion/canonicalization and Cortex epochs.

Use [Standalone readiness](standalone-readiness.md) for the current evidence gate.
