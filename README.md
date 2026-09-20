# LumenCortex

**LumenCortex** is a persistent cognitive coding-agent runtime.

> Graph is Memory. Light is Attention. Agent is Execution.

The short CLI is `lcx`. Running either `lumencortex` or `lcx` with no arguments opens the full-screen TUI.

Existing ModelWeave workspaces are migrated automatically from `.modelweave/` to `.lumencortex/`. The old `modelweave` command remains as a deprecated compatibility alias.

## v0.5 LumenCortex runtime

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
- persistent BM25 + symbol search index for large repositories
- indexed candidate generation before Attention Light (avoids full-graph seed scans)
- source-change invalidation of dependent beliefs
- OpenCode integration

### Standalone agent

- complete multi-turn Agent Loop with per-step moving Attention Light
- OpenAI-compatible provider adapter
- OpenRouter / Groq / DeepSeek official presets
- generic local/API provider mode
- workspace tools: read, list, indexed `code_search`, write, exact replace, shell
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
npm install -g github:edynasty/ModelWeave  # current repository URL; rename in GitHub Settings is still pending
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

The CI performance gate currently validates a synthetic 100k-node index at ~4.47s build time and 0.039ms query p95 for exact/symbol-heavy queries.

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

Resume:

```bash
lcx sessions
lcx agent "continue and fix the remaining failure" --session session_xxx --yes
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
--tools read_file,write_file,shell
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

## Real-model smoke test

The included smoke test creates a temporary repository, ingests it, asks the LLM to retrieve a hidden value by calling tools, loops over tool results, and checks the final answer.

```bash
OPENROUTER_API_KEY=... npm run smoke:free
```

or:

```bash
MODELWEAVE_PROVIDER=groq GROQ_API_KEY=... npm run smoke:free
```

## OpenCode mode

LumenCortex can still be used only as the cognitive layer under OpenCode:

```bash
lcx install-opencode .
```

This installs `lumencortex_context`, `lumencortex_ingest` and `lumencortex_state` tools.

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

The core has zero runtime npm dependencies and requires Node.js 20+.

## Documentation

- `docs/architecture.md`
- `docs/data-model.md`
- `docs/opencode.md`
- `docs/agent-runtime.md`

## Implementation honesty

Implemented and covered by automated tests: moving Attention Light, bounded long-task working context, Active Promotion, source-change invalidation, Cognitive Git, standalone Agent Loop, persistent BM25/symbol retrieval index, LSP protocol client/tools, MCP modern+legacy client, Subagents, parallel sessions, TUI, provider abstraction, tools and resumable sessions.

Still planned rather than claimed as complete: embedding retrieval, real Git-worktree transaction binding, graph GC/hot-warm-cold storage, reusable Skills layer, vision/browser tooling and automatic split/merge canonicalization.

## Current engineering direction

v0.4 intentionally keeps the orchestration/control plane in Node.js. At large graph sizes the next bottleneck is not JavaScript syntax; it is full-graph candidate scoring and rebuilding indexes on each query. Persistent lexical/symbol indexing is now implemented; the next data-plane optimizations are embeddings (optional), cached adjacency, incremental index segments and eventually a Rust core only if profiling justifies it.

## License

MIT


### DeepSeek provider distinction

- `--provider deepseek` defaults to `deepseek-flash`, the current official DeepSeek Flash API model.
- `--provider openrouter-deepseek-free` defaults to `deepseek/deepseek-v4-flash-0731:free`, the zero-token-price OpenRouter V4 Flash 0731 route.
- The OpenRouter free route still requires an `OPENROUTER_API_KEY` for authentication even though prompt/completion token price is zero.


## Rename compatibility

LumenCortex v0.5 keeps compatibility with the former ModelWeave name:

- `modelweave` remains a deprecated CLI alias.
- `MODELWEAVE_*` generic-provider/LSP environment variables remain accepted as fallbacks.
- legacy `.modelweave/` repositories migrate automatically to `.lumencortex/` on first open.
- legacy Agent tool names `modelweave_context` and `modelweave_ingest` remain executable for resumed old sessions but are hidden from new tool schemas.
