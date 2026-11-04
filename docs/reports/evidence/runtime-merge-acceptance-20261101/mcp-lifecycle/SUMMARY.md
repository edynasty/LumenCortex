# Browser (Playwright MCP) lifecycle through Core wt/core-rt (rt/execution-runtime)

Driver: /tmp/lcx-mcp/driver/main.go (module /tmp/lcx-mcp/driver, replace => wt/core-rt),
binary /tmp/lcx-mcp/lcxmcp; pre-fix binary /tmp/lcx-mcp/lcxmcp-prefix built from HEAD~1
(archived source at /tmp/lcx-prefix, `git archive HEAD~1`).
PATH shim: /tmp/lcx-mcp/bin/playwright-mcp -> `exec node ~/.npm/_npx/9833c18b2d85bc59/node_modules/@playwright/mcp/cli.js "$@"`
(package @playwright/mcp 0.0.82, node v24.20.0). Template applied byte-identically through
Engine.ApplyMCPTemplate(ctx, "global", "browser") + SetMCPServerEnabled(ctx, "global", "browser", true).
Chromium: the template's `--browser chrome` (Google Chrome 156.0.8078.74), headless, isolated.

Run: `cd /tmp/lcx-mcp && ./lcxmcp -mode {idle|restart|crash|cancel|close|policy}`
Each mode uses its own workspace, XDG dirs, Engine and mock-provider port; evidence in
evidence/<mode>.jsonl (one JSON object per mark) and evidence/<mode>.log.

Core commit under test: 15f9039 (fix) on rt/execution-runtime; idle/restart/crash/cancel/close/policy
were re-run after that commit. evidence/prefix-close.jsonl is the same close check on HEAD~1.

# Browser (Playwright MCP) lifecycle — timeline tables

## prefix-close

| t | mark | detail |
|---|---|---|
| 1.7s | sample:before-close | browserProcsAlive=9 orphanCount=1 pidAlive=True totalRSSKB=1136272 |
| 1.7s | engine.close.begin |  |
| 1.7s | engine.close.done |  |
| 3.8s | after-close | pidAlive=False |
| 3.8s | sample:after-close | browserProcsAlive=2 orphanCount=2 pidAlive=False totalRSSKB=0 |

## idle

| t | mark | detail |
|---|---|---|
| 1.0s | run.end | lastTag=final seconds=0.9 status=completed toolsOffered=41 |
| 1.0s | server.pid |  |
| 1.1s | sample:after-run | browserProcsAlive=9 orphanCount=1 pidAlive=True totalRSSKB=1153616 |
| 301.1s | sample:t+300s | browserProcsAlive=1 orphanCount=0 pidAlive=True totalRSSKB=87104 |
| 601.1s | mcp.stopped | afterLastUseSeconds=600.0 at=2026-11-01T09:02:15.859606Z reason=idle |
| 603.1s | sample:after-idle-stop | browserProcsAlive=0 orphanCount=0 pidAlive=False totalRSSKB=0 |
| 603.2s | orphans.after-idle | pidAlive=False |
| 603.2s | engine.close.begin |  |
| 605.3s | orphans.after-close | pidAlive=False |

## restart

| t | mark | detail |
|---|---|---|
| 0.3s | mcp.start#1 | tools=25 |
| 1.2s | sample:running#1 | browserProcsAlive=9 orphanCount=1 pidAlive=True totalRSSKB=1154784 |
| 3.3s | after-disable | oldPidAlive=False statuses=0 treeBefore=7 |
| 3.5s | mcp.start#2 | newPid=True oldPid=30949 tools=25 |
| 4.2s | sample:running#2 | browserProcsAlive=9 orphanCount=1 pidAlive=True totalRSSKB=1147968 |
| 4.2s | engine.close.begin |  |
| 6.3s | orphans.after-close | pid1Alive=False pid2Alive=False |

## crash

| t | mark | detail |
|---|---|---|
| 1.2s | run.end | lastTag=final seconds=1.1 status=completed toolsOffered=41 |
| 1.3s | kill | signal=SIGKILL treeBefore=['32001(rss=236048KB)', '32014(rss=148736KB)', '32015(rss=101840KB)', '32017(rss=81968KB)', '32024(rss=190800KB)', '32027(rss=102704KB)', '32028(rss=142912KB)'] |
| 6.4s | survivors@5s | survivorCount=5 survivorRSSKB=578624 |
| 31.6s | survivors@30s | survivorCount=5 survivorRSSKB=577264 |
| 31.7s | run.end | lastTag=final seconds=0.0 status=completed toolsOffered=41 |
| 31.7s | after-failed-call | pidAlive=False statuses=0 |
| 32.6s | run.end | lastTag=final seconds=0.9 status=completed toolsOffered=41 |
| 32.6s | restarted | isNew=True newPid=32278 oldPid=31954 |
| 32.7s | survivors@restart | survivorCount=6 survivorRSSKB=517552 |
| 34.9s | orphans.after-close | pid2Alive=False |

## cancel

| t | mark | detail |
|---|---|---|
| 1.0s | sample:before-cancel | browserProcsAlive=9 orphanCount=1 pidAlive=True totalRSSKB=1158752 |
| 1.2s | cancel.inflight | callsInFlight=True |
| 1.3s | run.cancelled | status=interrupted |
| 4.3s | after-cancel | pidAlive=True |
| 6.5s | after-close | pidAlive=False |
| 6.6s | sample:after-close | browserProcsAlive=0 orphanCount=0 pidAlive=False totalRSSKB=0 |

## close

| t | mark | detail |
|---|---|---|
| 1.0s | sample:before-close | browserProcsAlive=9 orphanCount=1 pidAlive=True totalRSSKB=1154624 |
| 1.0s | engine.close.begin |  |
| 1.1s | engine.close.done |  |
| 3.2s | sample:after-close | browserProcsAlive=0 orphanCount=0 pidAlive=False totalRSSKB=0 |

## policy

| t | mark | detail |
|---|---|---|
| 1.0s | run.end | lastTag=final seconds=0.9 status=completed toolsOffered=41 |
| 1.0s | readonly.run.end | status=completed toolsOffered=23 |
| 1.0s | capabilities:disabled | count=5 |
| 1.0s | capabilities:enabled | count=5 |
| 1.0s | computer-use.mcptools | count=15 |
| 3.2s | orphans.after-close |  |


## Commands (Core)

    gofmt -l internal/mcp internal/shell                       # empty
    go vet ./internal/mcp/... ./internal/shell/...             # ok
    GOOS=windows go vet ./internal/mcp/... ./internal/shell/... # ok
    go test -race -count=1 ./internal/mcp/...                  # ok 18.4s (includes the two new tests)
    go test -race -count=1 -run 'TestRuntimeMCP|TestMCP|TestCapability|TestSetCapability|TestToolSchemaCost' ./runtime/  # ok 18.5s
    go test -race -count=1 ./internal/shell/...                # ok 4.7s
    go build ./...                                             # ok

## Notes

- evidence/pre/ holds an earlier run of each mode against the same code with pre-final comments; the
  files quoted above (evidence/<mode>.jsonl) are the post-commit runs.
- The orphan scan counts processes reparented to pid 1 whose command line is a browser or the MCP
  server. During normal operation it reports exactly one: chrome_crashpad_handler, which Chrome starts
  detached (own pgid) and which dies with the browser (orphanCount=0 once the browser is gone).
- Leftovers from the crash check and the pre-fix close check were killed by hand after measurement; the
  machine has no playwright-mcp or Chromium process from these runs left.
