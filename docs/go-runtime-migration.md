# Go runtime migration

LumenCortex is migrating the runtime from Node.js to Go without replacing the repository or discarding the validated Node implementation.

## Current milestone

The first Go vertical slice provides:

- public `runtime.Engine`,
- durable Session metadata/messages backed by the existing SQLite file/schema,
- paged recent-message access instead of loading complete history,
- bounded event subscriptions,
- bounded head/tail shell output with live streaming events,
- explicit working-memory budget accounting,
- a preview `cmd/lcx-go` binary for runtime smoke testing.

It does **not** yet replace the production Node Agent Loop, Workflow runtime, Cognitive Graph runtime, LSP/MCP, or TUI. Those remain the behavioral reference until Go parity tests close each area.

## Preview

```bash
go run ./cmd/lcx-go health
go run ./cmd/lcx-go session-new "inspect this repository"
go run ./cmd/lcx-go sessions
```

The preview writes to the same `.lumencortex/lumencortex.db` Session tables used by the Node implementation.

## Migration gate

A Node subsystem is removed from the default path only when:

1. Go behavior has parity fixtures/tests,
2. cancellation and error persistence are covered,
3. memory behavior is bounded for unbounded inputs,
4. the current CLI/session contract remains compatible,
5. validation/readiness documentation is updated.


## Agent/provider milestone

The Go runtime now also contains the first bounded Agent Loop, Workflow integration, built-in workspace tools, and an OpenAI-compatible streaming provider. See [Go OpenAI-compatible provider](go-provider.md).

The Node implementation remains the behavioral reference for Cognitive Graph retrieval, LSP/MCP, subagents, TUI behavior, and the remaining provider/compatibility cases until those parity gates are closed.
