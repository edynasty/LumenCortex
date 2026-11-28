# lc-testing V1 — coverage matrix

Where each requirement of the lc-testing V1 task lives in the skill (`.agents/skills/lc-testing/`, abbreviated `S` = `SKILL.md`, `TM` = `references/test-matrix.md`, `PTC` = `prompt-tools-cache.md`, `CC` = `context-cognitive.md`, `REL` = `reliability.md`, `ACC` = `acceptance.md`) and how it was checked (`V§n` = `LC_TESTING_VALIDATION.md` section).

## 1. Layers and selection

| Requirement | Location | Checked |
|---|---|---|
| L0 Preflight (repo/branch/tree, env, affected modules, known failures, risk flags, no whole-repo scan) | S §2 L0; `scripts/affected.sh` | V§3 all six scenarios ran `affected.sh` (0.7–3.1 s) |
| L1 Focused as default | S §1 steps 5–6, §2 L1 | V§3 S1 |
| L2 Package / Integration by impact | S §2 L2, §3; TM §2–3; `runtime-group.sh` | V§3 S2–S5 |
| L3 Reliability (cancel, timeout, retry, txn, race, permissions, processes, worktrees) | S §2 L3, §4; REL §1–4 | V§3 S4, S5 |
| L4 Candidate / Release | S §2 L4; ACC | V§3 S6 (plan), V§4 baseline full run |
| Benchmark separate, never default | S §2 BENCH, §2 last para | — (rule) |
| Risk table (docs … Core candidate, 14 change types) | S §3; TM §2 path rows | V§3 per scenario |
| Upgrade with reason; never skip a touched invariant | S §1 step 3, §3 intro, §4 | V§3 S3/S5 escalations |
| Ordinary change not forced into full tests (GATE-4) | S §2 hand-off gate paragraph; LT-D1 | design §5 (planner decision) |

## 2. Agent loop and integrity

| Requirement | Location |
|---|---|
| Understand → … → report loop | S §1 |
| Probe before long runs; early working version | S §1 last para; REL §5 |
| Red regression for bug fixes; fails on behavior, not compile | S §1 step 5 (`-overlay` pre-fix/mutant) — V§3 S1 red/green, S3 compile-fail rejected |
| New tests cover target and failure path | S §4 intro, §1 step 2 |
| Diagnose, no mechanical reruns; recorded targeted rerun | S §1 step 7, §6 triage |
| No weakening tests / deleting failures / answer keys / verifier holes | S §5 |
| SKIP/TIMEOUT/BLOCKED/NOT_RUN never PASS | S §7 |
| No fix claim without evidence; claim ladder | S §7 last para |
| Pre-existing vs new regression | S §6 triage, §8 report |

## 3. Areas (GATE-5, GATE-6)

| Area | Location | Anchors verified to exist |
|---|---|---|
| Prompt (clauses, permissions, structure, no dynamic data in stable prefix, lost rules EVO-07) | PTC §1 | V§2 anchor check |
| Tools (names, schema, validation, permission, serialization, order, provider compat, errors, causality) | PTC §2 | V§2 |
| Provider wire / usage buckets / retries / hidden reasoning | PTC §3 | V§3 S2 |
| Prompt cache (ADR-0024 receipts, system/tools stability, transcript prefix, Task State, resume, increments) | PTC §4; CC | V§3 S3 (mutant caught) |
| Context Space lifecycle and isolation | CC §1 | V§3 S3 |
| Cognitive Core persistence, permission narrowing, state transitions, recovery | CC §2 | V§3 S4 (Cognitive group) |
| SQLite migration, recovery, compatibility, data preservation | REL §1 | V§3 S4 |
| Concurrency, cancellation, race | REL §2 | V§3 S4, S5 |
| Processes (exit, timeout, orphans, release) | REL §3 | V§3 S5 (gap LT-G1 found) |
| Worktrees | REL §4 | — (rows only) |
| Timeouts: test vs package vs agent vs process vs provider idle vs CI | REL §5 | V§4 durations |
| Platform (macOS gate, Linux NOT_RUN, compile-only) | S §9; REL §6 | V§3 S5 cross-compile |

Mandatory invariants (S §4): hidden reasoning, permissions only narrow, terminal state, no replayed effects, atomic transactions, path/trust confinement, no secrets, hermetic tests.

## 4. Gates

| Gate | Evidence |
|---|---|
| GATE-1 no duplicate build | design §1 (empty `.agents/`, CORE-REQ-02 unrelated, Opus candidate inherited) |
| GATE-2 discoverable by repo rules | V§2 `lcx-go skills list/show project` |
| GATE-3 layers and rules executable | V§3 commands generated from the skill and run |
| GATE-4 no forced full runs | S §2; LT-D1 open for the planner |
| GATE-5 data/permission/concurrency/recovery kept | §3 above; S §4 |
| GATE-6 CS / Cognitive / Prompt / Tools / Cache strategies | §3 above |
| GATE-7 real scenario evidence | V§3 RECEIPTS (outside Git) |
| GATE-8 macOS tests pass; history reported | V§4 full run on `main`; LT-G2 hosted |
| GATE-9 no architecture change | diff touches only `.agents/skills/lc-testing/**` and `docs/reports/lc-testing-v1/**` |
| GATE-10 commit/push | final report |
