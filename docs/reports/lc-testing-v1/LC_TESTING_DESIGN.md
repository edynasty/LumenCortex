# lc-testing V1 — design

Date: 2026-11-28. Branch `feat/lc-testing-v1` (from `main` `b21049c`). Skill: `.agents/skills/lc-testing/` (version `1.0.0-rc1`).
Status: `LC_TESTING_READY_FOR_INDEPENDENT_REVIEW` candidate — not frozen; the planner's acceptance and an independent review decide (`CORE_PREPARATION_TODO.md` Phase T2).
Companion reports: `LC_TESTING_COVERAGE_MATRIX.md` (requirements → skill sections), `LC_TESTING_VALIDATION.md` (scenarios, commands, results).

## 1. What existed before this work (GATE-1)

| Item | Verified state (2026-11-28) | Used how |
|---|---|---|
| `.agents/skills/` in Core | `.agents/` existed and was empty on `main`; `git log --all -- '.agents/**'` returns nothing — no branch ever committed a skill | new skill placed here (ADR-0015 project root, see §4) |
| CORE-REQ-02 | `feat/core-req-02-host-file-read` (`6180347`, 5 ahead, unmerged) and `review/…` (`bf266aa`): host read of one checkout file (`ReadWorkspaceFile`). No skill, test-policy or `.agents` file in its diff | unrelated to lc-testing; not reused, not touched |
| Opus `lc-testing` candidate | workspace `archives/core-preparation-monitor/lc-testing/candidate/` (`v0.1.0-candidate`): `SKILL.md` 126 lines + `levels-and-selection.md`, `go-sqlite-runtime.md`; its SKILL named five more files that were never written (run stopped by SIGTERM, `opus.log`) | inherited: layer idea, Diff-to-Test commands, red-first rule, status vocabulary, Go/SQLite/runtime rules, timeout facts. Changed: L4/L5 merged into one L4; missing files replaced by the five references below; mandatory full runs limited to the hand-off gate |
| Test-scope guidance | Core `AGENTS.md` "Required validation": focused tests during development, then `go test -count=1 ./...` + build + vet for affected Go code. No other repo rule; `bench/evidence/core-final-independent-acceptance-20261125/{final_gates.sh,gate.py,docs_only_delta.sh}` are the only gate scripts (outside Git) | skill defers to AGENTS.md (hand-off gate) and adds layered selection below it; LT-D1 |
| Test seams | scripted providers (`internal/agent/loop_test.go`, `runtime/agent_test.go`, `runtime/agents_test.go`), `runtime/testutil_test.go` `TestMain` private HOME + 64-model catalog, `fakeAnthropic` `httptest` SSE, crash helper processes, `testHook*` globals, migration fixtures | named per area in the references; no new fake |
| CI | `go-runtime.yml`: `go test ./...` on ubuntu, no `-timeout`, `-race`, `-count=1`, path filter or `timeout-minutes`. `ci.yml`/`harness-ci.yml` are Node legacy | unchanged (E1 owns CI); hosted state recorded (LT-G2) |
| Candidate `7c032ae` vs `main` | `main` is an ancestor; 45 commits. Test-relevant additions: receipt-notes / cache-accounting / cache-fallback tests in `internal/agent`, `provider/internal/wireusage`, `protocol/usage.go`, run-posture tests in `runtime` and `internal/session`, V2 prompt-contract tests | references mark candidate-only anchors *(candidate)*; scripts work on both trees (`runtime-group.sh --check 7c032ae` and `--check b21049c` both cover every runtime test file) |

## 2. Mature harness practice (research)

Sources: released source and docs of OMP `v19.2.1`, OpenCode `v1.18.32`, Codex `rust-v0.172.0`, public `claude-code` repo (`main`), cloned in `wt/fable-architecture-challenge-20261126-external/` (`CLONES.tsv`). Inventories by three read-only scout agents, kept with file:line anchors in workspace `bench/evidence/lc-testing-v1-20261128/research/` (outside Git); conclusions below were spot-checked, not all re-read. Labels: **REL** released code/tests, **DOC** official docs in the repo, **COMM** community, **INF** inference. Claude Code's product is closed: only its plugin/mod test runner and CHANGELOG are public.

| Mechanism | Mature harness practice | LC today | Adopt? | Why |
|---|---|---|---|---|
| Fake provider | Codex: wiremock SSE + recorded requests (REL); OpenCode: local SSE server DSL with reset/hang/429 (REL); OMP: in-process `mock` model, no HTTP (REL) | both kinds exist: scripted in-process providers and `fakeAnthropic` `httptest` SSE | reuse, name the seam per test | parity already; a new fake would duplicate |
| Agent-loop tests | mount turn sequence → drive turn → assert next request body (Codex REL, OpenCode REL) | same pattern with `scriptedProvider.seen` | reuse | — |
| Tool contract | registry duplicate rejection + schema normalization + sent-tools assertions (Codex, OMP REL); per-tool JSON-schema snapshot (OpenCode REL) | schema validity, digest, catalog agreement tests; **no tool-order test** | adopt "add an order test when order logic changes" | gap is narrow; no snapshot infra needed |
| Prompt regression | clause/structure asserts, no prompt snapshot (OMP REL, Claude mods DOC); normalized whole-request snapshots (Codex REL) | `TestSystemPromptFollowsTheRunPurpose` clause pattern; default rules unpinned (EVO-07) | adopt clause lists; not snapshots | byte goldens of the prompt churn on every edit; clauses pin behavior |
| Prompt-cache prefix | Codex `prompt_caching.rs` asserts prefix identity across requests (REL); OMP capturing-fetch marker positions (REL) | candidate receipt-projection + cache tests; offline 103-request replay outside the repo | adopt the 4-step offline prefix check | offline, free, already half present |
| Fault injection | SSE truncation, 5xx/429 + retry-after, WS close taxonomy, SIGKILL (Codex, OpenCode, OMP REL) | `httptest` error cases, crash helpers, `testHook*`, migration `os.Exit` hooks | reuse; list per area | — |
| Concurrency | barriers/gates, no sleeps; no race detector (Codex, OMP) | Go `-race` available but **not in CI**; `*_race_test.go` patterns | adopt `-race` at L3 for affected packages | Go has it for free |
| Flake policy | OMP nextest `retries = 0` ("a flaky test is a bug"); Codex `retries = 1` | no retries anywhere; `final_gates.sh` never reruns | keep no-retry; one recorded targeted rerun | matches existing discipline |
| Timeouts | per-test + per-chunk watchdog + job limits (OMP, Codex) | only `go test -timeout` per package; `runtime` ~11 min serial | per-package budgets from measurements | Go has no per-test timeout flag |
| Minimal scope | Codex "run the crate you changed" (DOC); OpenCode forbids root runs (REL); Claude Code `verify` skill before commit (DOC, CHANGELOG) | AGENTS.md full run for any Go change | layered selection + `affected.sh`; full run only as hand-off gate (LT-D1) | the full suite is ~11 min; iteration on it wastes time |
| Fast vs full CI | path filters + small PR gate + post-merge full shards (Codex, OMP REL) | one unfiltered job | **not now** (E1, Linux CI deferred) | out of scope this round |
| Bench isolation | `#[ignore]`/`manual`/env opt-in for live and bench (Codex, OMP REL) | real-model tests env-gated (`LCX_REAL_MODEL`, `realmodel` tag) | keep; skill forbids setting them | already isolated |

Not adopted: bucketed runners, test sharding, snapshot frameworks, nightly jobs — they solve CI scale LC does not have yet, and need CI work the plan assigns to E1.

## 3. Design

- **Form:** one repository skill, progressive loading. `SKILL.md` (≈150 lines) holds the loop, layers, risk table, non-optional invariants, integrity rules, result vocabulary and report format. Five references load only when the plan needs them (`SKILL.md` §10): `test-matrix.md`, `prompt-tools-cache.md`, `context-cognitive.md`, `reliability.md`, `acceptance.md`.
- **Layers:** L0 Preflight, L1 Focused (default), L2 Package/Integration, L3 Reliability, L4 Candidate/Release; BENCH separate and never implied. The Opus candidate's L5 is folded into L4 (one acceptance matrix in `acceptance.md`).
- **Selection:** risk table by change type (`SKILL.md` §3) and path table (`test-matrix.md` §2). Never below the row for the invariants touched; escalation needs a reason.
- **`runtime` package:** 667 serial tests, 634 s idle. L2 runs **test groups by file** (`scripts/runtime-group.sh`, 12 groups, every test file covered on `main` and `7c032ae`), not the package. Whole package at L4 and at the hand-off gate.
- **Scripts (read-only helpers, bash + git + go list):** `affected.sh [BASE [TARGET]]` → changed files, packages, importers, risk flags, suggested runtime groups; `runtime-group.sh` → `-run` regex per group, `--check [REF]` coverage; `run-regex.sh` → exact `-run` regex for given test files.
- **Evidence:** `-count=1`, explicit `-timeout`, `-json` for long runs, exit codes, load; statuses PASS/FAIL/TIMEOUT/BLOCKED/NOT_RUN/SKIP/UNKNOWN; claim ladder IMPLEMENTED → SELF_VERIFIED → READY_FOR_REVIEW → INDEPENDENTLY_VERIFIED.
- **Red proof without old checkouts:** `go test -overlay` swaps the pre-fix file or a one-line mutant; build failures are not red; surviving mutants are coverage gaps.

## 4. Discovery and loading

ADR-0015 / `internal/skills/registry.go` `DefaultRoots`: project roots are `<workspace>/.lumencortex/skills` (writable), `<workspace>/.agents/skills`, `<workspace>/.claude/skills`. OMP's `agents` provider reads `.agent(s)/skills` (OMP docs `skills.md`). `.agents/skills/lc-testing/` is therefore found by LumenCortex itself and by OMP without a new loader or a copy. Verified with `lcx-go skills list project` / `skills show project lc-testing` in a throwaway HOME (`LC_TESTING_VALIDATION.md` §2). Nothing was installed globally.

## 5. Decisions needed and deliberate limits

| ID | Item | Proposal | Owner |
|---|---|---|---|
| LT-D1 | Core `AGENTS.md` requires `go test -count=1 ./...` (~11 min) for any affected Go code; the skill runs it once at hand-off, never per iteration, but cannot waive it | keep until the planner decides whether layer selection (L1–L3 by risk) may replace it for ordinary changes; the skill already says AGENTS.md wins | planner |
| LT-D2 | Mandatory entry (`T2-07`) | after freeze, Core `AGENTS.md` points to the skill in one line; not done here (user-owned unstaged `AGENTS.md` edit in the main checkout) | planner |

Not implemented here (owned by later stages): CI timeouts/race/path filters and fast/full split (E1, EVO-01/EV-07/EV-08); committed replay data (EVO-03); fixing `TestDowngradeEveryBoundary`'s 30 s lock wait (E1); default-prompt clause tests (EVO-07); Linux runtime gates (deferred).

## 6. Gaps found while validating (not fixed: Core test/production code is out of this task's scope)

| ID | Sev | Finding | Evidence | Next owner action |
|---|---|---|---|---|
| LT-G1 | P3 | No committed test covers a cancelled shell call whose group has a member ignoring SIGTERM; the reap condition `ctx.Err() != nil` in `internal/shell/runner.go` can be deleted without any failure in `internal/shell`, `internal/toolset`, `internal/agent` or `runtime` lifecycle/process tests | overlay mutant S5b + probe test red on the mutant, green on `main` (`LC_TESTING_VALIDATION.md` §3 S5) | add the probe as a committed regression test in a Core change (candidate acceptance or E1); behavior is correct today |
| LT-G2 | P2 (hosted signal) | `go-runtime` red on `main` `b21049c` (run 45318690704): Linux `internal/editing` `TestR201TwoOSProcessesCreateRace/…/before_commit` → `unsupported_filesystem` (2 hard links). Same tree green on push and PR runs of `d9a2113` → `FLAKY_SUSPECTED`, not a timeout, for this run (the earlier red `main` runs `cfef043`, `ef10ca7` were not re-examined here) | `gh run view 45318690704 --log-failed`; `git diff d9a2113 b21049c` empty | candidate acceptance / E1 investigates the Linux race; not a macOS gate item |
| LT-G3 | P3 | No tool-definition order-stability test (dedupe-then-sort in `internal/agent/stepspecs.go`) | `test-matrix`/`prompt-tools-cache` §2 | add when order logic changes |
| LT-G4 | P3 | `TestCallerCancellationGraceAndNoStartAfterCancellation` does not detect removal of the "cancelled before start" check in `internal/shell/runner.go` (0/50): the process starts and is SIGTERMed before its `touch` runs, so the marker assertion depends on a race | mutant 5a, `-count=50` (`LC_TESTING_VALIDATION.md` §3 S5) | make the command's first effect precede any signal handling (or assert no process was started, e.g. via a start hook) when this code is next touched |

## 7. For the independent reviewer

1. Is the L2 `runtime` grouping sound (groups by file stem; `--check` proves file coverage, not behavior coverage)?
2. Do the risk rows ever allow skipping an invariant listed in `SKILL.md` §4?
3. LT-D1 wording: does the skill make the AGENTS.md hand-off gate unambiguous while avoiding per-iteration full runs?
4. `affected.sh` flags scan whole changed files (over-flagging by design); confirm no under-flagging path (deleted packages, testdata, embedded docs).
5. Validation is offline structural plus real test runs; no model-in-the-loop A/B was performed (`LC_TESTING_VALIDATION.md` §1).
