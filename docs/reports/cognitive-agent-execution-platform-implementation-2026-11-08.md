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
5. DAP transport/manager/tools and real adapter integration — pending.
6. Hub/DAG/messages/worktrees/four workers — pending.
7. Persistent Eval + FA-07 host assertion + FA-01 UID broker — pending.
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

Next: DAP transport, trusted adapter registry, owned process lifecycle and permission-scoped tools. Desktop's development-only Core replace points at this task's Core worktree. Both main workspaces remain untouched; no new model requests, delegation, production changes or publication.
