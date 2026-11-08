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

1. Contracts, original materials, exact baselines — materials/baselines verified; interface inventory in progress.
2. Runtime Event Bus and same-store durable events — pending.
3. Inspector API and existing-layout UI — pending.
4. Agent debugger with safe-boundary stepping — pending.
5. DAP transport/manager/tools and real adapter integration — pending.
6. Hub/DAG/messages/worktrees/four workers — pending.
7. Persistent Eval + FA-07 host assertion + FA-01 UID broker — pending.
8. Existing hooks extension and permission invariants — pending.
9. Cognitive/provenance/verification integration — pending.
10. Full Desktop integration, deterministic capability experiments, full regression/security gates — pending.
11. Scoped local commits, documentation, evidence hashes and independent-review handoff — pending.

All named acceptance criteria in Owner's task remain required. Compile-only and mock-provider tests do not establish real adapter operation, complete delivery or improved LLM success rates.

## Current checkpoint

Next: extend `pluginapi.Event`/`runtime.events` with versioned event identity and reliable same-store history. Preserve nonblocking live fanout; important lifecycle persistence must return or surface errors rather than silently drop. Investigate existing session migrations, run identity and producer contracts before editing.

Verification so far: five original hashes match; five copies match; requested worktrees created from exact commits. No product behavior changed or runtime tests run yet.
