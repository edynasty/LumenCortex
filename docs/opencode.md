# OpenCode integration

ModelWeave is designed to complement OpenCode rather than replace its execution tools.

OpenCode custom tools live in `.opencode/tools/`. ModelWeave ships three tools and an installer:

```bash
npm install -g github:edynasty/ModelWeave
modelweave init
modelweave ingest .
modelweave commit "baseline"
modelweave install-opencode .
```

After restarting/reloading OpenCode, the model can call:

- `modelweave_context`
- `modelweave_ingest`
- `modelweave_state`

## Suggested AGENTS.md policy

```text
Use ModelWeave as the project cognition layer.

For non-trivial work:
1. Call modelweave_context with the concrete problem before broad repository exploration.
2. Prefer the returned active subgraph and normal deterministic tools (read/LSP/grep/db/test/vision).
3. Widen context with multi-light only when evidence is insufficient or contradictory.
4. Do not treat a model hypothesis as evidence.
5. After meaningful code changes, call modelweave_ingest.
6. Persist only useful project cognition with modelweave_state commit.
7. Use subagents only when a task is independently verifiable and parallelism is beneficial.
```

This intentionally puts deterministic tools before autonomous sub-agent expansion.

## Why this is different from ordinary RAG

A RAG query normally returns top-k chunks. ModelWeave also uses:

- persistent entity/evidence/belief relationships,
- graph propagation,
- source version and staleness,
- cognitive branches and conflicts,
- abstractions that preserve drill-down detail,
- explicit attention budgets.

The active context is therefore a versioned subgraph, not just a similarity search result.
