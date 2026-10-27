# Agent runtime — v0.6

LumenCortex v0.6 is both a cognitive runtime and a standalone coding-agent harness.

## Runtime loop

```text
Goal
  -> optional Workflow Contract (Facts / Action / Route / Gate)
  -> persistent SQLite FTS5/symbol candidate retrieval
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
- `read_files` — batch-read up to 32 already-known paths in one tool call to avoid one reasoning round per file
- `list_dir`
- `search_text`
- `code_search` — persistent SQLite FTS5 + symbol index
- `write_file`
- `replace_in_file`
- `apply_patch` — validates an exact multi-hunk / multi-file create-update-delete batch before mutating targets
- `shell` — asynchronous, timeout-bounded and cancellable; stdout/stderr stream as live Agent events; POSIX cancellation terminates the command process group
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

Permission policies have explicit scopes:

```text
read-only  -> read tools only
workspace  -> reads + workspace writes; no host shell; no external MCP writes
full       -> all configured read/write/exec tools
```

Unknown policy names are rejected. Ordinary autonomous `--yes` runs default to `full`; TUI without `--yes` remains `read-only`.

One-shot Agent and Chat commands bridge `SIGINT` / `SIGTERM` into the Agent `AbortSignal`, matching the TUI cancellation semantics: active provider/shell work is cancelled and the Session is stored as `interrupted` for resume.

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
  -> FTS5 lexical retrieval
  -> candidate node IDs
  -> graph propagation
  -> Attention Light
  -> finite Active Subgraph
```

Search and persistence share the same SQLite database. Exact code identifiers use the `symbols` table; natural-language/code-token recall uses FTS5. Graph mutations mark only changed node IDs in `search_dirty_nodes`; Runtime incrementally replaces those FTS5/symbol rows when the SQLite graph revision advances. A full rebuild is only needed for an empty/new index.

Current synthetic CI benchmark:

```text
100,000 graph nodes
index build       4.88 s
50 symbol queries
query p50         0.150 ms
query p95         0.280 ms
database size     79.60 MB
single mutation   207.87 ms
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

MCP servers are stdio processes configured in the global `mcp.json` (`<GlobalDir>/mcp.json`) and in
the project `<workspace>/.lumencortex/mcp.json`:

```json
{
  "servers": {
    "dbx": {
      "command": "dbx-mcp",
      "args": [],
      "protocolMode": "legacy"
    }
  }
}
```

`command` and `args` name the server process, `protocolMode` selects legacy `initialize` or modern
stateless discovery (default `legacy`), `disabled` keeps an entry without starting it, and the
optional `tools` map overrides tool permissions (below). A project entry with the same id as a
global entry overrides it while the workspace is trusted.

### Project configuration and trust

`<workspace>/.lumencortex/mcp.json` is a project input: it is used only while the workspace is
listed in the global `trustedWorkspaces` (ADR-0013). Trust is evaluated on every start and every
tool listing, against the Engine workspace and never against a session worktree.

- A server defined only by the project file of an untrusted workspace cannot start: `MCPStart`
  returns `ErrUntrustedWorkspace` and no process is created.
- While untrusted the project file is ignored entirely. An id it shadows resolves to its global
  entry, and that global entry is what the listing shows, so starting it is not a silent
  substitution. A project-only id is listed with `scope: "project"` and `blockedByTrust: true`.
- Withdrawing trust while a project server runs hides its tools from runs (`MCPTools` omits them)
  and refuses new calls, without killing the process.
- The global `mcp.json` is user-owned and is never gated.

### Tool permissions

Each MCP tool is exposed to runs with a permission:

- a tool the server annotates with `readOnlyHint: true` is `read`; anything else is `exec`;
- an entry in the server's `tools` map overrides that per tool, for example
  `{"tools": {"query": {"permission": "read"}}}`. The value must be `read`, `write` or `exec`;
  anything else is a configuration error that names the key.
- An override is honoured only from the global file, or from the project file of a trusted
  workspace, so a project file cannot claim read-only access its server does not declare.

Read-only runs (Ask) see `read` tools only, workspace runs also see `write`, and `exec` needs the
full policy. `MCPTools` reports the effective `permission` and its `permissionSource` (`annotation`
or `override`) for each tool.

```bash
lcx mcp status
lcx mcp tools dbx
lcx mcp call dbx query '{"sql":"select 1"}'
```

MCP tools are also dynamically added to normal agent runs unless `--no-mcp` is supplied.

## Subagents

Subagents are isolated durable Agent sessions with a focused system prompt and a restricted tool working set.

Two child prompts are built in and not editable (they are part of the harness, adapted from
oh-my-pi under MIT — see `THIRD_PARTY_NOTICES.md`):

- **Explore** (read-only child): the launch forbids writes, shell and spawning; the prompt asks for
  parallel searches within the per-step call limit, one alternate strategy before concluding
  something is absent, inferred thoroughness (quick/medium/thorough, default medium), and a compact
  markdown handoff: Summary, Files as `path:start-end`, Architecture, and a full Report only when the
  goal asks for one.
- **Worker** (execution child): one bounded objective in its own worktree, edit existing files,
  verify with evidence, record durable findings with `note_write`, obey an active Workflow Contract,
  and end with the minimum useful result for the parent. The workspace authority sentence is part of
  this prompt.

Specialists are ordinary catalog agents: `config.AgentPresets()` returns compiled-in definitions
(reviewer, security-reviewer, sonic, thread-historian) that an editor copies into
`lumencortex.json`; `config.ResolveAgentPresets` fills their model lists from the oh-my-pi
`modelRoles` once, as plain LC lists.

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
- live Agent events, including streaming shell stdout/stderr while commands are still running,
- current answer/result,
- session switching,
- new-session creation,
- parallel-task launch,
- `Ctrl+C` cancellation for the active Agent run; interrupted Session state remains resumable.

Commands:

```text
:new
:sessions
:use <session-id>
:parallel <tasks.json>
:help
:quit
Ctrl+C   # cancel the active run without discarding its persisted Session
```

Without `--yes`, TUI intentionally remains read-only because a full-screen readline loop and independent permission prompts must not compete for stdin.

## Sessions and resume

Sessions are stored in `lumencortex.db` in the workspace's state directory, outside the workspace by default ([ADR-0016](adr/0016-external-state-and-workspace-binding.md)) using normalized `sessions`, `session_messages`, and `agent_steps` tables. Session growth uses incremental UPSERTs rather than deleting/reinserting the complete history on every Agent step. The main Agent, TUI and Subagents inside one harness share one SessionStore connection; SQLite WAL supports independent readers/processes.

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

### Go loop exit contract

Every way the Go loop (`internal/agent`) ends a run, once the session row says `running`, writes one status and one cause in a single store update (`finish`). The cause is stored in `sessions.error_json` as `{"kind", "message", "at"}`; a `completed` run stores none, and the next run clears it. The write ignores the run's cancellation and has its own 5 s timeout. It happens after the step record and before the terminal event.

| Exit | Status | Kind |
|---|---|---|
| run context cancelled (also during a tool, or when a storage/provider error is the cancellation) | `interrupted` | `cancelled` |
| step budget exhausted | `interrupted` | `max_steps` |
| no tool calls and no content | `interrupted` | `empty_response` |
| reply cut at the output token limit (`finish_reason` `length`) without a tool call | `interrupted` | `output_limit` |
| provider or decision-provider error | `interrupted` | `provider:availability` |
| workflow human gate pending | `waiting_gate` | `workflow_gate` |
| storage error | `failed` | `storage` |
| workflow evaluation error | `failed` | `config` |
| panic in the run | `failed` | `panic` |
| completion accepted | `completed` | — |

If the terminal write itself fails, `Run` returns the cause joined with the write error, publishes no terminal event, and leaves the row `running`; the next run overwrites it. A panic's terminal event is `session.failed`, published after the write like the others, with the panic's stack.

A reply cut at the output limit is never accepted as the answer, in a conversation turn too: it stays in the transcript as the model's message, but the run does not complete with it. A provider stream counts as a response only when it ended: an OpenAI-compatible stream at a `finish_reason` or `[DONE]`, an Anthropic stream at `message_stop`. A stream that closes before that, or carries an error event, is an availability error that the adapter retries; the tool calls it carried so far are never executed. A reply cut at the output limit that holds tool calls continues the run: a call whose arguments are a whole JSON object was finished before the cut and runs; the last call, when its arguments are cut or empty, is answered with a tool error ("invalid tool arguments") and never runs.

Tool-call arguments must be a JSON object to run. Text that is not valid JSON, and text that is a JSON string, are stored as a JSON string holding the raw text; other JSON is stored as is. Arguments that are not an object are never executed: the model receives a tool error for that call, and the OpenAI-compatible adapter sends a stored JSON string back as its raw text and any other value as its JSON text, so the model sees what it sent. A tool stopped by cancellation leaves its call without a result row.

## Mutation persistence

Routine Cognitive Graph writes carry transient mutation hints from `CognitiveGraph` to the Repository. The SQLite layer directly UPSERTs or deletes only hinted node/edge rows and marks only affected node IDs dirty for FTS5/symbol synchronization. Graph snapshots use structural sharing: the node/edge index maps are copied, while immutable node/edge values are shared until replacement.

A 100k-node CI benchmark currently measures a single-node graph update at 129.11ms; the quality gate fails above 500ms.

## SQLite storage

The runtime uses one workspace-local database:

```text
.lumencortex/lumencortex.db
```

Important tables:

- `graph_nodes` / `graph_edges`
- `cognitive_commits` / `cognitive_refs`
- `sessions` / `session_messages` / `agent_steps`
- `journal`
- `search_documents` / `symbols` / `node_fts`

Cognitive Git stores diffs instead of duplicating the full graph in every commit. Sparse checkpoints are persisted roughly every 50 first-parent commits; history reconstruction starts from the nearest checkpoint and reconstructed snapshots are cached in memory. Pre-v0.6 JSON storage is imported once on open and archived after migration.

### Database maintenance

```bash
lcx db status
lcx db integrity
lcx db checkpoint
lcx db journal 100
```

`status` reports WAL mode, file/WAL sizes, graph/search revisions and table counts. `integrity` runs SQLite `PRAGMA integrity_check`. `checkpoint` performs a WAL checkpoint (TRUNCATE by default), and `journal` reads persisted runtime events.

Cross-process CI also starts two independent Node processes that write separate Agent Sessions concurrently while a third connection reads the database; both sessions must complete without lost updates or lock failures.

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


## Workflow Contract

Workflow state is orthogonal to the Cognitive Graph. The contract narrows the current Agent action, computes the visible tool set, observes deterministic tool outcomes into durable Facts, advances Routes, and blocks final completion until a terminal Action is proven. Human Gates persist the Session as `waiting_gate` instead of consuming more model turns.

See `docs/workflow-contract.md` and `examples/workflows/verified-code-fix.json`.


## Related documentation

- [System overview](system-overview.md)
- [Agent execution flow](execution-flow.md) — canonical step-by-step lifecycle
- [Workflow Contract](workflow-contract.md)
- [Security and trust](security-and-trust.md)
- [Validation evidence](validation.md)
