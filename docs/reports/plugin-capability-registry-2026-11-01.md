# Capability registry, on-demand MCP lifecycle and server templates — 2026-11-01

- **Status:** implemented and validated locally on branch `rt/plugins`; not pushed, not merged.
- **Base:** `ba0f7a5` (`rt/execution-runtime`: ADR-0019 execution runtime base, shell timeout, run
  deadline).
- **Decision:** [ADR-0021](../adr/0021-capability-registry-and-mcp-lifecycle.md) (new), amending
  [ADR-0012](../adr/0012-core-plugin-platform.md) and [ADR-0013](../adr/0013-project-inputs-and-workspace-trust.md).
- **User documentation:** [plugins-mcp-templates.md](../plugins-mcp-templates.md).

## Scope

One capability registry over built-in tools, Core-optional (LSP) tools and MCP tools; an on-demand MCP
lifecycle (nothing at Engine open, idle stop, per-server limits, failure isolation, cached catalogs);
per-server `include` lists; five official MCP templates as data, applied disabled; a CLI surface; the
docs. Core only — the Desktop Plugins page is the integrator's.

Not in scope, deliberately: a marketplace, package installation, Go `plugin` loading, a second
enablement store, and any change to the tool execution path (permission gate, tool deadline, events,
execution records are untouched).

## What changed

| Area | File(s) |
| --- | --- |
| Capability registry, per-tool/per-server switches, schema byte cost | `runtime/capabilities.go` (new) |
| Template API (`MCPTemplates`, `ApplyMCPTemplate`) | `runtime/templates.go` (new) |
| Server states, discovery at run start, stop events | `runtime/mcp.go`, `runtime/engine.go`, `runtime/agent.go` |
| Lifecycle: single-flight start, idle timer, call timeout, concurrency, session binding | `internal/mcp/session.go` (new) |
| Streamable HTTP transport, `unix://` endpoints, `{env:NAME}` headers | `internal/mcp/httpclient.go` (new) |
| Catalog persistence and fingerprinting | `internal/mcp/catalog.go` (new) |
| Config: `transport`, `url`, `headers`, `env`, `include`, limits, `session`, `template` | `internal/mcp/types.go` |
| Registry: states, missing catalogs, per-workspace lifecycle table | `internal/mcp/registry.go` |
| Static spec lists so the registry cannot drift from the toolset | `internal/toolset/toolset.go`, `internal/toolset/lsp.go` |
| MCP tool results are results, not errors; session id injected | `internal/toolset/mcp.go` |
| Templates as embedded JSON + validation + apply | `internal/mcptemplates/` (new) |
| CLI: `plugins capabilities/cost/templates/apply/server` | `cmd/lcx-go/plugins.go` (new), `cmd/lcx-go/import.go` |

## Public API (Core, consumed by Desktop)

```go
// The registry view: one entry per tool a run could be offered, nothing started.
func (e *Engine) Capabilities(ctx context.Context, query CapabilityQuery) ([]Capability, error)
func (e *Engine) ToolSchemaCost(ctx context.Context, query CapabilityQuery) (ToolSchemaCost, error)
func (e *Engine) SetCapabilityEnabled(ctx context.Context, toolID string, enabled bool) error
func (e *Engine) SetMCPServerEnabled(ctx context.Context, scope, serverID string, enabled bool) error
func (e *Engine) MCPServerStates(ctx context.Context, sessionID string) ([]mcp.ServerState, error)
func (e *Engine) MCPTemplates() []MCPTemplate
func (e *Engine) ApplyMCPTemplate(ctx context.Context, scope, id string) (MCPConfig, error)

type CapabilityQuery struct {
	SessionID  string   `json:"sessionId,omitempty"`
	Denylist   []string `json:"denylist,omitempty"`
	Capability string   `json:"capability,omitempty"`
}

type Capability struct {
	ToolID               string                  `json:"toolId"`
	DisplayName          string                  `json:"displayName"`
	Source               string                  `json:"source"`               // builtin | core-optional | mcp | local-adapter
	Provider             string                  `json:"provider,omitempty"`
	Capability           string                  `json:"capability"`
	Permission           string                  `json:"permission"`
	Enabled              bool                    `json:"enabled"`
	Availability         CapabilityAvailability  `json:"availability"`         // {state, detail}
	ExecutionConstraints CapabilityConstraints   `json:"executionConstraints"`
	ConfigRequirements   CapabilityRequirements  `json:"configRequirements"`
	LastError            string                  `json:"lastError,omitempty"`
	DisabledReason       string                  `json:"disabledReason,omitempty"` // user | denylist | trust | unavailable
	Template             string                  `json:"template,omitempty"`
}

type CapabilityConstraints struct {
	TimeoutSeconds     int  `json:"timeoutSeconds,omitempty"`
	IdleTimeoutSeconds int  `json:"idleTimeoutSeconds,omitempty"`
	MaxConcurrentCalls int  `json:"maxConcurrentCalls,omitempty"`
	MaxResultBytes     int  `json:"maxResultBytes,omitempty"`
	MutatesWorkspace   bool `json:"mutatesWorkspace"`
	RequiresApproval   bool `json:"requiresApproval"`
}

type CapabilityRequirements struct {
	MissingEnv       []string `json:"missingEnv,omitempty"`
	MissingBinary    string   `json:"missingBinary,omitempty"`
	MissingEndpoint  string   `json:"missingEndpoint,omitempty"`
	NeedsTrust       bool     `json:"needsTrust,omitempty"`
	NeedsLanguageSrv bool     `json:"needsLanguageServer,omitempty"`
}

type ToolSchemaCost struct {
	BuiltinBytes int            `json:"builtinBytes"`
	LSPBytes     int            `json:"lspBytes"`
	MCPBytes     map[string]int `json:"mcpBytes"`
	TotalBytes   int            `json:"totalBytes"`
	BuiltinTools int            `json:"builtinTools"`
	LSPTools     int            `json:"lspTools"`
	MCPTools     map[string]int `json:"mcpTools"`
}
```

`availability.state` is `available | disabled | needs-configuration | untrusted | error`. Events:
`capability.changed` (per-tool or per-server switch), `mcp.template.applied`, `mcp.started`,
`mcp.stopped` (now with `reason: "idle"` when the registry stopped it by itself).

## Measured numbers

Machine: Apple silicon (darwin 27.0.0, arm64), Go 1.26.5 toolchain against `go 1.23.0` language
version. Each figure is one local run unless stated.

### Processes and memory at Engine open (step 2)

With all five templates applied **and enabled**, opened through `runtime.Open` with the real store:

| measurement | value |
| --- | --- |
| configured servers | 5 (exa, github, browser, computer-use, codegraph) |
| servers running after `Open` | 0 |
| child processes before `Open` / after `Open` | 1 / 1 (Δ **0**) |
| capabilities listed without starting anything | 58 |

`ps` over the whole descendant tree of the test process, so a grandchild would be counted.

### Start latency and footprint of a stdio server (step 2)

- Test server (`os.Args[0]` re-executed as a stdio MCP server, no network): **13.5 ms** from first call
  to result (spawn + initialize + tools/list + call).
- Playwright MCP 0.0.82 (`node cli.js --headless --isolated`): **227 ms** start (spawn + initialize +
  tools/list), **416 ms** first `browser_tabs` call including Chromium launch. Tree RSS 139 MiB after
  start, 648 MiB / 7 processes after the first browser call; back to 1 process after `Engine.Close`.
- CodeBridge local daemon over the Unix socket: **~40 ms** handshake, 40 tools, 0 extra processes
  (the daemon already runs).

### Request tool-schema bytes (step 3)

JSON size of the spec list a request would carry (`name`, `description`, `parameters`), measured with
`Engine.ToolSchemaCost`:

| configuration | tools | bytes |
| --- | --- | --- |
| built-ins only | 10 | 3,600 |
| built-ins + LSP (attached) | 10 + 7 | 3,600 + 2,204 (specs only; no language server was started) |
| + exa (2 included tools) | +2 | not measured live (no credential, no paid call) |
| + github (12 included tools) | +12 | not measured live (no token) |
| + browser (15 included tools, live listing) | +15 | **+10,137** |
| + computer-use (5 included tools, live listing) | +5 | **+3,092** |
| + codegraph (7 included tools) | +7 | not measured live (binary not installed) |

Live totals actually observed: built-ins 3,600; built-ins + browser 13,737; built-ins + computer-use
6,692. The three unmeasured templates are configuration-only in this environment; their cost is
bounded by the number of included tools they name.

## Live checks (step 3 of the task)

| template | diagnostic run | live handshake |
| --- | --- | --- |
| Exa | `plugins capabilities` → `needs-configuration` only with a paid/authenticated entry; keyless entry reports `available` | **NOT_RUN** — needs network egress to `mcp.exa.ai` and would spend credits. A user runs: `lcx-go plugins apply exa --scope global && lcx-go plugins server enable exa --scope global && lcx-go plugins capabilities` |
| GitHub | `needs-configuration: environment variable not set: GITHUB_PERSONAL_ACCESS_TOKEN` | **NOT_RUN** — needs a PAT; no token on this machine was read or used. A user runs: `export GITHUB_PERSONAL_ACCESS_TOKEN=…; lcx-go plugins apply github --scope global && lcx-go plugins server enable github --scope global` |
| Browser | `needs-configuration: command not found: playwright-mcp` (the package is cached under `~/.npm/_npx`, not on `PATH`) | **RUN** — a stdio handshake through Core's client against the cached `@playwright/mcp` 0.0.82: 25 tools in 285 ms, 20,117 bytes of schemas; the template's include list was curated from that listing. No credential, no paid call. |
| Computer Use | `available` (the daemon socket exists) | **RUN** — Streamable HTTP over `unix://…/run/mcp.sock`: 40 tools listed, `computer_start/observe/action/status/stop` among them. No Computer Use session was opened. |
| CodeGraph | `needs-configuration: command not found: code-graph-mcp` | **NOT_RUN** — the binary is not installed, and Core does not install executables. |

## Tests run

All on branch `rt/plugins`, worktree `wt/core-rt-plugins`.

| command | result |
| --- | --- |
| `go build ./...` | pass |
| `go vet ./internal/mcp ./internal/mcptemplates ./internal/toolset ./runtime ./cmd/lcx-go` | pass |
| `gofmt -l` on the files this change touches | clean |
| `go test -count=1 ./internal/mcp` | pass |
| `go test -count=1 ./internal/mcptemplates` | pass |
| `go test -count=1 ./internal/toolset` | pass |
| `go test -count=1 ./cmd/lcx-go` | pass |
| `go test -count=1 -race ./internal/mcp -run 'TestHTTPClient\|TestRegistry'` | pass |
| `go test -count=1 -race ./runtime -run 'TestMCP\|TestCapability\|TestSetCapability\|TestToolSchemaCost'` | pass |
| `go test -count=1 ./runtime` | pass (full package) |

New coverage, all against real processes, the real store and a real restart:

- `runtime/mcp_lifecycle_test.go`: nothing starts at `Open` (helper marker file + descendant process
  count); start on the first call; a run discovering the catalog and calling the tool; idle stop, the
  process gone, and a restart on the next call; a crash → tool result + `lastError`, alone and inside a
  real run that then completes; a hung call cut by the call timeout and by `protocol.ToolDeadline`;
  `include` and server disable deciding exposure; a disabled server absent from the step tool list;
  denylist reporting; the cached catalog across close+reopen; session binding injection (a
  model-supplied id is replaced) and close on `Engine.Close`; template validity/apply/reload; per-tool
  switching; the schema byte cost.
- `internal/mcp/httpclient_test.go`: handshake and call over HTTP, an SSE response with a preceding
  notification, `{env:NAME}` header resolution, a missing reference failing with the variable name, a
  hung call cut by the server timeout, and a `unix://` endpoint with no TCP listener.
- `internal/mcptemplates/templates_test.go`: every shipped template is a valid disabled entry that
  names itself, a stored credential is refused, an `{env:NAME}` reference is accepted, an unknown id is
  refused, and the describe view names endpoint/tools/verification.

## Commits

`2f969b2` (feature), `0ed8bbc` (close stopped servers and compute requirements outside the registry
lock). Both local on `rt/plugins`; nothing pushed.

## Deviations from the task brief, and why

- The stdio test server is the test binary re-executed as an MCP server (`-test.run=TestRuntimeMCPServer`)
  rather than a program under `testdata/`. It is still a tiny local Go stdio MCP server with no network,
  and it is this repository's existing convention (`internal/mcp/client_test.go`), so it needs no
  `go build` inside a test and no extra toolchain on the test machine.
- `availability` has five states (`available`, `disabled`, `needs-configuration`, `untrusted`,
  `error`); a configured tool whose server was never started is reported as available with
  `detail: "tool list not yet discovered"` instead of a sixth `unknown` state, because "unknown" told a
  host nothing it could act on.

## Known limitations

- **First-run cost.** A run that may use MCP starts, once, every enabled server whose catalog was never
  recorded, concurrently under a 10 s budget. After that the catalog is cached and the run start costs
  nothing. A server that fails is skipped for 2 minutes by the automatic discovery (an explicit call
  still retries).
- **Cached catalogs can be stale.** A server whose upstream tool set changed is described by the last
  recorded catalog until it is started again (`MCPRefreshTools` or a call).
- **`include` lists for unstarted servers.** A server that was never started contributes rows named
  from its `include` list, with `availability.detail: "tool list not yet discovered"` and no schema.
- **Computer Use depends on CodeBridge.** The template hard-codes the macOS Application Support socket
  path; a custom `-mcp-socket` needs the `url` edited. Core does not implement screen capture or
  accessibility control, and CodeBridge's approval gate is not bypassed.
- **Browser** needs `playwright-mcp` on `PATH`; Core never installs it. `--isolated` keeps the profile
  in memory, but a crash of the server can leave a Chromium process behind.
- **CodeGraph is experimental** and not verified live; its upstream server indexes on the first query
  and installs a missing engine binary on first run (the template only disables its update check).
- **LSP tool cost** is the spec size only (2,204 bytes for 7 tools); no language server was started, so
  `ToolSchemaCost` reports `lspBytes: 0` until one is attached.
- **Exa and GitHub were not handshaked** (credentials/cost); their tool names come from the official
  documentation, not from a listing Core observed.
- A local MCP server runs with the user's authority: process separation and explicit configuration, not
  an OS sandbox. `pluginapi` remains trusted in-process code and is not claimed to be a sandbox.
- `TestDBCommandDispatchesBeforeOpen` in `cmd/lcx-go` runs `go build` against the module proxy; it was
  not exercised separately here (the package's tests passed on this machine).
