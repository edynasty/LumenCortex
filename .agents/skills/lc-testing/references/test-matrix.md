# Diff-to-Test matrix

Load this when writing a Diff-to-Test plan for an L2+ change, an exported API, `config`/`protocol`, or CI/scripts. Paths and test names were checked against Core `main` `b21049c` and candidate `7c032ae` on 2026-11-28; names marked *(candidate)* exist only on `7c032ae`. When a name no longer exists, find its successor (`git grep -n 'func TestName' -- '*_test.go'`) and record the substitution in the plan.

## 1. Compute the change set

```bash
.agents/skills/lc-testing/scripts/affected.sh            # BASE = merge-base with main
.agents/skills/lc-testing/scripts/affected.sh <base-sha> # explicit base (review of a range)
```

It prints changed files (committed, staged, unstaged, untracked), changed packages (including embedded assets and `testdata/`), direct in-module importers (production and test imports) and risk flags. It is read-only. Use its output as the plan's input, not as the plan: the rows below decide what must run.

Why importers are not run wholesale: `runtime` imports almost every package (its test closure covers 47 of 50 packages, Fable review 2026-11-26), so "all importers" degenerates into "the `runtime` package" (~12 min). Run the non-`runtime` importers in full, and `runtime` as the matching test group from §3 unless the row says the whole package.

## 2. Path → tags → minimum tests

"L2 packages" run in full. "Anchors" are tests that must appear in the plan and in the results (they usually run inside the package runs). A path that matches no row takes the row of its nearest parent area; never drop a path from the plan. A diff spanning several rows takes their union.

| Changed path | Tags | L2 packages (minimum) | Anchors / L3 |
|---|---|---|---|
| `runtime/prompt*.go`, `runtime/agents*.go`, `internal/instructions/**`, `internal/skills/**`, system prompt in `internal/agent/loop.go` | PROMPT, CONTRACT, TRUST | changed pkg, `internal/agent`; `runtime` group *Prompt* | `runtime/prompt_test.go` `TestEffectivePromptMatchesARealRun`, `TestToolCatalogsMatchAFullPolicyRun`; `internal/agent/interactive_test.go` `TestSystemPromptFollowsTheRunPurpose`; `runtime/agents_test.go` `TestAgentInstructionsAppendedAfterDefaultPrompt`; skills: `internal/skills/registry_test.go`, race `internal/skills/snapshot_race_test.go`. Load `prompt-tools-cache.md` |
| `runtime/capabilities.go`, `internal/toolset/**`, `internal/agent/stepspecs.go`, `internal/agent/argschema.go` | TOOLS, TRUST, CONCURRENCY | `internal/toolset`, `internal/agent`; `runtime` groups *Prompt* + *MCP* (+ *Worktree* for file tools) | `internal/agent/schema_test.go` `TestRequestToolSchemasAreValidJSONSchema`; `internal/agent/envelope_test.go` `TestToolSpecsDigestFollowsWhatTheModelSees`; `internal/agent/stepspecs_test.go`; `runtime/mcp_lifecycle_test.go` `TestToolSchemaCost`; race `internal/toolset` (`create_race_test.go`) |
| `provider/**`, `protocol/echo*.go`, `protocol/usage.go` *(candidate)*, cache/effort settings in `config/**` (also the `config` row) | WIRE, REASONING | changed provider pkg, `provider/...`, `protocol`, `internal/agent`; `runtime` group *Provider* | `provider/anthropic/wire_test.go` (`budgetGolden`), `provider/anthropic/engine_test.go` (`fakeAnthropic` SSE server), `provider/openai/*_test.go`, `internal/agent/envelope_test.go`, `runtime/request_envelope_test.go` `TestRealEngineEnvelopesMatchTheRequestsSent`. Load `prompt-tools-cache.md` |
| `internal/window/**`, `internal/taskstate/**`, `internal/session/projection.go`, `internal/agent/{send,envelope,transcript}.go`, `internal/agent/receipt_notes.go` *(candidate)*, `runtime/projection*.go` | CONTEXT, CACHE, REASONING, PERSISTENCE | changed pkg, `internal/session`, `internal/agent`; `runtime` group *Context* | `internal/window/window_test.go` `TestBuildProperties`, `TestE5Regression`; `internal/agent/context_space_stage{2,3}_test.go`; `runtime/context_space_{stage1,t3,t4}_test.go`. Load `context-cognitive.md` |
| `runtime/cognition*.go`, `internal/cognition/**`, `internal/knowledge/**`, `internal/session/run_posture*.go` *(candidate)*, `runtime/governor*.go`, `runtime/experience*.go` | COGNITIVE, LIFECYCLE, PERSISTENCE, CONCURRENCY | changed pkg, `internal/session`, `internal/migrate`; `runtime` group *Cognitive* | `docs/cognitive-core/COGNITIVE_CORE_TEST_PLAN.md` commands; `internal/migrate/cognition_test.go`. Load `context-cognitive.md` (L3) |
| `internal/migrate/**` (incl. `sql/`), store code in `internal/session/**` | PERSISTENCE, REASONING, CONCURRENCY | `internal/migrate`, `internal/session`, `cmd/lcx-go`; `runtime` group *Persistence* | `internal/migrate` in full, normal and `-race`; `internal/session/downgrade_test.go`; `internal/migrate/migrate_test.go` `TestNoReasoningAtRest`. Load `reliability.md` §1 (L3) |
| `runtime/agent.go`, `runtime/lifecycle*.go`, `runtime/delivery*.go`, `runtime/checkpoint*.go`, `internal/agent/{loop,execution,callback}*.go` | LIFECYCLE, CONCURRENCY, PERSISTENCE | `internal/agent`; `runtime` group *Lifecycle* (whole `runtime` when the run state machine itself changes) | `runtime/lifecycle_test.go` `TestCancelDuringShellSleepEndsInterruptedCancelled`; `runtime/checkpoint_resume_test.go` `TestInterruptedRunResumesFromPersistedSafeBoundaryWithoutToolReplay`; `runtime/request_crash_unix_test.go` `TestKilledAroundARequestIsUnknownAndNotResent`. Load `reliability.md` (L3) |
| `internal/shell/**`, `internal/procrt/**`, `internal/jobs/**`, `internal/runlock/**`, `internal/schedule/**`, `internal/cgroupv2/**` | PROCESS, CONCURRENCY, PLATFORM | changed pkg, `internal/toolset`, `internal/mcp`, `evalruntime` when imported; `runtime` group *Process* | `internal/shell/process_crash_unix_test.go`, `internal/jobs/restart_unix_test.go`, `internal/runlock/runlock_kill_unix_test.go`; `-race` on the changed pkg; `*_linux_test.go` → `NOT_RUN(platform)` + cross-compile. Load `reliability.md` (L3) |
| `internal/editing/**` | TRUST, CONCURRENCY, PLATFORM | `internal/editing`, `internal/editing/goedit`, `internal/toolset`; `runtime` group *Worktree* | race: `internal/editing/{concurrency,create_race,xreview_editing_concurrency}_test.go`; fault matrix `internal/editing/r7_create_fault_matrix_test.go` |
| `internal/repository/**`, `runtime/worktree*.go` | TRUST, LIFECYCLE | `internal/repository`, `internal/toolset`; `runtime` group *Worktree* | `runtime/worktree_safety_test.go`; `runtime/deletion_crash_unix_test.go`. Load `reliability.md` §4 |
| `internal/mcp/**`, `runtime/mcp*.go` | LIFECYCLE, CONCURRENCY | `internal/mcp`, `internal/toolset`; `runtime` group *MCP* | `runtime/mcp_lifecycle_test.go` (the test binary is the MCP server via `LCX_TEST_MCP`, modes `crash`/`hang`); `internal/mcp/registry_test.go` `TestAgentToolNameIsBoundedAndDeterministic` |
| `internal/debugger/**`, `runtime/dap*.go` | LIFECYCLE, PLATFORM | `internal/debugger/...`, `internal/toolset`; `runtime` group *Debugger* | fake adapter `internal/debugger/dapfake`; real adapters env-gated (`LC_TEST_LLDB`, `LC_TEST_DELVE`) → `SKIP` unless the change touches them |
| `runtime/hub*.go`, `runtime/subagents*.go`, `runtime/delegation*.go`, `internal/session/hub.go` | LIFECYCLE, CONCURRENCY, TRUST | changed pkg; `runtime` group *Hub* (+ *Lifecycle* when run state changes) | child authority only narrows (`runtime/prompt_test.go` `TestEffectivePromptChildAuthority`); lease/expiry and collaboration probes `runtime/xreview_hub_*_test.go`. Load `reliability.md` §2 |
| `evalruntime/**`, `evalapi/**`, `runtime/eval*.go` | TRUST, PLATFORM, CONTRACT | `evalapi`, `evalruntime` (~2–3 min), `internal/toolset`; `runtime` group *Eval* | Linux isolation tests `NOT_RUN(platform)`; Python/Node workers env-dependent |
| `internal/evaluator/**`, `internal/verify/**`, `runtime/checks*.go`, `runtime/verification*.go` | TRUST, LIFECYCLE | changed pkg, `internal/agent`, `internal/toolset`; `runtime` files `checks_test.go`, `verification_*_test.go` via `run-regex.sh` (they sit in group *Cognitive*) | a run's own writes never satisfy or rewrite its gates (ADR-0017) |
| `config/**`, `docs/lumencortex.schema.json`, `config/presets/*.md` | CONTRACT | imported almost everywhere → full module run | `config` tests incl. `config/testdata/{valid,invalid}` |
| `protocol/**` | CONTRACT, WIRE | imported almost everywhere → full module run | `protocol` tests + provider row |
| `pluginapi/**`, exported `runtime` Engine API, event JSON, CLI output | CONTRACT | callers: `internal/ompsession`, `internal/session`, `cmd/lcx-go`; `runtime` group of the API | contract tests and callers; record Desktop impact (verified in the Desktop repo, not here) |
| `cmd/lcx-go/**`, `cmd/**` | CONTRACT | changed cmd pkg | CLI tests build nested binaries: `GOFLAGS` must be unset |
| `*_test.go`, `testdata/**`, test helpers only | TEST | changed pkg; a changed shared helper → every package that uses it | L1 per changed test + Test changes table (SKILL.md §5) |
| `go.mod`, `go.sum` | WIDE | full module run | `go mod verify` |
| `scripts/**`, `.github/workflows/**` | CI | — | `bash -n`; `scripts/release-check.sh --mode local --allow-dirty --skip-tests`; hosted CI `NOT_RUN(hosted)` unless an authorized push runs it |
| `src/**`, `tests/**`, `package*.json` (Node reference) | reference | — | `npm test`; historical reference only, not Go acceptance (`docs/validation.md`) |
| `benchmarks/**`, workspace `bench/**` | harness | — | the harness's own offline tests; no Docker, trial or model request without Owner authorization |
| Core `bench/evidence/**` (committed review evidence; own `go.mod`, so outside `./...`) | evidence | — | sealed: never edit; its probe `_test.go` files are history, not part of any gate |
| `docs/**` (not embedded), `*.md`, `.agents/skills/**` | docs | — | L0: links resolve; English/`zh-CN` pairs updated together where a pair exists; for this skill: `bash -n` + `--check` of its scripts |

## 3. `runtime` test groups

`runtime` has 667 top-level tests that run serially: 634 s on an idle-ish host (`main` `b21049c`, load 11–17, 2026-11-28), up to ~1,560 s under heavy load (Fable measurement). For L2 run the group(s) of the change; run the whole package when the row says so, for L4, and for the hand-off gate.

Groups are defined **by test file** in `scripts/runtime-group.sh` (file stems follow the production files: a change to `runtime/worktree.go` → `Worktree`). Name-prefix regexes were tried and rejected: they left 327 of 667 tests (49 %) in no group.

```bash
S=.agents/skills/lc-testing/scripts
$S/runtime-group.sh --list                     # groups and file globs
$S/runtime-group.sh --check                    # every runtime test file is in a group? (run when adding a test file)
env -u GOFLAGS go test -list "$($S/runtime-group.sh Worktree)" ./runtime | grep -c '^Test'   # probe the selection
env -u GOFLAGS go test -count=1 -timeout=20m -run "$($S/runtime-group.sh Worktree Persistence)" ./runtime
$S/run-regex.sh runtime/foo_test.go runtime/bar_test.go   # exact tests of chosen files, any package
```

Measured on `b21049c` (serial durations sum; groups are disjoint; together they cover all tests except `TestHelper*` process entry points and the `LCX_T1_PERF` measurement):

| Group | Tests | Time | Group | Tests | Time |
|---|---|---|---|---|---|
| Prompt | 41 | 8 s | Process | 29 | 68 s |
| Provider | 29 | 4 s | Worktree | 29 | 29 s |
| Context | 17 | 13 s | MCP | 38 | 17 s |
| Cognitive | 184 | 383 s | Debugger | 33 | 7 s |
| Persistence | 48 | 14 s | Eval | 52 | 10 s |
| Lifecycle | 98 | 61 s | Hub | 60 | 22 s |

`Cognitive` is 60 % of the package; when a Cognitive change is confined to one area, start with the matching files via `run-regex.sh` and run the whole group before hand-off. Groups are a selection aid, not a coverage proof: when the change's own tests live in another file, add that file. Whole-package coverage comes only from L4 or the hand-off gate.

## 4. Counting results from `go test -json`

```bash
env -u GOFLAGS go test -count=1 -timeout=45m -json ./... > go-test.jsonl 2> go-test.stderr; echo "exit=$?" > go-test.exit
# top-level tests vs subtests by outcome
jq -r 'select(.Test != null and (.Action=="pass" or .Action=="fail" or .Action=="skip"))
  | [(if (.Test|contains("/")) then "subtest" else "top" end), .Action] | @tsv' go-test.jsonl | sort | uniq -c
# package outcomes with seconds; build failures have no test events
jq -r 'select(.Test == null and (.Action=="pass" or .Action=="fail" or .Action=="skip")) | [.Action, .Package, (.Elapsed // "")] | @tsv' go-test.jsonl | sort -k3 -nr
jq -r 'select(.Action=="build-fail") | .ImportPath' go-test.jsonl
# slowest tests
jq -r 'select(.Test != null and (.Test|contains("/")|not) and .Action=="pass") | [.Elapsed, .Package, .Test] | @tsv' go-test.jsonl | sort -nr | head -20
# skip reasons
jq -rs '[.[] | select(.Test != null)] | group_by([.Package, .Test]) | map(select(any(.[]; .Action == "skip")))
  | .[] | "\(.[0].Package) \(.[0].Test): " + ([.[] | select(.Action == "output") | .Output
  | select(test("^(=== |--- SKIP)") | not)] | join("") | gsub("\\s+"; " "))' go-test.jsonl
```

Report the exit code, package pass/fail/skip, top-level and subtest counts, build failures, skips with reasons and each anchor's outcome. A `[no test files]` package is listed, not counted as PASS. Exit 0 with any `fail` event, or non-zero exit with no `fail` event, is `UNKNOWN` until explained.
