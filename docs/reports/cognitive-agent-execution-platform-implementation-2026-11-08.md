# Cognitive Agent Execution Platform implementation

Task: LC-COGNITIVE-AGENT-EXECUTION-PLATFORM-001. Status: IN_PROGRESS.

## Authority and preservation

Owner's 2026-11-08 task authorizes all six modules, deterministic/local experiments and reversible dedicated Ubuntu VM security setup. No model requests, delegation, paid requests, formal TB2/Trial, production deployment, main merge or publication.

Core baseline `8c363ce0417160136ae7ee84db481fbc78bdd82f`, branch `feat/agent-debugger-execution-platform`, worktree `wt/core-agent-debugger-execution-platform`. Desktop baseline `6d8fe1cc5d2a619e0f1e551cad97cfd553968e2f`, branch `feat/agent-debugger-execution-platform-ui`, worktree `wt/desktop-agent-debugger-execution-platform`. Both main workspaces and their pre-existing changes remain untouched.

The original 513-line design and four DAP experiment files were SHA256 verified against Owner's values and copied byte-identically into this branch. Originals remain in Core main. Evidence: workspace `bench/evidence/cognitive-agent-execution-platform-20261108/ORIGINAL_MATERIALS.json`.

## Design reconciliation

- The original design principally describes DAP and trace observation; it does not contain all six-module details listed in the new task. Owner's current task supplements the design; no requirement is removed. Original bytes remain unchanged.
- Original DAP-only P1 suggests no migration. Current task explicitly requires durable Runtime Events in the existing SQLite database; use the existing migration runner, not a second store.
- Original five compact DAP tools are superseded by the ten named tools in the current task. Typed Core methods remain authoritative; raw DAP requests are not exposed to untrusted clients.
- Original fixture variables were observed before initialization. Those values are not correctness evidence. New adapter experiments must stop after initialization.
- `8c363ce` evidence is actual Linux tests and implementer self-review, not an independent model review. This task also uses only the primary agent; independent Opus review is a subsequent Owner-controlled step.
- Existing embedded Desktop/Core architecture remains. The requested privileged Linux broker is a narrow security boundary, not a second Session Manager or general runtime daemon.

## Delivery sequence and acceptance ledger

1. Contracts, original materials, exact baselines — verified; ADR-0022 records unified interfaces and safety ownership.
2. Runtime Event Bus and same-store durable events — implemented; focused race regression and real Engine shell/reopen smoke passed.
3. Inspector API and existing-layout UI — implemented; real Shell/Notebook artifact, history/filter/reconnect/error-recovery and viewport smoke passed.
4. Agent debugger with safe-boundary stepping — implemented; real file-write/UI stepping, once breakpoint, cancellation, read-only denial and asynchronous child completion checks passed.
5. DAP — transport, registry, Engine grants, run-bound tools, owned Attach, IntelliJ MCP reuse and Desktop panel implemented. Five real adapters and targeted race checks passed; final crash/security matrix and native Wails integration remain under the unified gates below.
6. Hub/DAG/messages/worktrees/four workers — implemented; real Core/Desktop localhost UI path, four concurrent worktrees, dependent fifth task, scoped pause/resume, messages and source revalidation passed. Full-platform lifecycle gate remains below.
7. Persistent Eval reused and extended with tree CPU quota, per-cell CPU accounting and confined artifact paths; Linux resource/scientific race proof passed. FA-07 versioned host assertion and real Harbor 0.25.0 refusal verified; FA-01 broker remains in progress.
8. Existing hooks extension and permission invariants — pending.
9. Cognitive/provenance/verification integration — pending.
10. Full Desktop integration, deterministic capability experiments, full regression/security gates — pending.
11. Scoped local commits, documentation, evidence hashes and independent-review handoff — pending.

All named acceptance criteria in Owner's task remain required. Compile-only and mock-provider tests do not establish real adapter operation, complete delivery or improved LLM success rates.

## Current checkpoint

Event implementation: `pluginapi.Event` identity, SQLite migration `0011`, content-free durable projection, bounded live fanout, cursor/run sequence, retention and explicit expiry, failure latch and affected-run cancellation, session-scoped query/subscribe. Contract: `docs/runtime-event-contract.md`.

Verification: `raw/event-foundation-race.json` records the exact successful race command over session/runtime/diag; `raw/events-real-engine-smoke.*` records real `Engine.Open → NewSession → RunShell → Query → Close → Open → Resume`, output `EVENT_SMOKE_OK lifecycle=3 cursor=3 reopened=true duplicate=false model_requests=0`. The initial regression's incorrect per-run sequence assertion failed and was corrected; its raw output remains preserved. No full-suite or Desktop validation is claimed yet.

Inspector: session/run-scoped transcript details, sanitized worker snapshots, typed Desktop bridge, three-column history/live UI, bounded buffers and existing Artifact Viewer. Focused Core/backend race and frontend window tests passed; TypeScript passed. Browser proof covers actual Shell output, Python-produced artifact bytes, filtered/empty history, disconnected cursor recovery, visible request failure/manual recovery, dark/light at 1440/1180/820/560 px without horizontal overflow. Transport for this smoke is a removed localhost adapter over real Desktop backend, not native Wails; native build remains a final integration gate. Raw receipts and screenshots are in the workspace evidence directory. Temporary smoke sources were removed and their owned backend shut down.

Debugger: run-owned controller, bounded exact-match/once breakpoints, revision-bound controls, safe model/tool/turn stepping, actual asynchronous child completion latch and revocation checks. Race receipts: `raw/agent-debugger-final-race.json`, `raw/agent-debugger-async-final.json`. Real UI receipts: `raw/agent-debugger-real-ui.json`, `raw/agent-debugger-breakpoint-ui.json`; deterministic local provider, zero external requests. TypeScript and Desktop backend targeted race passed. Smoke host exited 0 and temporary sources were removed. Full regression and native Wails remain final integration gates.

DAP: `raw/dap-manager-single-cancel.json` proves real sandboxed manager breakpoint line 4/value 42, foreign-session denial, conditional/write-evaluation denial, stale Agent control denial after human takeover, and clean human stop. Source validation preserves compiler path spelling after resolving containment. Existing managed-process cancellation now converges to one signal operation; concurrent context/transport/close previously produced a repeat-signal EPERM, retained in earlier raw receipts. Sandbox-only task-port/debug operations leave ordinary Eval profiles unchanged. Core debugger checkpoint is `3d6c67f`; Desktop debugger checkpoint is `01a6aff`.

Four-adapter checkpoint: `raw/dap-four-adapter-race.json` records real LLDB, Delve 1.27.2, debugpy and js-debug 1.140.0, plus Engine grant/revocation/durable-event regression. The Node archive matches the publisher SHA256 `27dab92937ec1ab35821ae955aac867544fe06a1b6307229049f2d789af10968`. Host-configured loopback grants are explicit and apply only to program adapters, not Eval. Unix transport uses an owned short private path; Node child DAP connects only to that socket and accepts only the current adapter target token. Terminal reverse requests and arbitrary executable/network configuration remain denied. macOS debugserver required exact standard-descriptor reopen permission, established by kernel denial and successful rerun.

Desktop DAP checkpoint: typed host configuration and Wails adapters, real program panel, grants, launch/owned attach, breakpoints, stack/scopes/variables, read-only watch, step controls, takeover and stop. `raw/dap-real-ui.json` proves actual Desktop backend → Core → sandbox LLDB (temporary localhost bridge, not native Wails), initialized value 42, step into add:2/out main:7 and cleanup. Four required widths have no horizontal overflow; screenshots retained. TypeScript and `go build ./...` passed. `raw/dap-desktop-backend.json` matched no tests and is not regression evidence. Temporary UI smoke sources and services were removed/stopped. Final native build remains pending.

IntelliJ external probe: isolated IDE configuration/project, Streamable HTTP `/stream`, tool whitelist uses unprefixed method names while advertised names retain `xdebug_`. Real Java `Main.java:5` and `value=42` observed. Initial lost response was followed by an unintended duplicate fixture launch; IDs change when duplicate-name peers exit. Both targets and agent breakpoint were explicitly removed (`raw/intellij-direct-cleanup.json`, empty sessions). Core integration must bind a single owned IDE session and deny arbitrary target IDs; HTTP connection success is not integration acceptance.

Resumption deviation: one historical-consultation subagent was mistakenly started before recovering the latest no-delegation instruction and immediately cancelled. No review PASS is claimed for it. Main agent continues directly; no further delegation or paid/model experiments.

Five-adapter integration: `raw/dap-five-adapter-integrated.json` passed real LLDB, Delve, debugpy, Node and IntelliJ, owned same-sandbox Attach, Engine revocation/durable events and protocol regression. IntelliJ uses an explicitly configured global `hostOnly` MCP entry pinned to the project, one manager-owned session per configured server, private IDE IDs and asynchronous status observation. Unknown cleanup stays `cleanup_failed`, not success. `raw/dap-host-config-regression.json` passed ambiguous/oversized/relative host configuration rejection; `raw/dap-mcp-regression.json` passed the MCP race suite. These are macOS/local receipts, not Linux or final full-platform acceptance.

Hub: `raw/hub-regression.json` records Core/session race coverage (four concurrent writers, duplicate launch/message rejection, foreign membership denial, dependency admission, message restart/fence/privacy). `raw/hub-real-ui.json` records actual Desktop backend/Core, four concurrent isolated writes, selected-worker pause at AfterModelCall/resume, persisted message and dependent integration task; all five wrote expected bytes. `raw/hub-message-revalidation.json` records page-global withdrawn/error injection, content clearing and recovery. Early isolated-realm browser injections did not exercise the app and are not failing-before evidence. Eight viewport captures are retained; final elapsed-time addition was observed at 560 px, not rerun across the whole matrix. TypeScript and the UI detector passed. Temporary smoke sources were removed; backend exited 0. No native Wails or model-quality claim.

Hub commits: Core `ccf632b`, Desktop `a42fd2f`. Eval: `raw/eval-linux-targeted-final.*` proves four descendants throttled (`nr_throttled +10`), per-cell CPU excludes earlier work, cgroup-bound tree and real scientific round trip with independent semantic/delivery checks. `raw/eval-integration-lifecycle.*` covers macOS real Python/JS/artifact/interrupt/reset paths. Miniconda SciPy is locally malformed (`__thread_bss` load failure); scientific macOS case is not PASS. The deterministic science fixture now disables provider retries so a failed transition cannot advance its script. No system dependency installation occurred.

FA-07: `docs/host-boundary.md` specifies schema/admission. `raw/boundary-real-manager-smoke-corrected.*` proves actual Python side effects allowed only in the explicit development mode and refused under the required boundary; Linux race identity/reset/refusal and installed Harbor 0.25.0 rejection passed. The host report is FAIL (same UID, public bwrap, unconfined restriction disabled), not production PASS. No environment changes or formal Trials occurred.

Next: implement FA-01 independent-UID broker and reversible Ubuntu security matrix. Final Desktop resource/health integration stays in the Desktop integration gate. Development-only Desktop replace remains this task's Core worktree. No main changes, production action or publication.
