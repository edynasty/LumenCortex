# Browser lifecycle re-run on Core 72d613f (2026-11-02)

Same driver (`../driver/main.go`, replace => wt/core-rt at 72d613f; MCP/LSP/toolset code is unchanged
between ba99d5c and the final SHA), same template, @playwright/mcp 0.0.82, Google Chrome 156.0.8078.74.
`./lcxmcp -mode idle` ran beside `close`, `restart`, `cancel`, `crash`, `policy` (each its own Engine,
workspace and port).

- idle: Core stopped the server with reason=idle 600.0 s after last use; afterwards 0 browser processes.
- close: `after-close` shows 1 orphan, pid 92414 = chrome_crashpad_handler of the concurrently running
  idle mode's Chrome (started 08:44:29 with that browser's helpers, listed in idle's own `after-run`
  orphans, and gone once idle's browser stopped). The close mode's own tree: pidAlive=false, RSS 0.
- crash (external SIGKILL of the server): 6 Chromium processes survived 30 s (known MCP-001); the
  next call restarted the server with a new pid. The survivors were killed by hand afterwards.
- restart (disable -> enable), cancel, policy: no process left after Engine.Close.
