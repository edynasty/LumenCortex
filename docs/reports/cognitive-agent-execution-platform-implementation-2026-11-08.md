# Cognitive Agent Execution Platform implementation

Task: LC-COGNITIVE-AGENT-EXECUTION-PLATFORM-001. Status: LOCAL_IMPLEMENTATION_VERIFIED — READY_FOR_INDEPENDENT_REVIEW (2026-11-09). This is not a release, production Linux acceptance or independently reviewed PASS.

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
5. DAP — five real adapters, owned Attach, grants, revocation, real four-adapter crash matrix, bounded stderr and four-language fault/repair experiment passed. Linux DAP adapter acceptance is not claimed.
6. Hub/DAG/messages/worktrees — four real concurrent isolated writers, dependent fifth task, control/message/UI paths and source revalidation passed.
7. Persistent Eval — real Python/JS lifecycle, artifacts and Ubuntu scientific round trip passed; FA-07 refuses unsupported Harbor admission; FA-01 dedicated Ubuntu independent-UID broker matrix passed and VM changes were rolled back.
8. Unified hooks — versioned vocabulary, policy narrowing/denial, timeout/recursion bounds and real sandboxed process smoke passed; in-process hooks remain trusted code.
9. Cognitive integration — bounded provenance-checked graph/Attention, explicit thread evidence, workflow/verification authority separation and actual file/reopen smoke passed.
10. All 15 Desktop surfaces implemented and exercised through real Core/backend paths. Native Wails arm64 build/self-check, full Go race and Vitest passed. Browser captures are not native interactive acceptance.
11. Scoped phase commits, interface documents, complete evidence indexes and independent-review handoff delivered. Workspace evidence is outside both Git repositories; its SHA256 manifest is the integrity receipt.

All named acceptance criteria in Owner's task remain required. Compile-only and mock-provider tests do not establish real adapter operation, complete delivery or improved LLM success rates.

## Current checkpoint

Event implementation: `pluginapi.Event` identity, SQLite migration `0011`, content-free durable projection, bounded live fanout, cursor/run sequence, retention and explicit expiry, failure latch and affected-run cancellation, session-scoped query/subscribe. Contract: `docs/runtime-event-contract.md`.

Foundation verification: `raw/event-foundation-race.json` and `raw/events-real-engine-smoke.*` prove real Engine shell execution and durable reopen/cursor recovery. Earlier failure receipts are retained. Final assembled validation is recorded below and in `FULL_REGRESSION_RESULTS.json`.

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

FA-01: real Core Engine → Unix Broker → independent UID/GID 62081 Python/Node and required host assertions passed. `raw/broker-security-matrix-race.*` covers four kernels, isolation, artifact permissions, interrupt/state retention, timeout, OOM/bystander, restart and Dispose under race instrumentation. Raw replay/foreign-peer, revoke/client SIGKILL, broker SIGKILL/restart and keeper SIGKILL cases passed. The leaked namespace FD, synthetic-directory access, callback absence, proc-self-bind admission and stale-socket recovery defects were fixed; failure receipts remain unchanged. Original Ubuntu profiles/sysctls are restored, temporary broker installation removed and owned-process count is zero. `BROKER_SECURITY_RESULTS.json` and `docs/security/execution-platform-security.md` record exact coverage and limits. This is not production deployment or independent Opus review.

Hooks: `raw/hooks-runtime-confirmed.*` passes targeted race policy and real Engine file-write/no-request/approval scenarios. `raw/hooks-public-smoke.*` runs the standalone public API and observes a sandboxed process denial plus one durable event. macOS external command isolation/timeout is real; Linux command hooks require the existing caller-isolation setup and are not covered by that macOS receipt. Same-action workflow approval initially bypassed hooks; the failure is preserved in `raw/hooks-runtime-integration.*` and fixed. Contract: `docs/runtime-hooks.md`.

Cognitive integration: `raw/cognitive-integrated-race.*` passes graph/forgery/staleness/exclusion/revocation and existing event/notes/projection regressions. `raw/cognitive-public-smoke.*` proves a real file write, grounded execution record, public API and Engine reopen (zero external model requests). `tool.end` now publishes after result commit. No Node tables, new DB, all-log injection, Workflow writer or semantic-success shortcut; contract `docs/context-evidence.md`. Explicit thread retrieval carries only bounded Attention plus cross-session lineage. Final full-platform gates remain pending.

Desktop context/resource integration: `raw/context-real-ui.json` proves committed execution sources, explicit failure/content clearing/manual recovery, session isolation, real Python/JavaScript Notebook output and Reset generation without stale usage. Sixteen Inspector/Notebook light/dark viewport captures (1440/1180/820/560) show no horizontal overflow. Resource limits bind each kernel's budget; memory peak unavailable is unknown; disk per-file and aggregate semantics are disclosed. `raw/desktop-resource-contract-race.*` passes actual reset/isolation and `raw/desktop-context-final-types.*` passes TypeScript. Temporary hosts exited 0 and smoke entries were removed. Native build/full regression remain separate gates.

Final admission correction: Desktop previously always opted into Eval, contradicting the current default-off requirement. It now requires `LCX_EVAL_ENABLED=1`; Linux also requires an administrator-issued independent-UID broker connection, without in-process fallback. `raw/desktop-eval-explicit-opt-in.*` proves default refusal and explicit execution/reset under the existing macOS boundary. This is a real host-boundary fix, not a new runtime owner. A final native rebuild follows the correction.

Final gate corrections: the Effective Prompt/capability catalog omitted worker messaging and granted DAP tools; it now uses the existing dynamic bindings. A synthetic nonexistent reader ID and direct `prepareRun` fixtures were migrated to real session/tracked lifetimes without weakening fence, delivery or replay assertions. `rand.Text` exceeded the module's Go 1.23 contract; events now reuse the existing random 128-bit ID helper. The one-second timeout fixture now uses its existing non-login Bash so user profiles cannot consume the entire command deadline; partial output and timeout assertions remain.

The final race run exposed a real classification regression: Debugger boundary revocation was reported as `cancelled` instead of `fenced`. All debug-boundary exit paths now preserve `ErrFenced`. `raw/fence-debug-boundary-cleared.*` proves six real cross-process fence trials and unaffected concurrent work. A deterministic loop regression pins the model/tool/delivery boundary cases.

Default-off UI proof: `raw/default-off-ui-final.json` and PNG show actual document creation/edit/save through Desktop/Core with execution disabled. Runtime/health/resource failures remain visible, not fabricated empty results. A language-less cell is refused without executing. The first smoke incorrectly requested a runtime-bound Python notebook while Eval was off; the next interactive host expired unused; both failures are preserved. The final host exited 0 and all three temporary sources were removed. Full native and browser evidence have distinct scope; native visual product acceptance remains an Owner check.

Full race fixture correction: `TestIdleWorkerDeathIsReportedAsLost` checked the crash event after a fixed 1.8s sleep, but the existing implementation marks failed before process/cgroup cleanup and emits only after cleanup. It now waits boundedly for the actual crash event, preserving failed state, explicit lost-state event and new-generation assertions. No Eval implementation or cleanup order was changed.

## Frozen delivery and final verification

Core implementation commit: `a3c322d0c52b71a0c9f89a4e96a9cac108382240`, branch `feat/agent-debugger-execution-platform`; Desktop: `31f5297f90528c453224f1175c76c9a4773c2a2d`, branch `feat/agent-debugger-execution-platform-ui`. A following Core documentation-only commit records this report; `GIT_STATE.txt` records its exact HEAD and every phase commit/file change. Desktop deliberately retains the local Core replace, not a release pin.

| Final check | Observed result | Receipt |
|---|---|---|
| Core ordinary full suite | 2292 PASS, 36 SKIP, 0 FAIL | `raw/core-normal-verified.*` |
| Core full race suite | 2292 PASS, 36 SKIP, 0 FAIL | `raw/core-race-verified.*` |
| Core build / vet | exit 0 / 0 | `raw/core-build-verified.*`, `raw/core-vet-verified.*` |
| Desktop Go race | 175 PASS, 0 FAIL | `raw/desktop-race-verified.*` |
| Desktop vet | exit 0 | `raw/desktop-vet-verified.*` |
| Vitest | 78 files, 433 tests PASS | `raw/frontend-vitest-stable.*` |
| Frontend + Wails macOS build | exit 0; arm64, ad-hoc signature, self-check PASS | `raw/desktop-native-verified.*` |

Go counts include named subtests, excluding package summaries. Opt-in adapter cases skipped in generic suites have separate real receipts. Full suite runs are macOS; Ubuntu safety/resource receipts are targeted real VM experiments. The early pipe-interrupted suite is incomplete evidence, never a passing run.

Native development artifact: `wt/desktop-agent-debugger-execution-platform/build/local/LumenCortex Desktop.app`, built from Desktop `31f5297` and Core `a3c322d`, runtime `0.1.4-preview`, app `0.1.0`. No DMG/release, notarization, remote Core pin or publication is claimed.

## Public contracts and storage changes

- Events: versioned identity, per-run sequence, bounded durable cursors/query/resume/subscribe, snapshots/tree/timeline/details; `docs/runtime-event-contract.md`.
- Agent controls: `AgentOptions.Debug`, revision-bound snapshots/controls and safe-boundary breakpoints; `docs/agent-debugger.md`.
- DAP: explicit adapters, separate grants/Attach/evaluation, run-owned sessions and ten typed tools; `docs/dap-debugger.md`.
- Hub: existing tasks/work units/relations plus dependency admission, bounded messages and worktree isolation; `docs/agent-hub.md`.
- Eval: per-cell resources, host assertions, typed peer-authorized broker; `docs/persistent-eval.md`, `docs/host-boundary.md`, `docs/security/execution-platform-security.md`.
- Hooks/context: versioned lifecycle narrowing, read-time grounded graph/Attention without a second database; `docs/runtime-hooks.md`, `docs/context-evidence.md`.
- SQLite additions: `0011_runtime_events.sql`, `0012_worker_messages.sql` and their down migrations in the existing migration owner. No persisted private reasoning or new Session Manager.
- Desktop: typed Wails adapters and domain DTOs; explicit `LCX_EVAL_ENABLED=1`, Linux broker socket/grant required, no fallback.

## Definition of Done and remaining gates

Inspector, Agent Debugger, DAP on the listed macOS adapters, Hub, Eval on the documented macOS/Ubuntu boundaries, Hooks, cognitive integration and Desktop backend/browser integration are locally verified by the named receipts. No known applicable P0/P1 implementation failure remains in the exercised scope. This is implementer evidence, not an independent security PASS.

`COGNITIVE_AGENT_EXECUTION_PLATFORM_COMPLETE` is **not issued as a platform-wide acceptance claim**: interactive native Wails acceptance and independent Opus review remain open; Linux DAP adapters/command hooks and other platforms were not validated. The exact per-module classification is in workspace `ACCEPTANCE_DECISION.md`; these limits do not imply missing code was replaced with mocks.

Owner checks: (1) independent read-only Opus review of these two candidate branches and evidence; (2) native packaged-app acceptance; (3) any production AppArmor/broker installation; (4) any real-model experiment, main merge, push/tag/release. None is authorized by local completion. macOS SciPy remains locally malformed; the scientific case passed on Ubuntu without changing system packages. Broker remains one authorized client with bounded kernels, not a multi-tenant service. Default-off UI repeats resource-disabled warnings; no measured usage is invented.

Cleanup: task frontend/isolated IDE and temporary backend services stopped; final local process scan found no exact task-worktree/isolated-IDE/DAP-path matches. Ubuntu owned broker/worker scan was empty after restoring original profiles/sysctls. No main/source originals, historical benchmark rewards/cost ledgers or Sub2API settings were edited. Core main remains `937e161`, Desktop main `4a6c2f3`. Original five material hashes still match. No real model requests, paid trials or publication occurred. Earlier sections are chronological checkpoints; this section is the final state.
