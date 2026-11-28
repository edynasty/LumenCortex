# lc-testing V1 — validation

Date: 2026-11-28. Host: macOS darwin/arm64, `go1.26.5`, `CGO_ENABLED=1`, shared machine (load 8–21 during runs).
Trees: `main` `b21049c` = worktree `wt/core-lc-testing-v1` (`feat/lc-testing-v1`, skill files uncommitted during the runs, no Go input changed); candidate `7c032ae` = `git archive 7c032ae` export in `/tmp/lc-testing-cand-7c032ae` (no other worktree used or modified).
Raw evidence (outside Git): workspace `bench/evidence/lc-testing-v1-20261128/` — `baseline-full/`, `skill-load/`, `scenarios/{RECEIPTS.tsv,logs/,overlay/,S*-affected*.txt}`, `research/`. Every scenario command went through `scenarios/rcpt.sh` (one receipt row per run, logs never overwritten).

## 1. Method and its limits

Each scenario takes a real Core change (a historical commit) or a deliberate one-line mutant, applies the skill as written (`affected.sh` → `SKILL.md` §3 row → `test-matrix.md` path row → reference), runs the selected commands, and checks: right strategy, sensible scope, no missed invariant, no unnecessary full run, correct references, correct status labels.

**Not done:** no model-in-the-loop A/B (an agent with vs without the skill). The selection was applied by the skill's author, so this proves the rules are executable and the commands sound, not that another agent will follow them. No paid model request, benchmark, Docker or Linux run.

## 2. Discovery, loading and anchors (GATE-2)

| Check | Command | Result |
|---|---|---|
| LC project discovery | throwaway `HOME`, built `lcx-go` from this tree, `lcx-go skills list project` in the worktree | `PASS`: `lc-testing`, `source: agents`, `scope: project`, root `<worktree>/.agents/skills`, `enabled: true`, 14,673 bytes (`skill-load/skills-list-project.json`) |
| LC content load | `lcx-go skills show project lc-testing` | `PASS`: content byte-identical to `SKILL.md` (`cmp`); worktree unchanged; nothing written outside the throwaway HOME |
| OMP discovery | OMP `agents` provider reads project `.agents/skills` (OMP `skills.md`) | by documented rule; not separately executed |
| Anchor names | every `Test…` name in the skill vs `func Test…` on `main` / `7c032ae` (`skill-load/anchor-tests.txt`) | 118 names: 110 on `main`, 7 candidate-only (all on lines marked *(candidate)*), 1 placeholder (`TestName` in an example) |
| Runtime grouping | `runtime-group.sh --check`, `--check b21049c`, `--check 7c032ae`; union of the 12 group regexes vs `go test -list . ./runtime` | every runtime test file is in a group on both trees; 658/667 tests selected, the 9 left are helper-process entry points (`TestHelper*`, skip unless spawned) and `TestT1PerfBaseline` (env-gated) — by design |
| Script syntax | `bash -n` on the three scripts | `PASS` |

## 3. Scenarios

Durations are wall seconds from `RECEIPTS.tsv`. All test commands used `env -u GOFLAGS`, `-count=1` and an explicit `-timeout`.

### S1 — small pure-function bug fix (`d587f0f`, `internal/editing/goedit` rename guard)

- `affected.sh 970c7f4 d587f0f`: package `internal/editing/goedit`, importer `internal/toolset`, flag `TEST_CHANGE` only. §3 row "small pure function" → **L1**; no reference needed.
- Red/green: pre-fix `goedit.go` via `-overlay` (file unchanged on `main` since the fix) → `TestReviewMethodRenameRejectsAnonymousPromotedCapture` **FAIL** on the assertion (`rename rebound anonymous struct selector`, 0.80 s); fixed tree **PASS** (0.75 s).
- L1: package `PASS` 2.64 s; `go vet` `PASS` 0.19 s. Importer `internal/toolset` not run: the row does not require L2 for a pure function; the AGENTS.md hand-off gate would run it.
- Verdict: correct layer, ~4 s of tests, no full run during iteration.

### S2 — provider request serialization (`c415dfc`, Anthropic reasoning-budget wire, candidate-only)

- `affected.sh`: `provider/anthropic`, importer `provider`, flags `PROVIDER_WIRE`, `CONCURRENCY` (whole-file scan). §3 provider row → **L2**, load `prompt-tools-cache.md`; `test-matrix` row adds `provider/...`, `protocol`, `internal/agent`, runtime group *Provider*.
- Ran on the `7c032ae` export: anchor `TestV2ConfiguredOutputLimitRefusesBeforeTransport` + wire tests `PASS` 2.1 s; `./provider/... ./protocol ./internal/agent` `PASS` 20.5 s; runtime *Provider* (34 tests) `PASS` 4.9 s; `-race ./provider/anthropic` `PASS` 9.8 s (flag confirmed against the diff: the client has shared state); vet `PASS`.
- No red: the fixed files changed in 4 later candidate commits, so a pre-fix overlay would not be faithful; recorded, not faked.
- Verdict: ~38 s at L2/L3 instead of the 11 min module run.

### S3 — prompt-cache prefix regression (`f5f2443` receipt base, candidate-only)

- `affected.sh`: `internal/agent`, `internal/taskstate`, `runtime` (+6 importers), flags `RUNTIME`, `CONTEXT_CACHE`, `CONCURRENCY`, `PERSISTENCE`; runtime group *Prompt*. Load `context-cognitive.md` + `prompt-tools-cache.md` §4; runtime groups *Context* + *Prompt*.
- Mutant 1 (`base = text`) failed to **compile** (`notesStart` unused): per `SKILL.md` §1 step 5 a build failure is not a red — rejected and redone.
- Mutant 2 (`base = text; _ = notesStart`, notes leak into the cache-stable base): **FAIL** in `internal/taskstate` `TestRenderBlockReceiptBaseRequiresCompleteNotesOnlySuffix` and `internal/agent` `TestReceiptNotesProjectionUsesTypedHistory` ("Task State prefix changed after note_write") within 4.3 s. Unmutated anchors `PASS` (54 tests, 2.6 s).
- L2: `internal/agent internal/taskstate internal/session internal/window` `PASS` 38.9 s; runtime *Context*+*Prompt* (66 tests) `PASS` 22.4 s. L3 `-race internal/agent internal/taskstate` `PASS` 65.8 s.
- Verdict: offline prefix protection exists and catches a prefix leak at L1; no provider request.

### S4 — SQLite transaction boundary (`bbfe699`: Start transaction refuses an unprojected fence)

- `affected.sh`: `internal/session`, `runtime` (+9 importers), flags `RUNTIME`, `CONCURRENCY`, `PERSISTENCE`; groups *Lifecycle*, *Cognitive*. §3 SQLite/store row → **L3**, load `reliability.md` §1.
- Mutant (`fenced && false` in `internal/session/start.go` on `main`): `TestRevocationBeforeTheStartTransactionRefusesTheStart` **FAIL** ("Task State version 0 -> 1 after a refused Start", 10.2 s). Unmutated four constraint-start anchors `PASS` 6.8 s.
- L2: `internal/session internal/migrate cmd/lcx-go` `PASS` 40.6 s; runtime *Persistence*+*Lifecycle*+*Cognitive* (330 tests) `PASS` 478.5 s. L3: `-race internal/session internal/migrate` `PASS` 46.3 s.
- Observation: the *Cognitive* group is 60 % of `runtime`; three groups cost 75 % of the package. The skill's guidance (start with the matching files via `run-regex.sh`, run the group before hand-off) is the cheaper path; recorded for the reviewer.

### S5 — cancellation and process cleanup (`0f5274f` shell timeout/cancel area, on `main`)

- `affected.sh 5ea8279 0f5274f`: 6 packages incl. `config` (→ `WIDE`), flags `RUNTIME`, `CONCURRENCY`, `PERSISTENCE`, `PROCESS`, `PLATFORM`; groups *Lifecycle*, *MCP*. Load `reliability.md` §2–3, §6.
- Mutant 5a (skip the "cancelled before start" check in `internal/shell/runner.go`): **survives** `TestCallerCancellationGraceAndNoStartAfterCancellation` in 1/1 and then 50/50 runs (`-count=50`, 8.1 s; run to measure the mutant's detection rate, not to get a green). Not equivalent: the command context is `WithoutCancel`, so the process does start; the test's "must-not-run" marker stays absent only because the watcher's SIGTERM reaches the shell before `touch` runs. → gap **LT-G4** (P3): the no-start-after-cancel guard is asserted through a race the test always wins on this host.
- Mutant 5b (drop `ctx.Err() != nil` from the group-reap condition): **survives** `internal/shell`, `internal/toolset`, `internal/agent` (34.1 s) and runtime `bench003_timeout`/`lifecycle*` + *Process* (84.4 s). An evidence-only probe test (overlay, not committed: a group member that ignores SIGTERM) is **PASS** on `main` (2.9 s) and **FAIL** on the mutant ("child … ignoring SIGTERM survived a cancelled call", 4.7 s). → gap **LT-G1** (P3; behavior correct today, untested), recorded in `reliability.md` §3 and the design report.
- Unmutated L2/L3: `internal/shell internal/toolset internal/agent internal/mcp internal/jobs` `PASS` 31.3 s; runtime *Process*+*MCP* `PASS` 86.7 s; `-race internal/shell internal/toolset` `PASS` 97.4 s; Linux cross-vet `internal/shell internal/cgroupv2` `PASS` = `COMPILE_ONLY` (Linux runtime `NOT_RUN(platform)`).
- `WIDE` (config changed) → the skill requires the full module run before hand-off; covered for `main` by §4.

### S6 — Core candidate acceptance (`b21049c..7c032ae`), plan only

- `affected.sh b21049c 7c032ae` (`S6-affected-v2.txt`): 14 packages (one deleted on `main`'s view: `provider/internal/wireusage` exists only on the candidate), 18 importers, every risk flag, 26 runtime stems all mapped to groups. → **L4**, load `acceptance.md`.
- Executed cheap gates on the export: A1 `go mod verify` `PASS` 0.2 s; A2 build+vet `PASS` 5.1 s; A9 Linux cross-vet of the Linux-tagged packages `PASS` (`COMPILE_ONLY`). A3 = the plan above. A11: no hosted run exists for `7c032ae` → `NOT_RUN(hosted)`.
- A4–A8, A10 `NOT_RUN(out-of-scope)`: the independent candidate acceptance is the next stage and needs a non-author reviewer.
- First version of `affected.sh` left 5 candidate runtime files unmapped and `runtime-group.sh --check` did not see candidate files; both fixed (range mode, `--check REF`, extra globs) and re-verified (§2).

### Summary

| # | Layer chosen | References | Red proof | Tests run | Missed invariant | Unneeded full run |
|---|---|---|---|---|---|---|
| S1 | L1 | none | overlay pre-fix: red→green | 4 s | none found | no |
| S2 | L2 + race | prompt-tools-cache | not faithful → not claimed | 38 s | none found | no |
| S3 | L2 + L3 | context-cognitive, prompt-tools-cache | mutant red (compile-fail rejected) | 130 s | none found | no |
| S4 | L3 | reliability §1 | mutant red | 572 s | none; group cost noted | no |
| S5 | L3 | reliability §2–3, §6 | mutant red via probe; 2 surviving mutants analysed | 216 s | **LT-G1**, **LT-G4** (test gaps in Core) | full run required by `WIDE`, done once (§4) |
| S6 | L4 plan | acceptance | n/a | 6 s cheap gates | n/a | not run here (next stage) |

Status vocabulary used as defined: `PASS`, `FAIL` (all mutant/red runs), `NOT_RUN(platform|hosted|out-of-scope)`, `COMPILE_ONLY`; no `TIMEOUT` or `BLOCKED` occurred.

## 4. macOS baseline (GATE-8)

`env -u GOFLAGS go test -count=1 -timeout=60m -json ./...` on `main` `b21049c` (`baseline-full/`): exit 0, 661 s wall (load 11–17). 49 packages: 43 pass, 6 without tests (`cmd/lcx-experience-demo`, `docs`, `internal/cgroupv2` (Linux-only tests), `internal/debugger/dapfake`, `pluginapi`, `provider/internal/retryafter`); top-level 2,048 pass / 45 skip / 0 fail; subtests 1,427 pass / 1 skip. Skips are env/platform gated (21 `runtime` helper processes and perf, 8 debugger adapters, evalruntime interpreters, `LCX_BENCH004_TRACE_ROOT` replay) — `SKIP`, not coverage. Slowest packages: `runtime` 634 s (667 tests), `evalruntime` 105 s, `internal/session` 41 s (30.7 s of it `TestDowngradeEveryBoundary`, known lock wait).

Hosted CI (`go-runtime`, Linux): `main` `b21049c` red — `internal/editing` `TestR201TwoOSProcessesCreateRace` (`unsupported_filesystem`, hard links), same tree green twice on `d9a2113` → `FLAKY_SUSPECTED` (LT-G2). Not hidden, not a macOS item; Linux CI is deferred.

Not run: the full module on `7c032ae` (next stage), full-module `-race`, Linux runtime tests, real-adapter env-gated tests, any model request.

## 5. This change's own check

The change touches only `.agents/skills/lc-testing/**` and `docs/reports/lc-testing-v1/**`; `affected.sh` on the worktree reports `DOCS_ONLY` (no Go build or test input; `docs/embed.go` embeds only `lumencortex.schema.json`). Applicable checks: `bash -n` on scripts, script self-runs above, anchor/path check, skill load. The AGENTS.md Go hand-off gate does not apply; the §4 full run on the same `HEAD` stands as the macOS baseline.
