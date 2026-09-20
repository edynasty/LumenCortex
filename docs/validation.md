# Validation status

This document separates architecture claims from evidence.

## Automated core tests

Latest validated core suite:

- 54 core tests / 54 passed / 0 failed
- Node.js 20 and 22 core matrix
- dedicated isolated Search / LSP / MCP / Subagent / TUI harness jobs
- MCP HTTP modern + legacy fallback + stdio transport coverage
- real overlapping Subagent concurrency assertion
- 100k-node persistent search benchmark

Covered behaviors include:

- cognitive graph integrity and reversible diffs,
- repository ingestion and source-change invalidation,
- Attention Light propagation and multi-light policies,
- manual Promotion and Active Promotion Controller,
- Cognitive Git commit/branch/merge/conflict/revert,
- Cognitive Git blame/cherry-pick/rebase,
- provider tool-call normalization,
- Agent Loop tool-call round trips,
- max-step protection,
- bounded Working-Set paging,
- a 20-tool-round / 21-reasoning-step long-loop runtime test,
- atomic cognitive worker commit/rollback,
- workspace path traversal protection.

## Long-task runtime test

The deterministic long-loop test intentionally separates runtime mechanics from model intelligence.

It performs:

```text
20 tool-call rounds
        +
21st LLM turn final answer
```

Assertions:

- Attention is recomputed on every reasoning turn.
- Full session history remains durable.
- Model working history contains only recent complete assistant/tool rounds.
- Tool results are not retained without the assistant tool-call message they answer.
- Working message count stays bounded while durable history grows.

This proves bounded-context loop mechanics. It does not prove that a weak model can solve a difficult coding task.

## Real local-model VM observations

Environment:

- GitHub-hosted Ubuntu VM
- local Ollama OpenAI-compatible endpoint
- Qwen tool-calling model

Observed with the earlier 1.7B ExpertTools model:

- endpoint health succeeded,
- ModelWeave provider compatibility succeeded,
- real `tool_calls` were received,
- `read_file` executed successfully,
- by step 3 the model received the correct hidden file content,
- the weak 1.7B model nevertheless repeated `read_file` and reached `max_steps`.

Interpretation:

The tool protocol and result feedback path worked. The observed failure was model policy/termination quality, not absence of the tool result.

A stronger Qwen3 4B VM validation is maintained in `.github/workflows/vm-real-agent-smoke.yml`.

A separate strict graph-memory proof in `.github/workflows/vm-long-memory-real-agent.yml` now passes on a GitHub-hosted Ubuntu VM with local Ollama and `qwen3:4b-instruct`:

- 7 real reasoning steps,
- step 1-4 each read exactly one distinct file,
- `recentRounds=1`, so the oldest tool result was no longer retained as ordinary recent chat history,
- the write step's Active Subgraph contained all 4 durable read Observation nodes,
- Active Promotion triggered after the fourth read,
- step 5 wrote the report from those values,
- step 6 ran an independent verifier,
- verifier printed `GRAPH_MEMORY_LONG_TASK_OK`,
- step 7 returned the final answer.

This is direct real-model evidence for the central design claim: paged-out tool evidence can remain durable in the Context Graph and be reactivated by the moving Attention Light when needed.

## DeepSeek validation

Integration support exists for:

- official DeepSeek OpenAI-compatible API through the `deepseek` provider; its current default model is `deepseek-flash`, which DeepSeek currently serves as DeepSeek-V4.1-Flash,
- OpenRouter preset `openrouter-deepseek-free`, pinned to `deepseek/deepseek-v4-flash-0731:free` for the free V4 Flash 0731 route.

A public no-key community DeepSeek V4 Flash endpoint was tested from an Ubuntu VM and re-tested on 2026-09-20. Four consecutive health requests returned:

```text
HTTP 400
Bad Request: The endpoint is paused, ask a maintainer to restart it
```

Therefore there is currently **no claim of a successful real DeepSeek inference run without credentials**.

A credentialed real-model validation must use one of:

- `DEEPSEEK_API_KEY` with the official API, or
- `OPENROUTER_API_KEY` with a currently free DeepSeek V4 route.

## Benchmark snapshot

The current synthetic benchmark in CI uses 1,205 graph nodes:

```text
fullEstimatedTokens   = 393879
activeEstimatedTokens = 1989
selectedNodes         = 5
contextRatio          = 0.005
```

This is a routing/attention benchmark, not a model-quality benchmark.

## Coding-harness validation

Dedicated `harness-ci` runs isolated tests for:

- persistent BM25/symbol retrieval and Attention Light seeding,
- Content-Length LSP JSON-RPC against a fake language server,
- MCP 2026 modern discovery and 2025 legacy fallback,
- dynamic MCP tool registration/calling,
- focused Subagents and parallel durable sessions,
- TUI frame/session/event rendering.

A separate 100k-node search benchmark is enforced by `search-benchmark`.

Latest successful synthetic run:

```text
nodes             = 100000
queries           = 50
index build       = 4468.01 ms
query p50         = 0.011 ms
query p95         = 0.039 ms
index size        = 84.27 MB
indexed terms     = 600015
indexed symbols   = 400000
```

The benchmark specifically stresses exact/symbol-heavy code lookup. It proves that this path no longer performs a full 100k-node scan per query; it does not substitute for semantic-retrieval quality evaluation.

Index consistency is separately tested: graph mutations increment an O(1) repository revision and Runtime automatically rebuilds a stale persistent index before searching.

## Still not validated / still planned

The following are not represented as complete features:

- embedding/vector semantic retrieval,
- cognitive branch <-> actual Git worktree transaction binding,
- transactional rollback of workspace edits,
- graph GC / hot-warm-cold memory tiers,
- full temporal validity querying,
- automatic split/merge/canonicalization,
- reusable Skills layer,
- vision/browser tools,
- formal independent Definition-of-Done evaluator.
