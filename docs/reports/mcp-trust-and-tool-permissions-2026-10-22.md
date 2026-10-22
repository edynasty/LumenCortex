# MCP project trust gate and per-tool permissions — 2026-10-22

- **Status:** implemented and validated locally; not pushed.
- **Baseline:** Core `1ac5932` (`v0.1.0`), branch `p0/mcp`.
- **Scope:** P0-B (MCP half of SECURITY-001) and P0-E1 item 2 (MCP per-tool permission override).
- **Related:** ADR-0013 (written by the TRUST slice), plan
  `../LumenCortex-architecture-audit/design/handoffs/phase0-and-workbench-plan-2026-10-22.md` §2.3, §2.6.

## Delivered

| Capability | Where |
| --- | --- |
| Project trust check on the registry, installed by the Engine (`config.Load(engine workspace)` → `Effective().Trusted`, load error = untrusted), evaluated per start, per tool listing, per status listing and per call | `internal/mcp/registry.go`, `runtime/engine.go` |
| Refusal with `mcp.ErrUntrustedProject` before any process is created, exported as `runtime.ErrUntrustedWorkspace` | `internal/mcp/types.go`, `runtime/mcp.go` |
| Trust-aware effective listing and scope: `scope` (`global`/`project`) and `blockedByTrust` on configs and statuses; running project clients stop contributing tools when trust is withdrawn, without being killed | `internal/mcp/registry.go`, `internal/mcp/types.go` |
| Per-tool permission override (`tools: {"<name>": {"permission": "read"\|"write"\|"exec"}}`), validated on load and upsert, honoured only from the global file or a trusted project | `internal/mcp/types.go`, `internal/mcp/registry.go` |
| Toolset maps the effective permission instead of re-deriving read-only from the annotation | `internal/toolset/mcp.go` |
| Rules documented in the MCP section (trust gate, precedence, override, effective permission) | `docs/agent-runtime.md` |

## Trust rule (for ADR-0013)

1. The check belongs to the Engine workspace, never to a session worktree: the runtime workspace
   only decides the process working directory.
2. While the project is untrusted its file is **ignored entirely**: resolution and the effective
   listing behave as if it did not exist. A project-only id cannot start (`ErrUntrustedProject`)
   and is listed with `scope: "project"`, `blockedByTrust: true`. An id the project shadows resolves
   to the global entry, and that global entry is what the listing shows — the substitution is
   visible, never silent.
3. Trust withdrawn after a project server started: the process keeps running, its tools disappear
   from `AgentTools`/`MCPTools`, `CallTool`/`RefreshTools` return `ErrUntrustedProject`, and
   `Statuses` reports `running: true` with `blockedByTrust: true`.
4. A registry constructed without a check treats its project file as untrusted (fail closed); the
   Engine always installs one.
5. Overrides follow the same ownership rule: a project file cannot declare a tool read-only that
   its server does not annotate; a global entry can.

## Public shapes (additive)

Go (`internal/mcp`, re-exported by `runtime` as aliases):

```go
type Config struct {
    ID, Name, Command string
    Args []string
    Workspace string
    ProtocolMode ProtocolMode
    Disabled bool
    Tools map[string]ToolConfig `json:"tools,omitempty"`            // new
    Scope string `json:"scope,omitempty"`                            // new, listing only
    BlockedByTrust bool `json:"blockedByTrust,omitempty"`            // new, listing only
}
type ToolConfig struct { Permission string `json:"permission,omitempty"` } // new

type Status struct {
    // existing fields
    Scope string `json:"scope,omitempty"`                            // new
    BlockedByTrust bool `json:"blockedByTrust,omitempty"`            // new
}

type AgentTool struct {
    Name, Description string
    InputSchema map[string]any
    ServerID, ToolName string
    ReadOnly bool
    Permission string `json:"permission"`                            // new, always set
    PermissionSource string `json:"permissionSource,omitempty"`      // new: annotation|override
    Scope string `json:"scope,omitempty"`                            // new: global|project
}
```

JSON example of a listing entry:

```json
{"id":"dbx","name":"DBX","command":"dbx-mcp","protocolMode":"legacy",
 "scope":"project","blockedByTrust":true,
 "tools":{"query":{"permission":"read"}}}
```

`scope` and `blockedByTrust` are never written back to `mcp.json` (upsert clears them, save strips
them).

## Tests

- `go build ./... && go vet ./...` — pass.
- `go test -count=1 ./...` — pass (all packages).
- `go test -count=1 -race ./internal/mcp ./internal/toolset` — pass.

New coverage:

- `internal/mcp/registry_test.go`: untrusted project server refused with no process spawned (a
  `/bin/sh` command that would touch a marker file), listing reports `scope`/`blockedByTrust`;
  global server starts while the project is untrusted; untrusted shadow resolves to the global entry
  and switches to the project entry when trust is granted; tools of a running project server vanish
  when trust is withdrawn and return when it is restored (`CallTool` refused meanwhile); the trust
  check is independent of the runtime workspace; permission precedence table (annotation vs global
  override vs trusted project override vs untrusted project override ignored); override reaches
  `AgentTools` with `permissionSource: "override"`; invalid and empty permission rejected on upsert
  and on load, naming `tools["echo"].permission`; listing fields are not persisted.
- `internal/toolset/mcp_test.go`: a stdio test server with two unannotated tools; a read-only run
  sees the one overridden to `read`, does not see the other, is denied the exec tool and can call
  the overridden one.
- `runtime/mcp_trust_test.go`: with the Engine open, `MCPStart` on an untrusted project entry
  returns `ErrUntrustedWorkspace`, the command is not spawned, the listing is blocked and there are
  no statuses or tools; after the workspace is trusted, the same call spawns the command and the
  listing is unblocked.

## Deviations from the plan

- The plan's P0-E1 wording says an override "can only lower the permission to `read`"; the task
  allows `read`, `write` and `exec`. Implemented as specified by the task: the value is validated
  against all three, so a user may also tighten an annotated tool or grant `write` explicitly.
- The MCP documentation lives in `docs/agent-runtime.md` (the existing MCP doc); the
  `docs/security-and-trust.md` MCP section is owned by the TRUST slice. The section also dropped a
  false claim of HTTP transport and `transport`/`url` keys: the runtime only spawns stdio servers
  and `mcp.Config` has no URL field.

## Risks and out-of-scope findings

- A malformed project `mcp.json` still fails `Engine.Open` even when the workspace is untrusted
  (pre-existing: the file is parsed at construction). It is a workspace-open denial, not a
  privilege path; making the project file load lazy is deferred.
- Trust is re-read from disk on every start/tool listing (one `config.Load` per call); the plan
  requires per-call evaluation and the cost is a small file read.
- Desktop must add `tools` to its `MCPConfig` type to preserve overrides when saving a server; the
  Core JSON is additive and compatible without it.
