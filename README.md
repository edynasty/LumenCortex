# LumenCortex

**LumenCortex** is a standalone persistent cognitive coding agent with a full-screen TUI and CLI.

> Graph is Memory. Light is Attention. Agent is Execution.

The primary interface is the full-screen TUI. Run `lumencortex` or the short command `lcx` with no arguments to open it directly. Shell/test stdout and stderr are streamed into the active Agent view while commands are still running. While an agent run is active, `Ctrl+C` cancels the provider/tool execution, persists the Session as `interrupted`, and leaves it resumable with `--session`.

## v0.6 LumenCortex runtime

### Cognitive runtime

- persistent Context Graph
- Evidence / Belief separation
- evidence grades instead of fake numeric confidence
- Cognitive Git: commit / branch / checkout / merge / conflict / revert / blame / cherry-pick / rebase
- non-destructive structural cut/restore + explicit graph graft
- Attention Light with propagation and token budgets
- per-run Tool Working Set allowlist and per-step tool-call fanout limits
- provider retry / timeout / empty-turn recovery for long-running agents
- exploit / explore / contrarian / anomaly lights
- manual + active Promotion without destructive compaction
- incremental repository ingestion
- SQLite WAL persistence for graph, sessions, Cognitive Git, journal, symbols and FTS5 retrieval
- persistent FTS5 + symbol search index for large repositories
- indexed candidate generation before Attention Light (avoids full-graph seed scans)
- source-change invalidation of dependent beliefs

### Workflow Contract

- durable Facts → Action → Route → Outcome → Gate state inside normal Sessions
- per-Action tool visibility plus execution-time enforcement
- deterministic tool-result Outcomes that write Facts with provenance
- final-answer gate: model claims cannot bypass required evidence
- human gates pause as `waiting_gate` and resume after explicit approval
- independent from the Cognitive Graph: Workflow controls legal progress; Attention Light controls remembered evidence

### Standalone agent

- complete multi-turn Agent Loop with per-step moving Attention Light
- OpenAI-compatible provider adapter
- OpenRouter / Groq / DeepSeek official presets
- generic local/API provider mode
- workspace tools: single/batch read (`read_file` / `read_files`), list, indexed `code_search`, write, exact replace, validated multi-file `apply_patch`, asynchronous cancellable shell
- LSP tools: definition, references, symbols, hover, diagnostics
- MCP client: 2026 modern + legacy negotiation, stdio + HTTP transports
- focused Subagent + parallel Subagent tools
- multi-session parallel runner and full-screen TUI
- LumenCortex context and ingest tools inside the loop
- permission gate for read / write / exec
- automatic graph refresh after workspace mutation
- persistent resumable full sessions + bounded model Working-Set Pager
- interactive chat CLI
- max-step guard
- per-session usage accounting
- real-model free-tier smoke test

## Install

```bash
npm install -g github:edynasty/LumenCortex
```

Inside a repository:

```bash
lcx init
lcx ingest .
lcx commit "baseline"
```

## Free LLM quick start

### OpenRouter

```bash
export OPENROUTER_API_KEY=sk-or-...
lcx agent "inspect this project, find the bug, fix it and run the relevant tests" \
  --provider openrouter \
  --model openrouter/free \
  --yes
```

### Groq

```bash
export GROQ_API_KEY=gsk_...
lcx agent "inspect this project, find the bug, fix it and run the relevant tests" \
  --provider groq \
  --model openai/gpt-oss-120b \
  --yes
```

### DeepSeek official

```bash
export DEEPSEEK_API_KEY=...
lcx agent "inspect this project, fix the failure and verify it" \
  --provider deepseek \
  --model deepseek-flash \
  --yes
```

### DeepSeek V4 Flash 0731 free through OpenRouter

```bash
export OPENROUTER_API_KEY=sk-or-...
lcx agent "inspect this project, fix the failure and verify it" \
  --provider openrouter-deepseek-free \
  --model deepseek/deepseek-v4-flash-0731:free \
  --yes
```

### Local vLLM / any OpenAI-compatible API

```bash
export LUMENCORTEX_BASE_URL=http://127.0.0.1:8000/v1
export LUMENCORTEX_MODEL=Qwen/Qwen2.5-32B-Instruct-AWQ
export LUMENCORTEX_API_KEY=dummy
lcx agent "run the tests and repair failures" --provider generic --yes
```

## Agent CLI

One-shot autonomous run:

```bash
lcx agent "add pagination to the user API and test it" --yes
```

Interactive session:

```bash
lumencortex                         # opens TUI directly
lcx                                 # short form, also opens TUI
lcx chat --provider groq --model openai/gpt-oss-120b --yes
lcx tui --provider groq --model openai/gpt-oss-120b --yes
```

Large-repository code intelligence:

```bash
lcx index build
lcx search "reserveInventory"
lcx lsp references src/main/java/.../InventoryService.java 42 18
```

The CI performance gate currently validates a synthetic 100k-node SQLite/FTS5 index. Recent runs show roughly 3.4–5.0s full index build, sub-millisecond exact/symbol query p95 (latest 0.29ms), and a separate single-node mutation gate below 500ms (recent successful runs 129–213ms).

MCP:

```bash
# configure .lumencortex/mcp.json
lcx mcp status
lcx mcp tools my-server
```

Parallel sessions:

```bash
lcx parallel tasks.json --concurrency 4
```

Workflow-constrained coding:

```bash
lcx workflow validate examples/workflows/verified-code-fix.json
lcx agent "fix the failing tests" --workflow examples/workflows/verified-code-fix.json --yes

# Human gate, when present
lcx workflow status session_xxx
lcx workflow approve session_xxx release-approval
lcx agent --session session_xxx --yes
```

Resume:

```bash
lcx sessions
lcx agent "continue and fix the remaining failure" --session session_xxx --yes
```

Database health:

```bash
lcx db status
lcx db integrity
lcx db checkpoint
lcx db journal 50
```

Check providers:

```bash
lcx providers
lcx doctor --provider openrouter
lcx doctor --provider openrouter --live
```

Useful controls:

```text
--provider openrouter|groq|deepseek|generic
--model MODEL
--base-url URL
--max-steps 24
--max-tokens N
--budget 24000
--recent-rounds 6
--working-chars 120000
--timeout-ms 120000
--tools read_file,code_search,apply_patch,shell
--workflow examples/workflows/verified-code-fix.json
--max-tool-calls-per-step 1
--no-auto-promote
--policy read-only|workspace|full
--yes
--session ID
--no-ingest
--cognitive-commit
--json
```

Without `--yes`, write and shell actions require interactive approval. Non-interactive runs deny those actions unless explicitly approved.

Permission policies are scope-aware:

- `read-only` — read tools only.
- `workspace` — workspace reads and writes, but no host shell execution and no external MCP write tools.
- `full` — workspace access plus host shell execution and external write-capable tools.

Unknown policy names fail closed instead of silently becoming full access. For normal autonomous coding with `--yes`, the default remains `full`.

For one-shot `lcx agent` and interactive `lcx chat`, `SIGINT` / `SIGTERM` are forwarded into the active Agent run. Provider/tool work is cancelled and the durable Session is persisted as `interrupted` so it can be resumed.

For non-trivial edits, `apply_patch` can validate multiple exact hunks across multiple files before mutating any target. It supports update/create/delete operations, rejects ambiguous hunks and workspace traversal, and is treated as a mutating tool by the parallel-session safety gate.

## Persistence

Each workspace keeps one local database:

```text
.lumencortex/
├── lumencortex.db
├── lumencortex.db-wal
├── lumencortex.db-shm
├── lsp.json        # optional
└── mcp.json        # optional
```

SQLite runs in WAL mode. Repository, Runtime/SearchIndex, and SessionStore connections are explicitly closed when the TUI/agent harness exits. The database stores the Cognitive Graph, Cognitive Git commits/refs, durable Sessions, Agent steps/messages, runtime journal, code symbols and FTS5 search data. Graph snapshots use structural sharing plus transient mutation hints, so the normal single-node mutation path avoids full-table scans and full deep copies. Cognitive commits store graph diffs rather than a full graph snapshot per commit. Sparse SQLite checkpoints are written roughly every 50 first-parent commits, so long histories do not need to replay from genesis after every restart; reconstructed snapshots are also cached in memory.

Existing pre-v0.6 JSON-based `.lumencortex` repositories are imported automatically once and moved into a timestamped `json-backup-*` directory after successful migration.

## Real-model smoke test

The included smoke test creates a temporary repository, ingests it, asks the LLM to retrieve a hidden value by calling tools, loops over tool results, and checks the final answer.

```bash
OPENROUTER_API_KEY=... npm run smoke:free
```

or:

```bash
LUMENCORTEX_PROVIDER=groq GROQ_API_KEY=... npm run smoke:free
```


## Core cognitive commands

```text
lcx init [dir]
lcx ingest [dir]
lcx light <goal> [--budget N] [--multi]
lcx promote <title> <nodeId> [nodeId...]
lcx verify
lcx status
lcx commit <message>
lcx log
lcx branch [name]
lcx checkout <branch>
lcx merge <branch>
lcx revert <commit>
lcx blame <node-or-edge-id> [limit]
lcx cherry-pick <commit>
lcx rebase <branch>
lcx edge cut <edgeId> [reason]
lcx edge restore <edgeId>
lcx edge graft <from> <type> <to> [weight] [reason]
```

## Development

```bash
npm test
npm run benchmark
npm run benchmark:search
npm run smoke:free   # requires a free provider API key
```

The core has zero runtime npm dependencies and requires Node.js 22.13+ because persistence uses the built-in `node:sqlite` module.

## Documentation

Start with **[`docs/README.md`](docs/README.md)**.

Core documents:

- [System overview](docs/system-overview.md)
- [Architecture diagrams](docs/architecture-diagrams.md)
- [Agent execution flow](docs/execution-flow.md)
- [Architecture details](docs/architecture.md)
- [Agent runtime](docs/agent-runtime.md)
- [Workflow Contract](docs/workflow-contract.md)
- [Data model](docs/data-model.md)
- [Security and trust](docs/security-and-trust.md)
- [Validation evidence](docs/validation.md)
- [Standalone readiness](docs/standalone-readiness.md)
- [Documentation guide](docs/documentation-guide.md)
- [Architecture Decision Records](docs/adr/README.md)

## Implementation honesty

Implemented and covered by automated tests: moving Attention Light, bounded long-task working context, Active Promotion, source-change invalidation, Cognitive Git, standalone Agent Loop, deterministic Workflow Contracts, SQLite FTS5/symbol retrieval, incremental graph/index persistence, optimistic graph revisions, LSP protocol client/tools, MCP modern+legacy client, Subagents, parallel sessions, shared durable SessionStore, TUI, provider abstraction, tools and resumable sessions.

Still planned rather than claimed as complete: embedding retrieval, real Git-worktree transaction binding, graph GC/hot-warm-cold storage, reusable Skills layer, vision/browser tooling and automatic split/merge canonicalization.

## Current engineering direction

v0.6 intentionally keeps the orchestration/control plane in Node.js. Full-graph seed scans and per-query index rebuilds have been removed from the normal retrieval path: SQLite FTS5/symbol lookup generates candidates, graph writes use optimistic revisions, and dirty-node queues incrementally synchronize search rows. The next data-plane optimizations are embeddings (optional), cached adjacency and graph hot/warm/cold tiers; a Rust core is only justified if profiling later shows a native data-plane bottleneck.

## License

MIT


### DeepSeek provider distinction

- `--provider deepseek` defaults to `deepseek-flash`, the current official DeepSeek Flash API model.
- `--provider openrouter-deepseek-free` defaults to `deepseek/deepseek-v4-flash-0731:free`, the zero-token-price OpenRouter V4 Flash 0731 route.
- The OpenRouter free route still requires an `OPENROUTER_API_KEY` for authentication even though prompt/completion token price is zero.

