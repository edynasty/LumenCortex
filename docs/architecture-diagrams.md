# Architecture diagrams

This file is the visual architecture index.

Curated static visuals are stored under `docs/assets/architecture/` and are the primary presentation layer. Reusable Mermaid topology sources remain under `docs/diagrams/` for maintainable architecture logic.

## Legend

- navy: entry/user surface,
- indigo: Workflow and policy control,
- cyan: cognitive retrieval/attention,
- green: execution,
- orange: host/external integration,
- purple: durable persistence,
- dashed gray: planned/incomplete.

## System architecture

Question answered:

> What are the major runtime layers, and how do they interact?

<p align="center">
  <img src="assets/architecture/system-architecture.webp" alt="LumenCortex system architecture" width="100%">
</p>

_Maintainable topology source: [`diagrams/system-architecture.mmd`](diagrams/system-architecture.mmd)._

Source: `docs/diagrams/system-architecture.mmd`.

## Cognitive control plane

Question answered:

> How do Decision Layer judgments, framework routing, Category model chains, dynamic Think effort, Agent execution, and Graph Governor fit together?

The maintainable English diagram source is:

- [`diagrams/cognitive-control-plane.en.mmd`](diagrams/cognitive-control-plane.en.mmd)

It reflects the implemented Node.js baseline:

```text
Decision Layer
    -> Cognitive Kernel / Framework Router
        -> Category Model Chains
        -> Think / Agent Execution
            -> Context Graph / Cognitive Git

Graph Governor
    <-> durable cognitive state
```

Jev/Laya are advisory Decision Layer providers. Category chains contain generative execution/reasoning models only.

## Agent execution logic

Question answered:

> What exactly happens from a user goal until completion, pause, or resume?

<p align="center">
  <img src="assets/architecture/agent-execution-flow.webp" alt="LumenCortex agent execution flow" width="100%">
</p>

_Maintainable topology source: [`diagrams/agent-execution-flow.mmd`](diagrams/agent-execution-flow.mmd)._

Source: `docs/diagrams/agent-execution-flow.mmd`.

See [Execution flow](execution-flow.md) for step-by-step semantics.

## Workflow and Cognitive Graph dual control plane

Question answered:

> Why are Workflow Contract and Context Graph separate instead of being one graph?

<p align="center">
  <img src="assets/architecture/workflow-cognitive-dual-plane.webp" alt="Workflow and Cognitive Graph dual control plane" width="100%">
</p>

_Maintainable topology source: [`diagrams/workflow-cognitive-dual-plane.mmd`](diagrams/workflow-cognitive-dual-plane.mmd)._

Source: `docs/diagrams/workflow-cognitive-dual-plane.mmd`.

The invariant is:

```text
Workflow: what is legal / proven
Cognition: what is remembered / attended
Execution: what actually happens
```

## Persistence and data flow

Question answered:

> What survives process restart and where is it stored?

<p align="center">
  <img src="assets/architecture/persistence-data-flow.webp" alt="LumenCortex persistence and data flow" width="100%">
</p>

_Maintainable topology source: [`diagrams/persistence-data-flow.mmd`](diagrams/persistence-data-flow.mmd)._

Source: `docs/diagrams/persistence-data-flow.mmd`.

## Long-task invariant

```text
Durable Session + Context Graph may grow
                 │
                 ▼
      retrieval narrows candidates
                 │
                 ▼
       Attention Light moves
                 │
                 ▼
        finite Active Subgraph
                 │
                 ▼
       bounded recent tool rounds
                 │
                 ▼
             finite LLM input
```

LumenCortex does not require the entire durable history to be sent to the model every turn.

## Implementation vs target

The diagrams intentionally show planned components as dashed gray.

Currently important planned/incomplete items include:

- transactional real Git worktree isolation,
- vector/embedding retrieval,
- whole-run rollback for arbitrary workspace mutations,
- richer Skills/vision/browser layers.

For claim status, use [Standalone readiness](standalone-readiness.md), not the diagram alone.
