# ADR 0001: Migrate the LumenCortex runtime to Go incrementally

- Status: Accepted
- Date: 2026-09-21

## Context

The current Node.js runtime proved the LumenCortex architecture: persistent Sessions, Workflow Contract, Cognitive Graph, indexed retrieval, LSP, MCP, subagents, TUI, and real-model coding loops. The next product goal is a long-running coding harness with predictable memory use and a desktop client that can embed the same runtime.

A full rewrite would discard a working reference implementation and make behavioral regressions difficult to detect.

## Decision

LumenCortex will migrate its runtime to Go inside the existing repository.

The Node.js implementation remains the reference implementation during migration. Go capabilities are introduced as independently testable vertical slices and become default only after parity evidence exists.

The public Go package is `github.com/edynasty/LumenCortex/runtime`. CLI, TUI, and Desktop should depend on that runtime boundary instead of importing internal agent modules.

Initial migration order:

1. runtime kernel, event stream, bounded resource accounting,
2. SQLite-backed Session access compatible with the current database,
3. bounded streaming shell/tools and provider streaming,
4. Workflow Contract and Agent Loop,
5. Cognitive retrieval/attention/graph access,
6. LSP, MCP, subagents, and TUI parity.

## Consequences

- Existing Node tests remain required until their Go replacement has equivalent coverage.
- Go code must open the existing `.lumencortex/lumencortex.db` rather than introduce a second session database.
- The Desktop repository can embed the public runtime package.
- Rust/native modules remain an optimization option only for benchmark-proven data-plane bottlenecks.
