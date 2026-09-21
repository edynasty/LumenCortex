# LumenCortex Documentation

This directory is the authoritative technical documentation set for LumenCortex.

The README at the repository root is intentionally a quick-start and product summary. Detailed architecture, runtime semantics, evidence, safety boundaries, and maintenance rules live here.

<p align="center">
  <img src="assets/architecture/system-architecture.webp" alt="LumenCortex system architecture" width="100%">
</p>

[Open the full visual architecture index →](architecture-diagrams.md)

## Recommended reading order

### I want to understand the system

1. [System overview](system-overview.md)
2. [Architecture diagrams](architecture-diagrams.md)
3. [Execution flow](execution-flow.md)
4. [Architecture details](architecture.md)
5. [Data model](data-model.md)

### I want to understand the autonomous coding runtime

1. [Agent runtime](agent-runtime.md)
2. [Workflow Contract](workflow-contract.md)
3. [Security and trust](security-and-trust.md)
4. [Validation evidence](validation.md)
5. [Standalone readiness](standalone-readiness.md)

### I am changing the project

1. [Documentation guide](documentation-guide.md)
2. [ADR guide and template](adr/README.md)
3. [Validation evidence](validation.md)


### I want to follow the Go runtime migration

1. [Go runtime migration](go-runtime-migration.md)
2. [ADR 0001 — Go runtime migration](adr/0001-go-runtime-migration.md)
3. [ADR 0002 — Bounded-memory runtime](adr/0002-bounded-memory-runtime.md)

## Document authority

| Topic | Canonical document | Purpose |
|---|---|---|
| Product summary / quick start | `README.md` | Short user-facing entry point |
| System boundaries and component map | `docs/system-overview.md` | First architecture document to read |
| Architecture invariants and cognitive design | `docs/architecture.md` | Deep architecture specification |
| Diagrams | `docs/architecture-diagrams.md` | Visual architecture source |
| Agent lifecycle and failure behavior | `docs/execution-flow.md` | End-to-end runtime logic |
| Agent tools/providers/sessions | `docs/agent-runtime.md` | Runtime implementation reference |
| Workflow schema and semantics | `docs/workflow-contract.md` | Workflow Contract reference |
| Persistent structures | `docs/data-model.md` | Data and persistence reference |
| Permission/trust/safety boundaries | `docs/security-and-trust.md` | Security model and known gaps |
| Verified claims | `docs/validation.md` | Evidence, benchmarks, real-model runs |
| Product readiness / gaps | `docs/standalone-readiness.md` | What can and cannot currently be claimed |
| Documentation conventions | `docs/documentation-guide.md` | How future docs should be written |
| Architecture decisions | `docs/adr/` | Why important decisions were made |

## Status vocabulary

Use only these labels for implementation state:

- **Implemented** — code exists.
- **Validated** — the real execution path is covered by automated or real-model evidence.
- **Partial** — useful implementation exists but a meaningful part is missing.
- **Planned** — design target only.
- **Evidence gap** — implementation exists, but the required proof does not yet exist.

Do not use “complete”, “production ready”, “safe”, or “OpenCode parity” merely because a module exists.

## Core design model

LumenCortex has three deliberately separate state domains:

```text
Workflow State
Facts / Action / Route / Outcome / Gate
        │
        │ constrains legal progress
        ▼
Execution State
Agent / LLM / Tools / Shell / LSP / MCP / Sessions
        ▲
        │ supplies evidence and context
        │
Cognitive State
Evidence / Belief / Attention Light / Context Graph
```

The separation is intentional:

- Workflow decides what is legal and what counts as proven completion.
- Cognitive memory decides what should be remembered and attended to.
- Execution performs the actual coding work.

## Current major engineering gaps

The authoritative list is [Standalone readiness](standalone-readiness.md). The highest-value remaining gaps are currently:

1. transactional real Git worktree isolation for an Agent run,
2. rollback of arbitrary workspace mutations produced by shell commands,
3. semantic/vector retrieval as an optional recall layer,
4. reusable Skills,
5. richer TUI navigation and interaction,
6. vision/browser tools,
7. credentialed DeepSeek real-model validation.

When one of these changes, update the readiness and validation documents in the same change.
