# ADR-0021: Unified capability registry, on-demand MCP lifecycle and server templates

- **Status:** Accepted
- **Date:** 2026-11-01
- **Owners:** project owner
- **Supersedes:** none
- **Amends:** ADR-0012 (the plugin platform stays the in-process contract; this ADR adds the
  out-of-process path and the one capability view), ADR-0013 (workspace trust now also gates what the
  registry reports as available)

## Context

Core had three separate ways to reach a tool and no single answer to "what can this run do, and where
does it come from":

- built-in tools, registered in `internal/toolset` and gated by the run's policy;
- optional language-server tools, registered when a manager happens to be attached;
- MCP servers, configured in `<GlobalDir>/mcp.json` and `<workspace>/.lumencortex/mcp.json`, whose
  tools were only visible after a host started the process (`MCPStart`) — `AgentTools` listed running
  instances only.

Consequences: a host could not show the model's reachable surface without starting servers; a run
started every MCP server the host had started, whether or not it needed it; a server's tool list was
lost across restarts; and there was no per-server bound on call time, concurrency or result size. The
in-process `pluginapi` path (ADR-0012) is for trusted built-in code and explicitly not a sandbox, so
third-party integrations had no place at all.

## Decisions

### D1 — One registry, four sources

`Engine.Capabilities(ctx, CapabilityQuery)` returns every tool a run could be offered in the query's
workspace, whatever provides it. Each entry carries `toolId`, `displayName`, `source`, `provider`,
`capability`, `permission`, `enabled`, `availability`, `executionConstraints`, `configRequirements`,
`lastError` and (when switched off) `disabledReason`.

`source` is one of:

| source | meaning | example |
| --- | --- | --- |
| `builtin` | compiled into Core | `read_file`, the command tool, the job, notebook, wake-up, thread, skill, task and subagent tools, `start_task` |
| `core-optional` | offered when a dependency can serve the workspace | `lsp_definition` (a configured language server on `PATH`) |
| `mcp` | a remote MCP endpoint | Exa, GitHub |
| `local-adapter` | a local process or local socket running third-party code | Playwright MCP, CodeGraph, CodeBridge |

`capability` is the semantic job (`repo.read`, `repo.write`, `process.exec`, `code.intelligence`,
`web.search`, `code.host.read`, `browser.control`, `computer.control`, `code.graph`, and for Core's own
agent tools `context.read` (threads, skills), `agent.state` (notebook, wake-ups, work units, notes)
and `agent.delegation` (subagents, `start_task`)). Several sources may provide one capability; the
registry keeps them apart by `provider` and by the model-visible tool name, so two providers of "web
search" never collide.

The `builtin` rows and the built-in part of `Engine.ToolSchemaCost` are read from a toolset built
exactly as a host's run builds it (the one `EffectivePrompt` reports), not from a separate list, so a
tool family added to the run appears in the registry and in the cost without a second edit. The cost
counts a top-level run under the full policy without a delegation grant; the registry also lists
`start_task`.

The registry describes tools; it does not run them. Every tool still executes through the same path as
before — `toolset.Set.Execute`, the permission gate, `protocol.ToolDeadline`, the tool events and the
execution records. There is no second execution path and no second enablement store: enabling an MCP
tool edits the `include` list of its server entry in the scope that entry came from, enabling a server
writes `disabled` in the same file, and a plugin's enablement stays in `<GlobalDir>/plugins.json`.

### D2 — Availability is computed, never probed by starting

`availability` is `available`, `disabled`, `needs-configuration`, `untrusted`, `error` or `unknown`,
computed from configuration, the environment (`{env:NAME}` references that are unset), `exec.LookPath`
for a stdio command, the existence of a Unix socket endpoint, the cached tool catalog and the last
error. Listing capabilities starts no process. A server's `include` list lets the registry describe
what enabling it would offer before its catalog was ever discovered; those rows are `unknown` until
the first start records the catalog.

### D3 — On-demand lifecycle

`internal/mcp` now owns one lifecycle per (workspace, server):

- **Nothing starts at Engine open.** No MCP process exists until a call needs it. The registry,
  `MCPTools`, `MCPStatuses`, `Capabilities` and the step tool list all read configuration and cached
  catalogs only.
- **A run discovers what it does not know.** A run that may use MCP starts, once, the enabled servers
  whose catalog was never recorded (`discoverMCPCatalogs`): concurrently, under a 10 s total budget,
  and skipped for a server whose start failed within a 2-minute backoff. In the steady state — a
  catalog recorded by an earlier start — this starts nothing. A server that cannot start contributes no
  tools and never delays the run beyond that budget.
- **Single-flight start.** Concurrent first calls share one start; the start is bounded by
  `DefaultCatalogTimeout` (8 s) so a server that never answers cannot hold a run.
- **Idle stop.** After the last call, a timer of `idleTimeoutSeconds` (default 10 min) closes the
  server; a call that arrives first re-arms it. `Engine.Close` and `MCPStop` close it immediately.
- **Per-server limits.** `callTimeoutSeconds` (default 120 s) bounds one call together with the run's
  tool deadline (the earlier wins) and the caller's cancellation; `maxConcurrentCalls` (default 4)
  bounds calls in flight; `maxResultBytes` (default 1 MiB) bounds one result. A call past the deadline
  sends `notifications/cancelled` and returns a timeout.
- **Failure isolation.** A start failure, a crash (EOF / closed pipe) or a hung call becomes a tool
  result error plus `lastError` in the registry; the client is dropped so the next call starts a fresh
  process; the run continues. Nothing in the MCP path can end the loop.
- **Cached catalog.** A start records its tool list to `<state>/mcp-catalog/<workspace key>/<id>.json`,
  keyed by a fingerprint of the server definition, bounded by 1 MiB and 512 tools. A run is offered
  those tools without a start; an edited definition never reuses the old list.

### D4 — Exposure: include lists and namespacing

`mcp.json` gains `transport` (`stdio` default, `streamable-http`), `url` (http(s) or `unix:///abs/sock`),
`headers`, `env`, `include`, the four limits, `session` and `template`. A credential is only ever an
`{env:NAME}` reference, resolved when a process starts or a request is made; the resolved value is
never written to a file, an error, an event or a log line.

An enabled server exposes only the tools in its `include` list (empty means every tool it reports,
bounded by 512). Model-visible names keep the existing `mcp__<server>__<tool>` namespacing, so two
servers may report the same tool name. Installing many servers therefore does not put their tools in
front of the model: a server contributes nothing until it is enabled, and only its included tools
reach a step.

### D5 — Session binding for long-lived resources

A server that controls a long-lived resource (a computer session) may declare a `session` binding:
Core injects the LumenCortex session id into one tool argument (replacing a model-supplied value),
remembers the server-side id a start tool returned, and calls the stop tool for each remembered id
when the server is stopped or the Engine closes. The model never chooses the id, and nothing a run
opened outlives the run.

### D6 — Templates are data, disabled, and credential-free

`internal/mcptemplates/templates/*.json` ships the official server templates (Exa, GitHub, Browser via
Playwright MCP, Computer Use via CodeBridge, CodeGraph). Each is embedded in the binary, validated
against the same loader the runtime uses (`mcp.ValidateConfig`), applied by
`Engine.ApplyMCPTemplate(ctx, scope, id)` or `lcx-go plugins apply <id>`, and written **disabled**. A
template is never enabled by applying it, never overwrites an existing entry, and carries no value that
looks like a credential: a header or environment entry is an `{env:NAME}` reference, and a template with
a literal credential-looking value fails validation. Each template states its endpoint, auth form,
cost, default tool list, what was verified and when, and what was not verified.

### D7 — Third-party code runs out of process

A third-party integration is a separate process (stdio MCP server) or a remote endpoint; Core never
loads a Go `plugin`, never installs a package and never downloads a binary. What Core guarantees for
these servers:

- explicit enable (a disabled entry, then a deliberate switch), explicit permissions per tool;
- its own timeout and cancellation per call, and an idle stop;
- error isolation (a failure is a tool result) and resource limits (concurrency, result size);
- no access to Core's state: the process gets a working directory, an environment and a JSON-RPC pipe,
  nothing else;
- workspace trust is not bypassed: a project-defined server stays invisible and unstartable while the
  workspace is untrusted (ADR-0013).

What Core does not claim: an in-process plugin is still trusted code with Core's authority (ADR-0012
D9), and a local MCP server runs with the user's own authority — its isolation is process separation
and an explicit configuration, not an OS sandbox. `pluginapi` is not a sandbox and this ADR does not
make it one.

## Invariants

- Opening an Engine starts no MCP process, whatever is configured.
- No tool reaches a step's list unless its server is enabled, its workspace is trusted and its name is
  in the server's `include` list.
- A failure of any MCP server is a tool result and a `lastError`; it never ends a run.
- Every tool call, from any source, passes the same permission gate, tool deadline and event path.
- No credential is persisted, logged or reported; only the `{env:NAME}` reference is.
- A server-side session opened through a bound server is closed when the server stops.

## Alternatives considered

### Start every configured server at open and keep them running

The previous shape. Rejected: idle processes and memory for servers a run never uses, and no way to
report availability without starting.

### Discover catalogs at run start for every enabled server

Rejected: it turns "connect on demand" into "connect every run". A template's `include` list already
describes what a server offers; a hand-written server without one is discovered by an explicit start
(the host's action or the first call), and the catalog then persists.

### A new store for enablement and per-tool switches

Rejected: `mcp.json` already owns server configuration and `plugins.json` plugin enablement. A second
store would let the two disagree.

### Attach every tool schema to every request and let the model choose

Rejected: it is the byte cost and the selection-by-hope this ADR avoids. Selection is configuration
(enable + include) plus the run's policy; no extra model call is made to pick tools.

## Consequences

- Positive: one view for hosts; nothing starts until used; a server's failure is visible and contained;
  third-party integrations have a defined, non-privileged path; templates make the common servers a
  two-step, explicit act.
- Negative: a server without an `include` list contributes no tools until it was started once; the
  first call to a server pays its start latency; a cached catalog can describe a server whose tool set
  changed upstream until the next start refreshes it.

## Validation

`internal/mcp`: the existing client, registry and trust tests, plus the lifecycle suite in
`runtime/mcp_lifecycle_test.go` — nothing starts at open (measured by the helper process's marker file
and the child-process count), start on first call, a run discovering a catalog and calling the tool,
idle stop and restart, crash → tool result + `lastError` (alone and inside a real run, which then
completes), hung call cut by the call timeout and by the tool deadline, include/disable exposure,
denylist reporting, a disabled server absent from the step tool list, catalog across a restart, session
binding injection and close, template validation and apply, and the schema byte cost. See `docs/reports/plugin-capability-registry-2026-11-01.md` for the
measured numbers and what was not run.
