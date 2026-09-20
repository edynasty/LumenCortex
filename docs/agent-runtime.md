# Agent runtime — v0.4

LumenCortex v0.4 is both a cognitive runtime and a standalone coding-agent harness.

## Runtime loop

```text
Goal
  -> persistent BM25/symbol candidate retrieval
  -> Attention Light + graph propagation
  -> Active Promotion when granularity is too dense
  -> Active Subgraph
  -> bounded Working-Set Pager
  -> LLM
  -> tool_calls
  -> permission gate / Tool Working Set
  -> read / search / LSP / MCP / subagent / edit / shell
  -> tool observations
  -> re-ingest + index refresh after workspace mutation
  -> next reasoning turn
  -> final / max_steps
```

Full session history is durable on disk. The model only receives the current Active Subgraph plus a bounded number of complete recent assistant/tool rounds.

## Built-in coding tools

Core workspace tools:

- `read_file`
- `list_dir`
- `search_text`
- `code_search` — persistent BM25 + symbol index
- `write_file`
- `replace_in_file`
- `shell`
- `lumencortex_context`
- `lumencortex_ingest`

LSP tools when a language server is configured:

- `lsp_definition`
- `lsp_references`
- `lsp_symbols`
- `lsp_hover`
- `lsp_diagnostics`

Subagent tools:

- `subagent_run`
- `subagent_parallel`

Configured MCP tools are dynamically registered as:

```text
mcp_<server>_<tool>
```

All tools participate in the same permission gate. File tools are workspace-scoped and reject path traversal.

## Large-repository retrieval

`lcx ingest` builds repository evidence and refreshes the persistent search index.

```bash
lcx ingest .
lcx index stats
lcx search "reserveInventory"
```

The retrieval path is:

```text
query
  -> exact symbol lookup
  -> BM25 selective postings
  -> candidate node IDs
  -> graph propagation
  -> Attention Light
  -> finite Active Subgraph
```

High-document-frequency postings are pruned when more selective symbol/lexical terms are available, avoiding a hidden O(N) query path.

Index freshness uses a small `.lumencortex/graph.revision` counter. Graph writes update the revision in O(1); queries rebuild only when the persisted index revision is stale.

Current synthetic CI benchmark:

```text
100,000 graph nodes
index build       4.47 s
50 symbol queries
query p50         0.011 ms
query p95         0.039 ms
index size        84.27 MB
```

This benchmark measures exact/symbol-heavy code navigation, not semantic-natural-language quality.

## LSP

The LSP client implements Content-Length JSON-RPC over stdio.

Default command mapping:

```text
.java                 -> jdtls
.ts/.tsx/.js/.jsx     -> typescript-language-server --stdio
.py                   -> pyright-langserver --stdio
```

Override or add servers in:

```text
.lumencortex/lsp.json
```

Example:

```json
{
  "servers": {
    "java": {
      "command": "/opt/jdtls/bin/jdtls",
      "args": [],
      "extensions": [".java"]
    }
  }
}
```

CLI examples:

```bash
lcx lsp status
lcx lsp symbols src/main/java/demo/OrderService.java
lcx lsp definition src/main/java/demo/OrderService.java 42 18
lcx lsp references src/main/java/demo/OrderService.java 42 18
lcx lsp diagnostics src/main/java/demo/OrderService.java
```

The same capabilities are exposed to the Agent Loop as tools.

## MCP

Configure MCP servers in:

```text
.lumencortex/mcp.json
```

Example stdio server:

```json
{
  "servers": {
    "dbx": {
      "transport": "stdio",
      "command": "dbx-mcp",
      "args": []
    }
  }
}
```

Example HTTP server:

```json
{
  "servers": {
    "remote": {
      "transport": "http",
      "url": "https://example.test/mcp",
      "headers": {
        "Authorization": "Bearer ..."
      }
    }
  }
}
```

LumenCortex supports modern stateless MCP discovery and legacy initialize fallback, plus both HTTP and stdio transports.

```bash
lcx mcp status
lcx mcp tools dbx
lcx mcp call dbx query '{"sql":"select 1"}'
```

MCP tools are also dynamically added to normal agent runs unless `--no-mcp` is supplied.

## Subagents

Subagents are isolated durable Agent sessions with a focused system prompt and a restricted tool working set.

```text
Main Agent
   |
   +-> subagent_run(goal)
   |
   +-> subagent_parallel([
         auth investigation,
         inventory investigation,
         DB investigation
       ])
```

Default subagents are read-oriented and do not:

- recursively spawn more subagents,
- auto-promote,
- auto-ingest,
- create persistent agent-task cognition,
- write tool-observation nodes.

They may still read the shared Context Graph and indexed repository state.

## Multi-session parallel execution

A task file:

```json
[
  {"goal":"inspect authentication flow","role":"security"},
  {"goal":"inspect inventory consistency","role":"domain"},
  {"goal":"inspect transaction boundaries","role":"data"}
]
```

Run:

```bash
lcx parallel tasks.json --concurrency 3
```

Each task receives a distinct durable Session ID. CI verifies that executions actually overlap rather than being serialized.

Parallel mutation is intentionally guarded. Multiple write-capable tasks require either `concurrency=1` or explicit `--unsafe-write-parallel`.

## TUI

Launch:

```bash
lcx tui --provider deepseek --yes
```

The full-screen terminal view exposes:

- current provider/model,
- current Session,
- recent Sessions,
- live Agent events,
- current answer/result,
- session switching,
- new-session creation,
- parallel-task launch.

Commands:

```text
:new
:sessions
:use <session-id>
:parallel <tasks.json>
:help
:quit
```

Without `--yes`, TUI intentionally remains read-only because a full-screen readline loop and independent permission prompts must not compete for stdin.

## Sessions and resume

Sessions are stored under:

```text
.lumencortex/sessions/<session-id>.json
```

A session persists:

- complete messages,
- tool rounds,
- context-history snapshots,
- provider/model,
- cumulative token/request usage,
- promotion history,
- current status/error/final output.

Resume:

```bash
lcx sessions
lcx agent "continue the task" --session session_xxx --yes
```

Global step numbers, context history and usage remain continuous across resume.

## Provider configuration

OpenRouter, Groq, DeepSeek official and arbitrary OpenAI-compatible APIs use the same runtime.

### DeepSeek official

```bash
export DEEPSEEK_API_KEY=...
lcx agent "run tests, fix failures and verify" \
  --provider deepseek \
  --model deepseek-flash \
  --yes
```

### Generic / local

```bash
export LUMENCORTEX_BASE_URL=http://127.0.0.1:8000/v1
export LUMENCORTEX_MODEL=Qwen/Qwen2.5-32B-Instruct-AWQ
export LUMENCORTEX_API_KEY=dummy

lcx agent "run the smoke tests" --provider generic --yes
```

For an unauthenticated local endpoint:

```bash
export LUMENCORTEX_REQUIRE_API_KEY=false
```

## Validation

The core suite, isolated coding-harness suite, 100k-node search benchmark and real-model long-memory workflows are maintained separately so failures can be attributed to the correct layer.
