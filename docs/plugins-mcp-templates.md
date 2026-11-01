# Plugins, MCP servers and capability templates

This page describes how LumenCortex Core exposes tools from several sources, how an MCP server is
started and stopped, and what each shipped template needs before it can be used. The decision record is
[ADR-0021](adr/0021-capability-registry-and-mcp-lifecycle.md); the plugin (in-process) contract is
[ADR-0012](adr/0012-core-plugin-platform.md).

## The capability registry

`Engine.Capabilities(ctx, CapabilityQuery{SessionID, Denylist, Capability})` returns every tool a run
could be offered in a workspace. Listing it starts nothing: no MCP process, no language server.

```json
{
  "toolId": "mcp__exa__web_search_exa",
  "displayName": "web_search_exa",
  "source": "mcp",
  "provider": "exa",
  "capability": "web.search",
  "permission": "read",
  "enabled": true,
  "availability": { "state": "needs-configuration", "detail": "environment variable not set: EXA_API_KEY" },
  "executionConstraints": {
    "timeoutSeconds": 60, "idleTimeoutSeconds": 600, "maxConcurrentCalls": 2,
    "maxResultBytes": 262144, "mutatesWorkspace": false, "requiresApproval": false
  },
  "configRequirements": { "missingEnv": ["EXA_API_KEY"] },
  "lastError": "",
  "template": "exa"
}
```

Sources:

| `source` | what it is | how it is switched |
| --- | --- | --- |
| `builtin` | compiled into Core (`read_file`, `apply_patch`, the command tool, …) | not switchable; the run's policy decides |
| `core-optional` | needs something attached: the `lsp_*` tools need a language server | attach or stop the language server (`LSPStart`) |
| `mcp` | a remote MCP endpoint (Exa, GitHub) | `SetMCPServerEnabled` / `SetCapabilityEnabled` |
| `local-adapter` | a local process or Unix-socket daemon (Playwright MCP, CodeGraph, CodeBridge) | the same two calls |

`availability.state` is `available`, `disabled`, `needs-configuration`, `untrusted`, `error` or
`unknown`; it is computed from configuration, the environment, `PATH`, the Unix socket and the last
error, never by starting something.

`lcx-go plugins capabilities` prints the same view; `--denylist shell,read_file` reports entries as
disabled with `disabledReason: "denylist"` and keeps them visible.

## On-demand MCP lifecycle

- No MCP process exists when an Engine opens. The first call that needs a server starts it; concurrent
  first calls share one start.
- A server stops after `idleTimeoutSeconds` without a call (default 600 s), on `MCPStop`, and on
  `Engine.Close`.
- `callTimeoutSeconds` (default 120 s), the run's tool deadline and cancellation all bound one call;
  the earliest wins.
- `maxConcurrentCalls` (default 4) bounds calls in flight, `maxResultBytes` (default 1 MiB) bounds one
  result.
- A start failure, a crash or a hung call is a **tool result** with `ok:false` plus a `lastError` in the
  registry. The run continues; the dead client is dropped and the next call starts a fresh process.
- The tool list a start discovers is cached at `<state>/mcp-catalog/<workspace>/<server>.json`, keyed by
  a fingerprint of the server definition, so a later run can be offered the tools without a start.

### `mcp.json` fields

```json
{
  "servers": {
    "example": {
      "transport": "streamable-http",
      "url": "https://example.test/mcp",
      "headers": { "Authorization": "Bearer {env:EXAMPLE_TOKEN}" },
      "include": ["search", "fetch"],
      "tools": { "fetch": { "permission": "read" } },
      "callTimeoutSeconds": 60,
      "idleTimeoutSeconds": 600,
      "maxConcurrentCalls": 2,
      "maxResultBytes": 262144,
      "disabled": false,
      "template": "exa"
    }
  }
}
```

- `transport` is `stdio` (default: `command`, `args`, `env`) or `streamable-http` (`url`, `headers`). A
  `unix:///abs/socket` url reaches an HTTP endpoint served on a Unix socket (path `/mcp`, Host
  `localhost`) — the form CodeBridge's local daemon accepts.
- A credential is only ever an `{env:NAME}` reference. It is resolved when a process starts or a request
  is made; the value is never written to a file, an error, an event or a log.
- `include` is the only set of tools runs may see from the server (empty = every tool it reports).
- `session` binds a server that controls a long-lived resource to the run:
  `{"argument":"session_id","startTool":"computer_start","sessionIdField":"computer_session_id","stopTool":"computer_stop","stopArgument":"computer_session_id"}`.
  Core injects the LumenCortex session id, remembers the server-side id, and closes it when the server
  stops.

Model-visible tool names are `mcp__<server>__<tool>`, so two servers may report the same tool name
without colliding.

## Templates

`lcx-go plugins templates` lists them; `lcx-go plugins apply <id> [--scope global|project]` writes one
as a **disabled** entry; `lcx-go plugins server enable <id> [--scope …]` turns it on. Applying never
enables, and never overwrites an existing entry.

Each template is a JSON document under `internal/mcptemplates/templates/`, validated in tests against
the same loader the runtime uses, so a shipped template can never describe a server the loader refuses.

### Exa — `web.search` (remote, off by default)

- Endpoint `https://mcp.exa.ai/mcp`; auth keyless (free rate limit), OAuth (`?login`), or an
  `x-api-key` header. The template ships **keyless**: no credential is needed to search and fetch. For a
  paid plan add `"headers": {"x-api-key": "{env:EXA_API_KEY}"}` to the entry.
- Default tools: `web_search_exa`, `web_fetch_exa` only. `agent_run` (multi-step, usage-billed) and the
  advanced search tool are **not** exposed.
- Limits: 60 s per call, 2 concurrent, 256 KiB per result, idle stop after 10 min.
- Cost: external paid service. Keyless search/fetch is rate limited and free; an API key or OAuth bills
  the Exa account. Nothing is invoked automatically.
- Verified 2026-11-01 against `docs.exa.ai/reference/exa-mcp`. **Live handshake NOT_RUN** — it needs
  network egress and would spend credits.

### GitHub — `code.host.read` (remote, off by default)

- Endpoint `https://api.githubcopilot.com/mcp/`, `Authorization: Bearer {env:GITHUB_PERSONAL_ACCESS_TOKEN}`,
  `X-MCP-Toolsets: repos,issues,pull_requests,actions`, `X-MCP-Readonly: true`.
- Default tools (curated read set): `get_file_contents`, `search_code`, `list_branches`,
  `list_commits`, `issue_read`, `list_issues`, `pull_request_read`, `list_pull_requests`,
  `actions_list`, `actions_get`, `get_job_logs`, `get_commit`.
- Write tools (create a pull request, push, workflow dispatch, comments) are **not** in the include
  list. Enabling one is an explicit edit of the entry, and it keeps `exec` permission, so the run's
  permission gate must admit it.
- Verified 2026-11-01 against `github/github-mcp-server` `docs/remote-server.md`. **Live handshake
  NOT_RUN** — it needs a token; no token on this machine was used.

To run it locally instead:

```bash
docker run -i --rm -e GITHUB_PERSONAL_ACCESS_TOKEN -e GITHUB_TOOLSETS=repos,issues,pull_requests \
  ghcr.io/github/github-mcp-server
```

with `{"transport":"stdio","command":"docker","args":["run","-i","--rm","-e","GITHUB_PERSONAL_ACCESS_TOKEN","ghcr.io/github/github-mcp-server"]}`.

### Browser — `browser.control` (local process, off by default)

- `@playwright/mcp` (official Playwright MCP server) over stdio, started on the first browser call.
- Flags: `--headless --isolated --browser chrome --timeout-action 15000 --timeout-navigation 30000
  --idle-timeout 300000 --output-dir .lumencortex/browser`.
- `--isolated` keeps the profile in memory; `--idle-timeout` closes the browser before Core stops the
  server (Core's own idle stop is 600 s). Downloads never happen: `playwright-mcp` must already be on
  `PATH` (`npm i -g @playwright/mcp`, then `npx playwright install chromium`); a missing binary is
  reported as `needs-configuration`, not installed.
- Web content is untrusted: a page's text is data, never an instruction. Independent of Computer Use.
- Verified 2026-11-01 against `@playwright/mcp` 0.0.82 `--help`. A live stdio handshake through Core
  was run locally (see the report) — no credential, no paid call.

### Computer Use — `computer.control` (local socket, off by default)

- Connects to CodeBridge's daemon: `unix://{env:HOME}/Library/Application Support/CodeBridge/run/mcp.sock`,
  the same-user `local_mcp` class (peer UID on a 0600 Unix socket, no bearer token).
- Tools: `computer_start`, `computer_observe`, `computer_action`, `computer_status`, `computer_stop`.
  All are `exec`: only a full-policy run sees them, and Core does not implement screen capture or
  accessibility control — it never reimplements what CodeBridge does.
- The `session` binding injects the LumenCortex session id into `session_id`, remembers the returned
  `computer_session_id`, and calls `computer_stop` when the server stops or the Engine closes.
- CodeBridge's own approval gate still decides each action; Core does not bypass it. `computer_start`
  returns `approval_required` until the human approves, and that is reported as a tool result.
- Verified 2026-11-01 against the local CodeBridge repository (`internal/daemon/ingress.go`,
  `internal/daemon/mcpcomputertools.go`) and by a live handshake against the running daemon.

### CodeGraph — `code.graph` (experimental, local process, off by default)

- `code-graph-mcp` (npm `@sdsrs/code-graph`) over stdio, working directory = workspace, so its index is
  per workspace.
- Tools: `semantic_code_search`, `get_call_graph`, `find_references`, `impact`, `project_map`,
  `module_overview`, `get_ast_node`.
- **Experimental and off by default.** The upstream server indexes the workspace on the first query call
  and re-indexes changed files later; it has no documented switch to answer without an index, so "never
  indexes by default" holds only because the template is disabled until a user enables it. It also
  installs a missing engine binary and self-updates by default (`CODE_GRAPH_NO_AUTO_UPDATE=1` is set by
  the template, which stops the update check but not the first-run install), which is why the template
  requires the binary to be installed by the user and never uses `npx`.
- Value compared with Core's own LSP tools (see below): no benefit was **measured**. The template is a
  shape plus a diagnostic, not a claim.

## CodeGraph vs. the LSP tools

| question | LSP tools | CodeGraph |
| --- | --- | --- |
| definition of a symbol at a position | `lsp_definition` | `get_ast_node` (by node id) |
| references | `lsp_references` (semantic, per position) | `find_references` (graph edges by name) |
| symbols in a file / workspace | `lsp_document_symbols`, `lsp_workspace_symbols` | `project_map`, `module_overview` |
| callers / transitive callers | not offered | `get_call_graph` |
| impact of a change | not offered | `impact` |
| concept search | not offered (use `search_text`) | `semantic_code_search` |

The LSP answers are compiler-grade but per language server and per position; the graph answers are
repo-wide but name-resolved (qualified calls, interface dispatch and reflection produce no edge — its
own README lists those boundaries). The real difference is cost: the LSP needs a running language
server, the graph needs an index. **No measurement of CodeGraph's benefit on this repository was taken**
(it is not installed here), so no improvement is claimed.

## Execution and permissions

Every tool from every source runs through one path: `toolset.Set.Execute` → the run's policy
(`read-only` sees `read` only, `workspace` adds `write`, `exec` needs `full`) → `protocol.ToolDeadline`
→ the tool events and the execution records. A server's own `readOnlyHint` decides its default
permission; `tools: {"<name>": {"permission": "read"}}` overrides it, but only from the global file or a
trusted project (ADR-0013).

## Workspace trust

A server defined by `<workspace>/.lumencortex/mcp.json` is a project input. While the workspace is not
listed in `trustedWorkspaces` it is ignored: `MCPStart` refuses with `ErrUntrustedWorkspace`, no process
is created, and the registry reports its tools with `availability.state: "untrusted"` and
`enabled: false`. The global `mcp.json` is the user's own configuration and is never gated.

## What Core does not do

- No plugin marketplace, no auto-install, no Go `plugin` loading, no package download.
- No claim that an in-process plugin is sandboxed: `pluginapi` runs with Core's authority (ADR-0012 D9).
  A local MCP server is separated by a process boundary, not by an OS sandbox, and runs with the user's
  own authority.
- No second execution path: the registry lists tools, it does not run them.

## Commands

```bash
lcx-go plugins                        # in-process plugins
lcx-go plugins capabilities           # the unified registry
lcx-go plugins cost                   # tool-schema bytes per source
lcx-go plugins templates              # shipped MCP templates
lcx-go plugins apply browser --scope global
lcx-go plugins server enable browser --scope global
```
