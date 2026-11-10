# Harness Evolution — DAP slice (2026-11-10/11)

Status: **implemented and validated on macOS with real lldb-dap and Delve**; Linux real-adapter
validation is **blocked by the production caller-isolation gate** (exact reason below), and
debugpy/js-debug are **not installed on this host** (not claimed).

Branch: `feat/harness-evo-dap` in worktree `wt/core-harness-evo-dap`, based on Core `696eb74`.
Commits:

```text
15cb691 fix(debugger): wait for stops, reject stale references, bound results
6cb4840 test(debugger): deterministic E1-D1..D5 and control-path regression tests
4c63752 feat(debugger): built-in adapter discovery and Linux-correct lldb resolution
6c54dd8 test(runtime): E1 Agent loop against real lldb-dap and Delve
77d3cf7 test(runtime): Delve Agent loop, panic exception stop and real lldb exception/crash
55f7961 test(debugger): gate every DAP test on the Linux caller-isolation contract
```

Evidence directory (workspace-level, **not version-controlled by Core**):
`bench/evidence/agent-harness-implementation-20261110/raw/dap/`. The primary design and
experiment evidence lives in `bench/evidence/agent-harness-capability-roadmap-20261110/`
(`DAP_AND_PERSISTENT_EVAL_DESIGN.md` §1.1, §2.2; `raw/e1-dap-agent-loop.json`;
`prototypes/e1-dap-agent-loop/main.go`).

## 1. Scope

Implement the five Agent-usage defects from E1, the DAP control/observation gaps in design §2.2,
and the adapter/crash/lifecycle additions the acceptance list requires:

- E1-D1 one-call stop reporting for continue/step, including the waiter-before-request rule;
- E1-D2 stop-epoch validation of frame and variable references (`ErrStaleReference`);
- E1-D3 one truncation policy in the Manager shared by Desktop and Agent;
- E1-D4 a distinct `ErrEvaluateNotGranted`;
- E1-D5 `debug.module` out of durable events;
- exception breakpoints and `exceptionInfo`, adapter-verified breakpoint state;
- graceful `disconnect{terminateDebuggee}` with a reported cleanup method and kill fallback;
- adapter crash/abnormal disconnect publishes `debug.session.lost` and reaps the process;
- built-in adapter discovery for lldb, delve, debugpy and js-debug;
- a real lldb-dap Agent-loop regression test converted from the E1 prototype, plus Delve.

Out of scope and untouched: the shared Managed Process Runtime (`internal/procrt`, design §4), the
Eval kernel cross-debug attach, Desktop UI, and the persistence/migration of debug sessions
(design §2.5). No migration was added.

## 2. Findings re-derived from primary evidence

`raw/e1-dap-agent-loop.json` shows 116 durable debug events of which 85 are `debug.module`;
`stop_polls_extra_model_turns: 1`; `locals_after_step_same_ref: {"variables":[]}`;
`evaluate_expression_without_grant` returns "program debugging is disabled or not authorized";
`state_returned_by_step` is `state=running`. The prototype confirms each was an Agent-visible
failure, not a presentation detail. The five defects were each reproduced against the pre-fix
tree with a deterministic fake adapter before the fix (`raw/dap/before-fix-probe-debugger.txt`,
`raw/dap/before-fix-probe-runtime.txt`):

| ID | Pre-fix observation (deterministic probe) | Fix | Post-fix evidence |
|---|---|---|---|
| E1-D1 | `debug_continue{action:configure}` returned the snapshot (`state=configuring`), no stop or frames | `ControlAndWait` registers the waiter before sending the request; returns `reason`, `threadId`, `stopEpoch`, ≤5 bounded frames, exception info; `wait=false` → `reason=pending`; timeout → `reason=timeout`; capped at 30 s and the tool deadline | `TestContinueWaitsForStopAndReturnsFrames`, `TestWaitTimeoutReturnsAsyncState`, `TestE1D1ContinueReturnsStopInOneCall`, real E1 loop |
| E1-D2 | after a stop, the old `variablesReference` resolved to `{"variables":[]}` with no error | references are stamped with the stop epoch that minted them; stopped/continued increments it; stale → `ErrStaleReference`; scopes/variables/stack/evaluate require a stopped target | `TestStepInvalidatesReferences`, `TestE1D2OldReferenceIsRejected`, real lldb/delve loops |
| E1-D3 | a 200 KiB adapter value reached the caller whole (205 016 bytes) | `Manager.Inspect`/`Evaluate` bound every value (2 KiB) and the whole result (32 KiB) and set `truncated`; Desktop reads the same bounded JSON | `TestInspectAndEvaluateAreTruncated`, `TestE1D3InspectionIsBounded` |
| E1-D4 | ungranted evaluation returned `ErrDebugDisabled` / "program debugging is disabled or not authorized" | `ErrEvaluateNotGranted` at Manager and Engine; the session keeps working | `TestEvaluateGrantIsDistinctFromDebuggingDisabled`, `TestE1D4EvaluateGrantIsDistinct` |
| E1-D5 | 25 `debug.module` events entered durable history | `debug.module` is excluded from durable events and stays on the live bus | `TestE1D5ModuleEventsStayLive` (25 live / 0 durable), real E1 loop assertion |

## 3. Changes

### Core debugger (`internal/debugger`)

- `control.go` (new): `WaitOptions`, `StopInfo`, `FrameSummary`, `ExceptionInfo`,
  `ErrStaleReference`, `ErrEvaluateNotGranted`, `ControlAndWait`, `Disconnect`,
  `SetExceptionBreakpoints`, reference epoch registry, and the single bounding function
  (`boundJSON`: 2 KiB per value, 32 KiB per result, rune-safe clipping, array-tail dropping,
  `truncated` flag).
- `manager.go`: stop tracking (`stopEpoch`, `stopReady` broadcast channel, `lastStop`), stop
  capture with top frames and `exceptionInfo`, terminal/lost transitions, `debug.session.lost`,
  adapter PID, exception filters from `initialize`, normalized adapter-verified breakpoint state,
  stopped-target checks and reference validation in `Inspect`/`Evaluate`, graceful stop through
  `Disconnect`.
- `presets.go` (new): presets for lldb (`xcrun --find lldb-dap`, else PATH `lldb-dap` /
  `lldb-dap-NN`), delve (`dlv`, `dlv dap --listen=unix:{socket}`), debugpy (real
  `import debugpy` probe and adapter path) and js-debug (only an actually found adapter);
  `AdapterStatus` fields `Available`, `Version`, `Reason`, `Preset` reported by `Adapters()`;
  `Discover` registers only probed-and-usable adapters and never installs anything.
- `dap/process.go`, `dap/socket.go`: report the adapter PID.
- `dapfake/` (new test-support package): deterministic DAP server used by both test packages;
  configuration is passed after `--` so neither the sandbox nor `TestMain` env rewriting matters.

### Runtime/tool surface

- `runtime/dap_debugger.go`: `DebugControlAndWait`, `DebugSetExceptionBreakpoints`,
  `DebugDisconnect`, `ErrEvaluateNotGranted`, `DebugOptions.Discover`; stop routes through the
  graceful path.
- `runtime/dap_tools.go`: `runDebugger` implements the three new controller methods and clamps
  the wait to the tool deadline.
- `runtime/events.go`: one-line exclusion of `debug.module` from durable events.
- `internal/toolset/debugger.go`: `debug_continue` / `debug_step` / `debug_pause` gain `wait`
  (default true) and `timeout_sec` (1..30) and return `StopInfo`; `debug_set_breakpoint` gains
  `exception_filters` and reports the adapter's supported filters; `debug_stop` gains
  `terminate_debuggee` (default true) and reports `cleanup`.

Public API changes are additive: new methods and fields; existing exported signatures and
semantics are unchanged. `Engine.DebugAdapters()` keeps its type; `Adapter` gained
`Available`/`Version`/`Reason`/`Preset`, and `Snapshot` gained `AdapterPID`/`StopEpoch`/
`StopReason`/`ExceptionFilters`. Tool results for continue/step/pause changed from a snapshot to a
`StopInfo` **by design** (E1-D1); `debug_stop` now returns the snapshot plus `cleanup`.

## 4. Tests run and results

Raw JSON/logs are under `bench/evidence/agent-harness-implementation-20261110/raw/dap/`.

| Run | Command (exact flags) | Result |
|---|---|---|
| Pre-fix probes | `go test -run 'TestProbeE1'` (debugger, runtime) | 5 failures, one per defect (evidence files above) |
| Deterministic debugger | `go test -count=1 -run 'TestE1\|TestStep\|TestInspectAndEvaluate\|TestContinueWaits\|TestWaitTimeout\|TestExceptionStop\|TestBreakpointsReport\|TestDisconnectIsGraceful\|TestAdapterCrash\|TestPreset\|TestManagerDiscovery' ./internal/debugger/` | pass |
| Deterministic runtime | `go test -count=1 -run 'TestE1\|TestDebugTools' ./runtime/` | pass |
| Debugger packages, full | `go test -count=1 ./internal/debugger/...` | pass (34 tests under `-race`) |
| Runtime DAP subset | `LC_TEST_LLDB=... LC_TEST_DELVE=... go test -count=1 ./runtime/ -run 'TestAgentLoopDebugs\|TestE1\|TestRealLLDB\|TestDelve\|TestDebugEngine\|TestDebugTools'` | pass: 12 tests, 0 failures (`runtime-dap-final.json`) |
| Whole debugger + runtime packages | `LC_TEST_LLDB=... LC_TEST_DELVE=... go test -count=1 ./internal/debugger/... ./runtime/` | pass: 608 tests, 0 failures, 21 skips (`full-dap-packages.json`) |
| Race, debugger + runtime DAP | `go test -race -count=1 ./internal/debugger/...` and `-race` runtime DAP subset | `race-debugger-final2.json` (34 pass, 0 fail), `race-dap-final.json` (34 pass, 0 fail), `race-dap-real.json` — 0 failures |
| Real lldb Agent loop | `LC_TEST_LLDB=... go test -run TestAgentLoopDebugsRealLLDB -v ./runtime/` | pass, 10.7 s, 22 tool turns |
| Real Delve Agent loop | `LC_TEST_DELVE=$HOME/go/bin/dlv go test -run TestAgentLoopDebugsRealDelve -v ./runtime/` | pass |
| Delve panic | `TestDelveExceptionStop` | pass: `reason=exception`, `exceptionId=panic` |
| lldb exception | `TestRealLLDBExceptionStopCarriesInfo` | pass: `reason=exception`, `exceptionId=signal`, `description="signal SIGABRT"` |
| lldb crash | `TestRealLLDBAdapterCrashIsLost` | pass: adapter reaped, session removed, exactly one durable `debug.session.lost` |
| Existing crash matrix | `LC_CAPABILITY_EXPERIMENT=1 LC_TEST_LLDB=... LC_TEST_DELVE=... go test -run TestRealAdapterCrashClosesTransport ./internal/debugger/dap/` | lldb + delve pass; debugpy/node skip (not configured) |
| Linux deterministic | container `lc-ubuntu-go` via `bench/.../integration/lc-linux-run.sh`, `./internal/debugger/ -run=TestE1` | 0 failures, 2 skips (caller-isolation gate) |
| Linux discovery | same container, `-run=TestFindLLDB` | `LLDB_DISCOVERY: /usr/bin/lldb-dap-18` |
| Linux real adapters | same container, `-run=TestReal` | 0 failures, 5 skips (see §6) |
| Build/vet | `go build ./...`, `go vet ./...` | pass |

The E1 Agent-loop test (scripted provider, **zero model requests**) covers: reproduce →
`debug_start` → adapter-verified breakpoints → one-call `debug_continue{configure}` with stop and
frames → `step_into` → `step_over` → scopes → variables → evaluate → ungranted evaluation refused
distinctly → second step makes the old reference stale and a fresh one works → `step_out` →
`continue{wait:false}` returns `pending` → `debug_pause` stops → `debug_stop` reports
`cleanup=disconnect` and `state=closed` → `replace_in_file` fix → rebuild and verify
`total=43 EXIT=0`. It asserts zero live sessions, no leftover adapter process, and zero
`debug.module` events in durable history.

## 5. Honest behaviour notes

- lldb-dap reports a DAP `pause` as the signal stop it is: `reason=exception` with the interrupted
  frames. The pause assertion accepts `pause` or `exception` and records which. This is adapter
  behaviour, not a Manager rewrite of the reason.
- lldb-dap has no generic "all exceptions" filter; it advertises C++/Objective-C/Swift
  catch/throw. A SIGABRT stop still arrives as `reason=exception` with
  `exceptionInfo{exceptionId:"signal"}`. Delve advertises `unrecovered-panic` and
  `runtime-fatal-throw`; a Go panic arrives with `exceptionId="panic"` and the runtime message.
- `debug.module` is **live-only**, not coalesced. A session that emits thousands of module events
  still puts them on the subscriber bus; durable history is unaffected.
- Adapter numeric references can be reused across stops. Staleness is detected from the epoch the
  Manager stamped when it issued the reference. If a caller re-reads the stack (minting the same
  number for the new stop) before using the old number, the old number is indistinguishable — the
  documented Agent flow (step → inspect) is covered.

## 6. Unverified / blocked

- **Linux real-adapter DAP sessions: Blocked by the production caller-isolation gate.** The
  Ubuntu 24.04 container has no
  `/proc/sys/kernel/apparmor_restrict_unprivileged_userns` (nor
  `..._unconfined`), so `evalruntime.checkCallerIsolation` cannot verify that the user-namespace
  grant behind bubblewrap is unborrowable. `ProgramCommand` therefore refuses to start any
  adapter. Tests skip with the exact reason and `LC_TEST_SANDBOX_BWRAP` names the required
  dedicated, group-restricted bwrap. This is not a test-configuration gap that can be worked
  around in this container; it needs a Linux host with AppArmor user-namespace restriction and a
  dedicated `lcbwrap` group (documented production setup).
- debugpy is **not installed** on this host; js-debug is **not installed**. Their presets report
  `available=false` with a reason, and no debugpy/js-debug session or test was run or claimed.
- Desktop rendering was not exercised (Core-only worktree); it consumes the same bounded JSON and
  the additive fields.
- Long-run soak/retention of debug events (10k/session) was not measured; D5 removes the flood
  source.

## 7. Risks / follow-ups

1. `ControlAndWait` holds the session operation slot while waiting (up to 30 s). Concurrent control
   calls queue. This is intentional (control is serialized) but a host that issues Desktop controls
   during an Agent wait will block until the stop. A future revision could separate wait from the
   command slot.
2. Stop capture issues `stackTrace`/`exceptionInfo` synchronously in the event loop (bounded 5 s).
   A dead adapter returns immediately; a slow adapter delays subsequent event processing.
3. References minted before this change (none persisted) are not a migration concern; session state
   remains in memory.
4. Design §2.5 persistence/migrations were deliberately not implemented in this slice.
5. Linux CI should run the same tests with `LC_TEST_SANDBOX_BWRAP` once the host contract exists;
   the Linux pass otherwise only proves discovery and that the gate fails closed.
