# DAP Program Debugger

Program debugging is separate from Agent safe-boundary stepping. The original P0 experiment is retained unchanged. Production transport is `internal/debugger/dap`.

## Transport and lifetime

Strict CRLF Content-Length framing bounds headers to 8 KiB and messages to 4 MiB. At most 64 requests are outstanding. Response command and sequence must match their request. Events use a bounded queue: output may be dropped with a counter, critical-event overflow closes the connection rather than silently lose state. Reverse terminal/target requests are refused, never executed. Cancelled pipe writes are interrupted by deadlines. Adapter stderr retains bounded head/tail, not unbounded output.

The transport uses the existing `shell.StartManaged` admission and crash-guard lifetime. It reaps on adapter exit, connection failure, owner cancellation and close. Linux cgroup containment is required by the manager; process-group-only environments must not claim complete detached-descendant cleanup.

## Sandbox

`evalruntime.ProgramCommand` reuses the Eval launcher without creating a new kernel or RPC. Host configuration names executable, installation read paths, workspace, private directory and exact environment. Defaults do not inherit host credentials. Linux program mode requires verified caller isolation; unavailable isolation fails closed.

On macOS, program mode alone adds `mach-priv-task-port` and `system-debug` restricted to `target same-sandbox`. Ordinary Eval workers do not receive these permissions. The kernel originally denied both operations against LLDB's own fixture; narrow grants were driven by recorded denial logs, not a global sandbox exemption. Outside workspace writes and network remain denied. Foreign-sandbox attach is not implied by these grants. Every program profile (adapter, debuggee, command hook) may signal only processes of its own sandbox.

## Stops and session end

Every reference the Manager hands out carries the stop epoch that minted it: stack frames, including the top frames of a continue/step stop report, scopes and variables. After the next stop the reference is refused as stale, never forwarded to an adapter that may answer an unknown reference with an empty list.

An empty `exception_filters` list reaches the adapter and clears its exception breakpoints.

`debug.session.lost` means the adapter exited on its own. A stop the host chose (disconnect, revocation, run end, Manager close, lifetime expiry) ends as `debug.session.stopped`.

## Current verified behavior

`raw/dap-transport-race.json`: out-of-order request correlation, interleaved events, malformed/oversized/truncated frames and cancellation.

`raw/dap-five-adapter-integrated.json`: real sandboxed LLDB, Delve, debugpy and Node; real IntelliJ MCP Java session; initialized `value=42`, stack and owned stop. Engine tests cover separate Attach authorization, pinned prepared same-sandbox target, revocation and durable lifecycle. `raw/dap-real-ui.json` covers Desktop backend → Core → LLDB, including step into/out. Native Wails build/self-check passed; native interactive and Linux adapter acceptance are not claimed.

## IntelliJ MCP

Configure an adapter with `id`, `language` and `mcpServer`, not a command or DAP transport. The server must be a global MCP entry with `hostOnly: true`, an explicit endpoint, and `IJ_MCP_SERVER_PROJECT_PATH` pinned to the workspace. Core reuses the existing MCP registry; `hostOnly` excludes raw tools from Agent catalogs and rejects session-bound raw calls. It does not grant arbitrary IDE control to models.

The endpoint must be dedicated and empty before launch. Core permits one active session per configured server, retains IDE IDs internally, polls bounded status, and removes its breakpoints on stop. IDE sessions run under the external IDE's host authority, not the DAP process sandbox. Lost launch responses are reconciled without another launch. Unprovable cleanup retains `cleanup_failed` and prevents endpoint reuse in the manager. No IDE Attach or arbitrary target selection is exposed.

The existing control epoch, workspace path checks and evaluation grants apply. Read-only watches read visible frame values; arbitrary evaluation requires a separate grant. IntelliJ hit conditions are rejected when its toolset cannot represent them.

## Host configuration

Desktop reads only the absolute host `LCX_DEBUG_CONFIG` path. Oversized input, unknown fields, concatenated JSON and relative paths are rejected. Adapter commands/environment are not model or UI inputs. `raw/dap-host-config-regression.json` and `raw/dap-mcp-regression.json` record the targeted configuration and MCP checks.

Final transport evidence: `raw/dap-real-adapter-crash-matrix.*` kills the actual
LLDB/Delve/debugpy/Node adapter and observes process reaping, closed transport and
rejected subsequent requests. `raw/dap-stderr-drain.*` proves a 2 MiB stderr stream
drains without stalling and retains no more than 8 KiB. The four-language fault
experiment observes initialized input=41/value=40 and independently executes the
repaired program to PASS; it is not an LLM accuracy benchmark.

Effective Prompt and capability/schema-cost reports bind the same worker messaging
and authorized DAP tools as execution. Inspecting a catalog does not launch an
adapter, send a message or acquire a debug grant.
