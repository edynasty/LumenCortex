# ModelWeave

ModelWeave is a **versioned cognitive graph + autonomous coding agent runtime**.

It replaces the assumption that an ever-growing chat history is the agent's memory. Project cognition is kept in a persistent graph, a bounded **Attention Light** selects the Active Subgraph for the current goal, and an independent **Agent Loop** reasons, calls tools, edits code, verifies results and continues until completion.

```text
Reality -> Evidence -> Belief -> Cognitive Graph
                                  |
                             Attention Light
                                  |
                            Active Subgraph
                                  |
                                  LLM
                                  |
                              tool_calls
                                  |
                        read / edit / shell / test
                                  |
                          refresh evidence graph
                                  |
                           next reasoning step
```

## v0.3 core features

### Cognitive runtime

- persistent Context Graph
- Evidence / Belief separation
- evidence grades instead of fake numeric confidence
- Cognitive Git: commit / branch / checkout / merge / conflict / revert / blame / cherry-pick / rebase
- Attention Light with propagation and token budgets
- exploit / explore / contrarian / anomaly lights
- manual + active Promotion without destructive compaction
- incremental repository ingestion
- source-change invalidation of dependent beliefs
- OpenCode integration

### Standalone agent

- complete multi-turn Agent Loop with per-step moving Attention Light
- OpenAI-compatible provider adapter
- OpenRouter / Groq / DeepSeek official presets
- generic local/API provider mode
- workspace tools: read, list, search, write, exact replace, shell
- ModelWeave context and ingest tools inside the loop
- permission gate for read / write / exec
- automatic graph refresh after workspace mutation
- persistent resumable full sessions + bounded model Working-Set Pager
- interactive chat CLI
- max-step guard
- per-session usage accounting
- real-model free-tier smoke test

## Install

```bash
npm install -g github:edynasty/ModelWeave
```

Inside a repository:

```bash
modelweave init
modelweave ingest .
modelweave commit "baseline"
```

## Free LLM quick start

### OpenRouter

```bash
export OPENROUTER_API_KEY=sk-or-...
modelweave agent "inspect this project, find the bug, fix it and run the relevant tests" \
  --provider openrouter \
  --model openrouter/free \
  --yes
```

### Groq

```bash
export GROQ_API_KEY=gsk_...
modelweave agent "inspect this project, find the bug, fix it and run the relevant tests" \
  --provider groq \
  --model openai/gpt-oss-120b \
  --yes
```

### DeepSeek official

```bash
export DEEPSEEK_API_KEY=...
modelweave agent "inspect this project, fix the failure and verify it" \
  --provider deepseek \
  --model deepseek-flash \
  --yes
```

### DeepSeek V4 Flash 0731 free through OpenRouter

```bash
export OPENROUTER_API_KEY=sk-or-...
modelweave agent "inspect this project, fix the failure and verify it" \
  --provider openrouter \
  --model deepseek/deepseek-v4-flash-0731:free \
  --yes
```

### Local vLLM / any OpenAI-compatible API

```bash
export MODELWEAVE_BASE_URL=http://127.0.0.1:8000/v1
export MODELWEAVE_MODEL=Qwen/Qwen2.5-32B-Instruct-AWQ
export MODELWEAVE_API_KEY=dummy
modelweave agent "run the tests and repair failures" --provider generic --yes
```

## Agent CLI

One-shot autonomous run:

```bash
modelweave agent "add pagination to the user API and test it" --yes
```

Interactive session:

```bash
modelweave chat --provider groq --model openai/gpt-oss-120b --yes
```

Resume:

```bash
modelweave sessions
modelweave agent "continue and fix the remaining failure" --session session_xxx --yes
```

Check providers:

```bash
modelweave providers
modelweave doctor --provider openrouter
modelweave doctor --provider openrouter --live
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

ModelWeave can still be used only as the cognitive layer under OpenCode:

```bash
modelweave install-opencode .
```

This installs `modelweave_context`, `modelweave_ingest` and `modelweave_state` tools.

## Core cognitive commands

```text
modelweave init [dir]
modelweave ingest [dir]
modelweave light <goal> [--budget N] [--multi]
modelweave promote <title> <nodeId> [nodeId...]
modelweave verify
modelweave status
modelweave commit <message>
modelweave log
modelweave branch [name]
modelweave checkout <branch>
modelweave merge <branch>
modelweave revert <commit>
modelweave blame <node-or-edge-id> [limit]
modelweave cherry-pick <commit>
modelweave rebase <branch>
```

## Development

```bash
npm test
npm run benchmark
npm run smoke:free   # requires a free provider API key
```

The core has zero runtime npm dependencies and requires Node.js 20+.

## Documentation

- `docs/architecture.md`
- `docs/data-model.md`
- `docs/opencode.md`
- `docs/agent-runtime.md`

## Implementation honesty

Implemented and covered by automated tests: moving Attention Light, bounded long-task working context, Active Promotion, source-change invalidation, Cognitive Git, standalone Agent Loop, provider abstraction, tools and resumable sessions.

Still planned rather than claimed as complete: hybrid BM25/symbol/embedding retrieval, persistent indexes, LSP, real Git-worktree transaction binding, graph GC/hot-warm-cold storage, MCP/Skills/subagent DAG, vision/browser tooling and automatic split/merge canonicalization.

## Current engineering direction

v0.3 intentionally keeps the orchestration/control plane in Node.js. At large graph sizes the next bottleneck is not JavaScript syntax; it is full-graph candidate scoring and rebuilding indexes on each query. The planned optimization path is persistent lexical/symbol indexes, hybrid retrieval and cached adjacency before considering a Rust data-plane implementation.

## License

MIT
