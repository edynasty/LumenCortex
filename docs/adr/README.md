# Architecture Decision Records

ADRs record durable architectural decisions and their trade-offs.

Use an ADR when a decision changes a major boundary or creates a compatibility constraint.

## Naming

```text
0001-short-decision-title.md
0002-next-decision.md
```

Use the next sequential number.

## Status

Use one of:

- Proposed
- Accepted
- Superseded
- Rejected

When superseding a decision, link both directions.

## Good ADR candidates

- transactional Git worktree isolation,
- rollback semantics,
- embedding retrieval architecture,
- Skills packaging/discovery,
- browser/vision trust boundaries,
- persistence format changes.

Ordinary implementation details and bug fixes do not need ADRs.

Use [0000-template.md](0000-template.md) as the starting point.


## Accepted decisions

- [0001 — Go runtime migration](0001-go-runtime-migration.md)
- [0002 — Bounded-memory runtime invariants](0002-bounded-memory-runtime.md)
- [0003 — Adaptive cognitive control plane](0003-adaptive-cognitive-control-plane.md)
- [0004 — M2.3 model runtime cutover decisions (D1–D6)](0004-m23-model-runtime-decisions.md)
- [0005 — Session worktree ownership, leases and deletion](0005-session-worktree-ownership-and-deletion.md)
- [0006 — Task context, resolution and conversational execution](0006-task-context-resolution-and-conversational-execution.md)
- [0007 — Cognitive layer status and growth path](0007-cognitive-layer-status.md)
- [0008 — Configurable agent concurrency](0008-configurable-agent-concurrency.md)
