# LSP lifecycle hardening — Core (2026-11-01)

Status: **implemented and validated locally** (Core only; Desktop consumes `Engine.LSPStatus` in its own
slice). Branch `rt/lsp`, base `ba0f7a5`. Owner decision (2026-11-01): LSP stays in Core, no protocol
rewrite, no bundled servers, no plugin conversion.

## What already held before this slice

Evidence is from `ba0f7a5`; only the gaps were changed.

| # | Owner requirement | At base | Verdict |
|---|---|---|---|
| 1 | Servers start on demand | `internal/lsp/manager.go:27` `Start` is explicit; the Engine started a manager only from `LSPStart` (`runtime/lsp.go:16`) and the toolset got the manager only when `Status().Running` was already true (`runtime/agent.go:247`) — so a run never started a server | **gap**: no on-demand start from a tool call |
| 2 | Reuse a server per workspace (+language) | `Manager` held **one** client per workspace (`manager.go:13-17`), so a second language replaced the first (`Start` closed `previous`); the Engine kept one manager per workspace (`runtime/lsp.go:141`) | **gap**: no per-language reuse |
| 3 | Reclaim idle servers | nothing | **gap** |
| 4 | Limit concurrent servers | nothing | **gap** |
| 5 | Fall back to text search when unavailable | `internal/toolset/lsp.go:129` `rawLSPResult` returned a bare Go error; the loop turned it into `{"error":…}` with no guidance | **gap** |
| 6 | Offer LSP tools only when a server is really available | registered whenever `s.lsp != nil` (`toolset.go:236`), which never happened in a run, and had no binary/marker check | **gap** |
| 7 | Lifecycle events into runtime logging | no logger anywhere in `internal/lsp` or the Engine; only `lsp.started`/`lsp.stopped` events from `LSPStart`/`LSPStop` | **gap** |
| 8 | Requests honour cancellation and timeouts | `client.go:184` sends `$/cancelRequest` on ctx done, and `Request` selects on `ctx.Done()` — **held**; no per-request timeout and no `protocol.ToolDeadline` | **partial** |
| 9 | LSP writes obey permissions and the protected-path guard | `lsp_rename` was `Permission: "read"`, `MutatesWorkspace` false, and described as a preview (`toolset/lsp.go:56-68`); it returned the raw edit and wrote nothing, so it neither bypassed the guard nor applied anything; ADR-0016 "Not provided" listed it as an unguarded file writer | **gap**: the write path the owner requires did not exist |

Also already held: workspace confinement of the document path (`features.go:resolvePath`, symlink and
`..` checks), message/document size bounds, pending-request bound (`MaxPendingRequests`), bounded stderr.

## Changes

### `internal/lsp` (manager rewritten around per-language servers)

- `Manager` (`manager.go`) keeps one `server` per language: `byLanguage`, `order`, and per server
  `client`, `state`, `lastError`, `startedAt`, `lastUsedAt`, `busy`, `starting`, `failures`,
  `nextAttempt`. `startMu` per server serializes starts, so concurrent calls share one process.
- **On demand**: `call()` resolves the language from the file extension, `begin()`s the server, then
  `client()` starts it under `startTimeout` (spawn + `initialize`). Nothing is started at construction.
- **Reuse**: a running client is returned as is; `TestManagerConcurrentRequestsShareOneServer` shows 8
  concurrent calls produce one process.
- **Idle reclaim**: ONE manager-level `time.Ticker` per manager (`tickLoop` → `reclaimIdle`), period
  `idleTimeout/4` clamped to 25 ms…30 s, started only while a server runs and stopped when the last one
  ends. No per-request timer. A busy server is never reclaimed.
- **Limit**: `maxServers`; `reserveLocked` detaches the least recently used **idle** server when full;
  when every server is busy the call returns `ErrServerLimit` (never queued). In-flight starts count.
- **Crash**: the client's `Done()` is watched; a server that ends on its own becomes `unavailable` with
  `lastError`, is reaped (`client.Close()` → `cmd.Wait`), and its next need waits a doubling backoff
  (500 ms…30 s). A deliberate stop (idle, evict, `Stop`, `Close`) is not reported as a crash because
  `srv.client` is cleared first.
- **Close**: `Stop()` clears every client and closes it (shutdown → exit → kill through the command
  context); `Close()` also waits for the watchers. `Client.Close` now ignores the exit status and the
  kill's `context.Canceled` (a stopped server is not a failure) and sets `cmd.WaitDelay`.
- **Cancellation/timeouts**: `requestContext` applies `RequestTimeout` and, when present,
  `protocol.ToolDeadline(ctx)`; the existing `$/cancelRequest` path in `Client.request` is unchanged.
- `detect.go`: built-in catalog (`go:gopls`, `typescript:typescript-language-server`,
  `python:pyright-langserver`, `rust:rust-analyzer`), `resolveCommand` (PATH or absolute), and
  `detectLanguages` — one bounded walk (4000 entries, depth 6, hidden/`node_modules`/`target` skipped)
  answering every language at once, cached per manager. `Available()` / `AvailableLanguages()` start
  nothing.
- `Servers()` reports `ServerStatus{language,name,command,root,state,pid,startedAt,lastUsedAt,lastError}`
  without starting anything.
- `edit.go`: `WorkspaceEdit.FileEdits()` (refuses resource operations and non-`file:` URIs),
  `ApplyTextEdits` (UTF-16 positions, refuses overlaps and out-of-range positions).
- `Client`: configurable initialize timeout (`Config.StartTimeout`), `Done()`, diagnostics wait on a
  wake channel instead of a 20 ms ticker.

### `internal/toolset`

- `registerLSPTools` returns early unless `s.lsp.Available()` (binary on PATH + file or marker of the
  language). `lsp_rename` is `Permission: "write"`, `MutatesWorkspace: true`.
- Failures become results: `{"error","code","hint"}` with `code` in
  `unavailable|timeout|busy|cancelled|failed` and a hint naming `search_text`/`find_files`.
- `lspedit.go` `applyWorkspaceEdit`: every path through `s.resolve` (workspace confinement, protected
  trees, symlink escape), `readChecked` for the content, one plan per file, duplicate real paths
  refused, then `applyPlans` (the atomic batch `apply_patch` now uses; rollback on failure). Nothing is
  written before every path is validated.
- `patch.go`: the write loop is `applyPlans`, shared by `apply_patch` and `lsp_rename`.
- `Options.Logger` / `Set.log` (nil → discard).

### `internal/agent`

- `executionTierOf("lsp_rename")` = `tierWorkspace`: the paths a rename touches are chosen by the server,
  so the whole work tree is snapshotted, as for `shell`. Frozen table in `execution_test.go` updated.

### `config` + schema

- `lsp` object, global file only (like `concurrency`/`execution`, with the `global_only` warning):
  `idleTimeoutSeconds` (600, 1–86400), `maxServers` (3, 1–8), `startTimeoutSeconds` (20, 1–300),
  `requestTimeoutSeconds` (20, 1–300), `servers.<language>{command,args,extensions,markers}`.
  `LSP.Limits()` applies the defaults; `docs/lumencortex.schema.json` carries the object.

### `runtime`

- `Engine.log *slog.Logger` (`Options.Logger`, nil → discard) with `engineLogger`.
- `lspManagerFor(workspace)` creates a manager (no process) and `Configure`s it from the global file at
  every run start / status call; `agent.go` and `prompt.go` use it instead of requiring a running server.
- `LSPStatus(ctx, sessionID) ([]LSPServerStatus, error)` — camelCase JSON, starts nothing.
  `LSPStart`/`LSPStop` reworked; `stopLSPWorkspace` still called on worktree deletion.
- Lifecycle events published as `lsp.started|ready|idle_stop|crashed|evicted|unavailable|stopped`.

### Docs

`docs/adr/0016-*` (D5 lists `lsp_rename`; "Not provided" no longer exempts it), `docs/agent-runtime.md`
(LSP section rewritten: catalog, config keys, lifecycle, tools, fallback, `LSPStatus`),
`docs/security-and-trust.md`, `docs/go-provider.md` (consumed keys).

## Tests

Real language server: **no `gopls` on this machine** (`which gopls` → not found), so the tests use
`internal/lsp/testdata/fakeserver`, a small Go JSON-RPC/stdio server built once per test binary
(`TestMain` in `internal/lsp`, `internal/toolset` and lazily in `runtime`).

| Command | Result |
|---|---|
| `go test -count=1 -race ./internal/lsp/` | ok (7.0 s) — start-on-first-request, reuse (same pid, one start line), idle reclaim, LRU eviction, busy-server refusal, crash→unavailable→restart after backoff, `Close` leaves no pid, request timeout, ctx cancellation + `$/cancelRequest` observed, tool-deadline bound, start timeout, availability (missing binary / no file / marker / default catalog), 8 concurrent calls → 1 process, UTF-16 edits |
| `go test -count=1 -race ./internal/toolset/` | ok (11.2 s) — tools absent without a binary or without a file of the language; failure result advises `search_text`; rename applies two files with `MutatesWorkspace`; protected path refuses the whole rename; symlink escape refuses the whole rename; read-only policy denies and workspace policy allows; resource operation and missing target refused |
| `go test -count=1 ./config/` | ok — `TestLSPIsReadFromTheGlobalFileOnly` (defaults, global-only warning, 11 invalid documents) |
| `go test -count=1 ./internal/agent/` | ok — schema and tier tables |
| `go test -count=1 ./runtime/` | ok (206 s with `-race`) |
| `go build ./... && go vet ./internal/lsp ./internal/toolset ./runtime ./config ./internal/agent` | ok |
| `gofmt -l` on the touched files | clean (`internal/lsp/features.go` and `client_test.go` are unformatted at `ba0f7a5` and were left as they were) |

## Measured cost (macOS arm64, Go 1.26.5, testdata server)

| Metric | Value |
|---|---|
| server processes with 0 servers | 0 |
| harness RSS, 0 servers | 5 056 KB |
| harness RSS, 1 server | 6 592 KB (+1 536 KB) |
| server RSS (fake server) | 4 880 KB |
| first `lsp_hover` (spawn + initialize + request) | 183 ms (test: 159–299 ms) |
| second `lsp_hover` (reused server) | 301 µs, same pid |
| idle reclaim (timeout 300 ms) | 343 ms after the last use; test with 150 ms timeout: 156 ms |
| `Close` with no running server | 4 µs; with servers: bounded by `cmd.WaitDelay` (2 s) + shutdown (1 s) |

The manager's steady-state cost is one goroutine per running server (the client read loop + its
watcher), one ticker while any server runs, and one 25 ms…30 s ticker for the whole manager.

## Known limitations

- `gopls` was not installed, so no test ran against a production language server; the fake server speaks
  the same framing and the same request/notification subset.
- `lsp_rename` applies text edits only. A workspace edit that carries resource operations
  (`create`/`rename`/`delete`) is refused whole rather than applied.
- Rename edits are not validated against the language server's own `documentChanges` version field: the
  file content on disk is the base, as for `apply_patch`.
- The document is re-read from disk on every call (`syncDocument`), so an editor buffer that is not
  saved is not what the server sees.
- `search_text`, `find_files`, the git tools and the read-only `lsp_*` tools remain outside the
  protected-path guard (ADR-0016 "Not provided", unchanged by this slice).
