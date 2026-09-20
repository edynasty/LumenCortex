# Validation status

This document separates architecture claims from evidence.

## Automated core tests

Latest validated core suite:

- 23 tests
- 23 passed
- 0 failed
- Node.js 20 and 22
- benchmark executed in CI

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

A stronger Qwen3 4B VM validation is maintained in `.github/workflows/vm-real-agent-smoke.yml`, including an independent multi-file long-task fixture.

## DeepSeek validation

Integration support exists for:

- official DeepSeek OpenAI-compatible API through the `deepseek` provider,
- OpenRouter through the generic/OpenRouter provider path.

A public no-key community DeepSeek V4 Flash endpoint was tested from an Ubuntu VM. It returned:

```text
HTTP 400
Bad Request: The endpoint is paused, ask a maintainer to restart it
```

Therefore there is currently **no claim of a successful real DeepSeek inference run without credentials**.

A credentialed real-model validation must use one of:

- `DEEPSEEK_API_KEY` with the official API, or
- `OPENROUTER_API_KEY` with a currently free DeepSeek V4 route.

## Still not validated / still planned

The following are not represented as complete features:

- BM25/symbol/embedding Hybrid Retrieval,
- persistent retrieval indexes,
- LSP semantic navigation,
- cognitive branch <-> actual Git worktree transaction binding,
- transactional rollback of workspace edits,
- graph GC / hot-warm-cold memory tiers,
- full temporal validity querying,
- automatic split/merge/canonicalization,
- MCP/Skills/subagent DAG,
- vision/browser tools,
- formal independent Definition-of-Done evaluator.
