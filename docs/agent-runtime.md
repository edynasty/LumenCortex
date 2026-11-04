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
  -> read / search / LSP / MCP / subagent / edit / bash
  -> tool observations
  -> re-ingest + index refresh after workspace mutation
  -> next reasoning turn
  -> final / max_steps
```

Full session history is durable on disk. The model only receives the current Active Subgraph plus a bounded number of complete recent assistant/tool rounds.

Each Start has no implicit step cap (`config.DefaultMaxSteps=-1`, shared by CLI and Loop).
`MaxSteps=0` means unspecified; `-1` explicitly removes the cap; a positive value is the host's limit.
A response/tool cycle consumes one step, including completion corrections; HTTP retries stay inside
that step. Explicit exhaustion is resumable `interrupted/max_steps`, never success. Tool batches are
uncapped unless `MaxToolCallsPerStep` is positive. Permissions and resource concurrency remain enforced.
Immediate children inherit execution controls; `ChildExecutionOptions` lets the host override their
step/batch caps and tighten their Deadline without extending the parent's lifetime or authority.
There is no default global Deadline. Caller cancellation, host Cancel and explicit Deadline reach
preparation, model requests, retry waits, tools and owned children. A run settles its owned children
and background jobs before the supervisor publishes `run.stopped`; host-started independent runs and
jobs are not adopted by a later Start of their historical parent session.
See [provider retry policy](go-provider.md#retrying-availability-failures).

### Explicit task delivery

Hosts can set `AgentOptions.delivery` (CLI: `LCX_DELIVERY=/absolute/contract.json`):

```json
{"files":[{"path":"src/result.go","changed":true}],"branch":"fix/result","commit":true}
```

These requirements are host-selected, not inferred from model text. Task State captures the initial
file blobs and commit HEAD once, renders the requirements in every request, and preserves them across
resume and context reset. Re-supplying a different contract in the same session is rejected. Paths are
literal workspace-relative regular files; symlinks outside the workspace do not satisfy delivery.
`changed` compares against the pre-execution file content; omit it to require only existence. A commit
requirement needs a descendant of the original HEAD with a nonempty delivered tree delta and no pending
changes to the listed files (the workspace when no files are listed). The runtime never commits for the
model. Without `commit:true`, dirty files are allowed. Deletions and semantic acceptance belong in
approved Checks/Workflow rather than existence requirements.

Autonomous final answers still pass Workflow, Work Units and approved required Checks, then delivery.
An unmet explicit condition refuses completion and gives the model its missing requirements. The
runtime does not infer progress, repetition or loops from tool-call patterns and does not terminate
delivery after a fixed number of corrections. It also never accepts a final answer merely because
a live-job correction allowance ran out. Hosts bound autonomous work with explicit step/request
budgets, Deadline and cancellation; provider availability recovery remains finite independently.
Conversation turns without a host workflow may end normally with delivery
open; turn completion is not objective completion. An unmet delivery condition prevents evidence-based
resolution. A notebook or summary is not delivery proof. No contract means no new file/commit gate;
natural-language completion claims alone cannot prove arbitrary behavior.


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
- `bash` — runs each command in bash (`/bin/bash -lc`, else bash on PATH; POSIX `sh` when bash is not installed; on Windows Git Bash, else `cmd.exe /C`; the tool description names the program actually used); asynchronous, timeout-bounded (default 120 s, at most `execution.shellMaxTimeoutSeconds`, default 1800 s, or the run's deadline) and cancellable; stdout/stderr stream as live Agent events; POSIX cancellation terminates the command process group. `mode: "background"` submits the command to the durable job queue instead and returns its job id at once; `timeout_sec` bounds a background job's run time. Before ADR-0019 it was named `shell`: host lists naming `shell` (allowlists, denylists, workflow `allowedTools` and tool conditions) mean `bash`. `git_status`/`git_diff` are no longer agent tools; the agent runs `git` through `bash`, and the host Git API is unchanged
- `lumencortex_context`
- `lumencortex_ingest`

Background job tools, registered with `bash` and under the same `exec` permission (ADR-0019 §3):

- `job_status` — state, exit code, duration and the bounded head and tail of a job's output (readable while it runs)
- `job_wait` — waits for one of the named jobs, or for any active job of the run, bounded by `timeout_sec` and by the time left in the run; it never cancels the job
- `job_cancel` — a queued job never starts; a running one has its process group killed
- `job_list` — this session's recent jobs

A job's row in `jobs` is its source of truth: every transition is one compare-and-set `UPDATE`, so a
restart finds each job where its last committed transition left it. Output is written head-and-tail,
bounded, under `<state>/jobs/<job_id>/`; the database holds byte counts only. The completion gate refuses
a final answer while the run's own jobs are queued or running (a correction naming them, at most twice per
run), and every run end cancels the run's remaining jobs (`run_ended`); host and schedule jobs are never
cancelled by a run. A run whose session has queued, running or `outcome_unknown` jobs starts with one
runtime row (origin `jobs`, ≤ 600 bytes) naming them. The manager that opens a database adopts the queued
jobs of a dead process and records its running jobs `outcome_unknown` rather than running them again.

LSP tools when a language server can serve the workspace (see LSP below):

- `lsp_hover`
- `lsp_definition`
- `lsp_references`
- `lsp_document_symbols`
- `lsp_workspace_symbols`
- `lsp_diagnostics`
- `lsp_rename` — write; applies the workspace edit through the file guard and the atomic batch

Subagent tools:

- `subagent_run`
- `subagent_parallel`

Configured MCP tools are dynamically registered as:

```text
mcp__<server>__<tool>
```

All tools participate in the same permission gate. File tools are workspace-scoped and reject path traversal.

Permission policies have explicit scopes:

```text
read-only  -> read tools, and the state tools (the run's own working notebook)
workspace  -> reads + workspace writes; no host shell; no external MCP writes
full       -> all configured read/write/exec tools
```

A `state` tool writes only the run's own durable working memory - never a workspace file, a command
or another session - so every policy admits it ([ADR-0020](adr/0020-agent-notebook.md)).

Unknown policy names are rejected. Ordinary autonomous `--yes` runs default to `full`; TUI without `--yes` remains `read-only`.

One-shot Agent and Chat commands bridge `SIGINT` / `SIGTERM` into the Agent `AbortSignal`, matching the TUI cancellation semantics: active provider/shell work is cancelled and the Session is stored as `interrupted` for resume.

## Working notebooks

A run may keep a **working notebook**: a Markdown document with `## ` sections (Plan, Checklist,
Findings, Experiments, Decisions, Failures, Verification, Handoff) stored in the state database and
owned by the session ([ADR-0020](adr/0020-agent-notebook.md)). It is external working memory, not
Task State: no status transition, completion-gate decision, check result or job state depends on it,
and a notebook cannot declare a check passed.

```text
notebook_list   {} [session_id]                     titles, versions, sizes, sections
notebook_read   {notebook, [section], [session_id]}  the body, or one section
notebook_update {notebook, op, content, [section], [expected_version]}
```

`op` is `create` (a new title, no version), `replace` (needs `expected_version`), `append` (adds at
the end) or `section` (replaces one `## ` section, creating it at the end when absent). Every write
returns the new version and size; a conflict returns `{"error", "currentVersion"}` and nothing was
written, so the model re-reads and retries.

- Limits: body 256 KiB, title 1…120 characters, 32 notebooks per session, the newest 50 revisions of
  each notebook kept (older pruned in the same transaction).
- Context: when a run starts and its session has notebooks, the Start appends one user row (origin
  `notebook`) with at most 8 titles, versions, update times and section headings, capped at 600
  bytes, plus a line telling the model to read the Handoff section before continuing. Bodies are
  never injected: they are read with `notebook_read`. A session without notebooks gets no row.
- Subagents: a child run has its own notebooks. A run reads its own and those of its direct children
  (`session_id`), refuses any other session, and always writes its own - a parent merges a child's
  findings by reading them and writing its own notebook. Reading a child is a session-source read,
  as `read_thread` is: the result names the child, the tool row derives from it (revoking the child
  fences the reader; deleting it removes the copy), and a child withdrawn as a source reads as
  `{"session", "withdrawn": true}` with nothing of it.
- Hosts: `Notebooks`, `Notebook`, `CreateNotebook`, `SaveNotebook` (compare-and-set, `updated_by`
  `host`), `NotebookRevisions`, `NotebookRevision`, `DeleteNotebook` and `ExportNotebook` (Markdown and
  a file name). Writes publish `notebook.updated` / `notebook.deleted` (id, version, bytes) and a
  content-free journal row. The reads of a session withdrawn as a source return the same entries
  marked `withdrawn` with no title, heading or body; `ExportNotebook` and `SaveNotebook` are refused
  (`ErrNotebookWithdrawn`, checked inside the write).

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

The LSP client implements Content-Length JSON-RPC over stdio. Nothing is bundled and nothing is
installed: a language server runs only when its command is on `PATH`.

Built-in catalog (language -> command):

```text
go         -> gopls
typescript -> typescript-language-server --stdio   (.ts .tsx .mts .cts .js .jsx .mjs .cjs)
python     -> pyright-langserver --stdio           (.py .pyi)
rust       -> rust-analyzer                        (.rs)
```

Override or extend the catalog in the **global** `lumencortex.json` (`lsp` is read from the global
file only: a server is a process on this machine, and a project file must not name a command that
gets executed):

```json
{
  "lsp": {
    "idleTimeoutSeconds": 600,
    "maxServers": 3,
    "startTimeoutSeconds": 20,
    "requestTimeoutSeconds": 20,
    "servers": {
      "java": { "command": "/opt/jdtls/bin/jdtls", "args": [], "extensions": [".java"] }
    }
  }
}
```

Lifecycle, per workspace:

- **On demand.** A server starts on the first `lsp_*` call that needs its language, bounded by
  `startTimeoutSeconds`; nothing starts at Engine open. Only a run in a trusted workspace starts one
  (ADR-0013): in an untrusted workspace a run uses a server the host started and is offered no `lsp_*`
  tool while none runs.
- **Reuse.** One server per (workspace root, language); concurrent calls share it.
- **Idle reclaim.** One manager-level ticker stops a server unused for `idleTimeoutSeconds`; the next
  call starts it again.
- **Concurrency limit.** At most `maxServers` run at once. Starting another stops the least recently
  used idle server; when every server is busy the call is refused, never queued.
- **Crash.** A server that dies is marked unavailable with `lastError` and restarted on the next need
  after a doubling backoff (500 ms to 30 s). An explicit `LSPStart` ignores the backoff.
- **Close.** `Engine.Close` and `LSPStop` send `shutdown`/`exit`, then kill, and wait: no child process
  survives.
- **Cancellation and timeouts.** The caller's cancellation and the run's tool deadline
  (`protocol.ToolDeadline`) bound a whole `lsp_*` call: the wait behind another start of the same
  language, the start, and the request. A call whose deadline has already passed starts and sends
  nothing. Within that budget a start is bounded by `startTimeoutSeconds` and a request by
  `requestTimeoutSeconds`; a cancelled request sends `$/cancelRequest`.
- **Logging.** Start, ready, idle stop, crash and eviction are logged to the Engine's diagnostics
  log (`language`, `workspace`, `duration_ms`, `error_code`; OBS-001) and published as `lsp.*`
  events (`lsp.started`, `lsp.ready`, `lsp.idle_stop`, `lsp.crashed`, `lsp.evicted`,
  `lsp.unavailable`).

Tools the Agent Loop sees, and only when a server can really serve the workspace (its command is on
`PATH` and the workspace holds a file or a marker of its language):

```text
lsp_hover  lsp_definition  lsp_references  lsp_document_symbols
lsp_workspace_symbols  lsp_diagnostics  lsp_rename (write)
```

A workspace without a usable server gets none of them; a call that cannot reach a server answers with
`ok:false`, a `code` (`unavailable`, `timeout`, `busy`, `cancelled`, `failed`) and a hint telling the
model to use `search_text` or `find_files` instead.

`lsp_rename` asks the server for a workspace edit and applies it through the file tools' guard and
atomic batch (ADR-0016): `write` permission, work-tree mutation recorded, every path checked for
workspace confinement, protected state and symlink escape before anything is written, and the whole
rename refused if one path is.

Hosts read the state without starting anything:

```go
statuses, err := engine.LSPStatus(ctx, sessionID) // language, root, state, pid, startedAt, lastUsedAt, lastError
```

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

MCP tools are also added to normal agent runs unless `--no-mcp` is supplied.

### On-demand lifecycle and limits

No MCP server process exists when an Engine opens. A server starts when a run needs its tool list (its
catalog is not recorded yet) or when a tool of it is called, and stops after `idleTimeoutSeconds`
without a call, on `MCPStop` and on `Engine.Close`. A start that fails, a crash or a hung call is a
tool result with `ok:false` plus a `lastError`; the run continues.

The server entry accepts `transport` (`stdio` default, or `streamable-http` with a `url`; a
`unix:///abs/socket` url reaches a local daemon), `headers`, `env`, `include` (the only tools runs may
see), `callTimeoutSeconds`, `idleTimeoutSeconds`, `maxConcurrentCalls`, `maxResultBytes`, `session`
(ties a server-side session to the run) and `template`. A credential is only ever an `{env:NAME}`
reference, resolved at start or per request and never persisted.

`Engine.Capabilities` is the one view of everything a run could be offered — built-in, `lsp_*` and MCP
tools — with `source`, `permission`, `availability`, `executionConstraints` and `configRequirements`.
Listing it starts nothing. `Engine.MCPServerStates` is the per-server view (enabled, running,
requirements, last error). Templates for the official servers are data: `lcx-go plugins templates`,
`lcx-go plugins apply <id>`. See [plugins-mcp-templates.md](plugins-mcp-templates.md) and
[ADR-0021](adr/0021-capability-registry-and-mcp-lifecycle.md).

## Subagents

Subagents are isolated durable Agent sessions with a focused system prompt and a restricted tool working set.

Two child prompts are built in and not editable (they are part of the harness, adapted from
oh-my-pi under MIT — see `THIRD_PARTY_NOTICES.md`):

- **Explore** (read-only child): the launch forbids writes, commands and spawning, and it has no Git tool (it reads the working tree as it is; a parent that needs a diff examined puts it in the goal); the prompt asks for
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
| run context cancelled by the host (also during a tool, or when a storage/provider error is the cancellation) | `interrupted` | `cancelled` |
| run reached `Options.Deadline` (a request or tool in flight is cancelled; ADR-0019 §2) | `interrupted` | `deadline` |
| step budget exhausted | `interrupted` | `max_steps` |
| run budget refused the next tool call or model request (ADR-0019 §6) | `interrupted` | `budget:tool_calls`, `budget:model_requests`, `budget:tokens`, `budget:cost` |
| no tool calls and no content | `interrupted` | `empty_response` |
| reply cut at the output token limit (`finish_reason` `length`) without a tool call | `interrupted` | `output_limit` |
| provider or decision-provider error | `interrupted` | `provider:availability` |
| workflow human gate pending | `waiting_gate` | `workflow_gate` |
| storage error | `failed` | `storage` |
| workflow evaluation error | `failed` | `config` |
| panic in the run | `failed` | `panic` |
| completion accepted | `completed` | — |

If the terminal write itself fails, `Run` returns the cause joined with the write error, publishes no terminal event, and leaves the row `running`; the next run overwrites it. A panic's terminal event is `session.failed`, published after the write like the others, with the panic's stack.

A reply cut at the output limit is never accepted as the answer, in a conversation turn too: it stays in the transcript as the model's message, but the run does not complete with it. It ends the run `output_limit` only where the reply would have completed it: a completion-gate correction (workflow, open work units, failed required checks) still applies and the run continues. The output limit is OpenAI `length` and Anthropic `max_tokens`, and also Anthropic `model_context_window_exceeded` (the reply filled the context window). A provider stream counts as a response only when it ended: an OpenAI-compatible stream at a `finish_reason` or `[DONE]`, an Anthropic stream at `message_stop`. A stream that closes before that, carries an error event, or ends with a `finish_reason` by which the provider says it interrupted the generation (`insufficient_system_resource`, `aborted`; also in a non-streaming response) is an availability error (`stream_incomplete`) that the loop fails over and retries (`execution.retry`, see go-provider.md); the tool calls it carried so far are never executed. A reply the provider withheld, OpenAI `content_filter` or Anthropic `refusal` (streaming or not), is not a response either: it is a bad request, not retried because the same request would meet the filter again, and the run fails `provider:bad_request` without running any call it held or keeping its text. A reply cut at the output limit that holds tool calls continues the run: the calls before the last were finished before the cut and run; the last call was being written when the limit cut the reply and is answered with a tool error and never runs, whatever its arguments, because servers that parse the model's own call format or return a parsed input object close the JSON themselves. When `MaxToolCallsPerStep` dropped the tail of the reply, every call kept was finished and runs.

Tool-call arguments must be a JSON object to run. Text that is not valid JSON, and text that is a JSON string, are stored as a JSON string holding the raw text; other JSON is stored as is. Arguments that are not an object are never executed: the model receives a tool error for that call, and the OpenAI-compatible adapter sends a stored JSON string back as its raw text and any other value as its JSON text, so the model sees what it sent. A call that arrived with no arguments at all is stored as the empty object only when the provider says the model ended the reply itself (`stop`, `tool_calls`; Anthropic `end_turn`, `tool_use`, `stop_sequence`), because some servers send nothing for an argumentless call. After any other finish (`length`, an unknown reason, or an OpenAI-compatible stream that ended at `[DONE]` without a `finish_reason`) nothing shows the call was finished, so it is stored as an empty JSON string and answered with the tool error above. The `bash` tool is the exception: a command the run's cancellation or deadline stops while it executes is answered before the run ends, with one tool row carrying the command's stopped result (`origin: tool_interrupted`) and its admitted execution settled `outcome_unknown` in the same transaction, so a finishing run leaves a paired transcript and terminal provenance. Every other tool keeps the previous behaviour: a stopped call is left without a result row and Reconcile closes it at the next Start. So does `bash` when the process dies before that row commits.

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
