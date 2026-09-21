# ADR 0002: Bounded-memory runtime invariants

- Status: Accepted
- Date: 2026-09-21

## Context

Coding-agent tasks can run for hours and produce large transcripts, tool output, terminal output, repository indexes, and parallel subagent state. A runtime that keeps these structures proportional to task duration will eventually exhaust memory regardless of implementation language.

## Decision

The Go runtime follows four invariants:

1. **Session size != RAM size.** Complete history is durable; active history is paged and bounded.
2. **Tool output size != RAM size.** Tool output is streamed rather than accumulated in memory; the agent keeps only bounded head/tail windows plus metadata. A durable artifact sink is required before this path replaces the Node runtime.
3. **Graph size != RAM size.** Persistent graph/search data stays disk-backed; only retrieval candidates and the active cognitive subgraph are loaded.
4. **Task duration != linear RSS growth.** Long-task state growth belongs primarily on disk, not in heap-resident slices/maps.

Any collection whose size can grow with repository size or task duration must use a bounded cache, cursor/pagination, stream, iterator, ring/head-tail buffer, or durable store.

UI event subscribers are bounded. Slow clients may lose transient stream events; authoritative state must remain recoverable from durable storage.

## Validation

The first Go slice includes tests proving:

- multi-megabyte shell output retains only bounded head/tail windows,
- event subscribers cannot grow without bound,
- a 1000-message Session can read only the recent requested page,
- explicit resource reservations fail before the configured hard budget.

Future CI will add RSS/heap benchmarks for long synthetic sessions and parallel agents.
