---
name: lc-testing
description: Test selection, execution and evidence rules for LumenCortex Core. Use before changing, reviewing or accepting Core Go code, tests, testdata, SQLite schema/migrations, prompts, tool schemas, provider wire or prompt-cache code, Context Space, Cognitive Core, runtime lifecycle/concurrency/recovery, or Core CI/test scripts, and before claiming a Core change is fixed, verified or ready for review. Picks the smallest sufficient test layer (L0–L4), names the invariants that must not be skipped, and defines PASS/FAIL/BLOCKED/TIMEOUT/NOT_RUN reporting. Not for Desktop, and never authorizes paid model requests or benchmarks.
---

# lc-testing — choose, run and report LumenCortex Core tests

Version `1.0.0-rc1` (2026-11-28). Not frozen: independent review and Dry Run pending (`CORE_PREPARATION_TODO.md` Phase T2).

This skill decides **which tests a Core change needs, how to run them, and what you may claim**. It adds to Core `AGENTS.md` and the workspace `../AGENTS.md`; where they conflict, they win. It authorizes nothing: no paid model request, benchmark/TB2 trial, push, merge, tag or release.

Reuse what exists: Go `testing`, the in-process scripted providers, `httptest` SSE fakes, `t.TempDir()`, helper-process crash tests, `scripts/release-check.sh`. Do not build a new framework to satisfy this skill.

## 1. The loop

```text
Understand → name invariants → select layer → baseline (when needed) → implement
→ focused tests → diagnose failures → fix root cause → widen only with a reason → verify → report
```

1. **Understand** the change: changed paths, exported symbols, state machines, persisted or wire formats. `scripts/affected.sh [BASE [TARGET]]` (this skill's directory) lists changed packages, their in-module importers, risk flags and suggested `runtime` groups; with `TARGET` it plans a committed range (a review) without checking it out.
2. **Name the invariants** the change can break (section 4). Every one needs a test that would fail if it broke.
3. **Select** the lowest layer that covers the invariants (sections 2–3). When unsure between two layers, take the higher one and say why.
4. **Baseline** only when you need to separate old failures from new ones: run the same planned command on the unmodified base and label it `BASELINE`.
5. **Bug or Finding fix: red first.** Write the regression test, run it on the unfixed code, keep the failing output. It must fail on the asserted behavior, not on a compile error or an unrelated panic. Without a checkout of the old code, `go test -overlay` can swap in the pre-fix file (or, in a review, a one-line mutant of the guard under test); the overlay JSON and file are evidence. A mutant that survives the selected tests is a coverage gap: record it, do not call the guard tested.
6. **Iterate on L1.** Fix the root cause; rerun the identical command.
7. **On failure, diagnose before rerunning** (section 6). A rerun needs a recorded reason.
8. **Widen** to L2/L3 only when the change or a failure justifies it; record the reason.
9. **Verify once** on the final tree, then **report** (section 8).

Before any run expected to exceed ~2 minutes, run a **probe** first: the single most relevant test (`-run '^TestX$'`), or `go test -list . ./pkg` to confirm the filter matches. Get a working version early; do not polish untested code.

## 2. Layers

| Layer | When | What runs | Typical cost |
|---|---|---|---|
| **L0 Preflight** | start of every task | worktree/branch/`HEAD`/`git status`; `go version`, `go env CGO_ENABLED` (must be `1`); `scripts/affected.sh`; known failures in the task brief/TODO; risk flags (SQLite, provider, concurrency, permissions, processes, worktrees). No test run; no whole-repo scan | seconds |
| **L1 Focused** | default for every code change | `gofmt -l` on changed files; `go vet` + `go test -run '<related tests>'` on the changed package(s); the red→green pair | seconds–2 min |
| **L2 Package / Integration** | module-level or cross-package change, or any row of section 3 that says L2 | full changed packages plus direct in-module importers that exercise the change; `runtime` only as the matching test group (`scripts/runtime-group.sh`); the named contract/anchor tests of the area | 1–15 min (whole `runtime` ~11 min idle) |
| **L3 Reliability** | runtime lifecycle, permissions, concurrency, persistence/transactions, processes, worktrees, recovery | L2 plus `-race` on the affected packages (or a `-run` subset of `runtime`), deterministic fault injection, crash/restart helper processes, cancellation/timeout boundaries | +2–20 min |
| **L4 Candidate / Release** | candidate acceptance, baseline integration, schema-migration release, version release | the full applicable matrix in `references/acceptance.md`: full module build/vet/test, race sets, `scripts/release-check.sh`, clean tree, independent re-execution | ~1–1.5 h |
| **BENCH** (separate) | only with an explicit Owner authorization naming the run | TB2, SWE-bench, real-model trials under the frozen benchmark rules | paid |

**Hand-off gate (current rule).** Core `AGENTS.md` "Required validation" currently requires `go build ./... && go vet ./... && go test -count=1 ./...` for affected Go code. Run it **once**, on the final tree, before handing off a Go change; never per iteration. It does not apply to changes that touch no Go build or test input (docs, reports, this skill). A proposal to replace it with layer-based selection is pending planner decision (LT-D1 in `docs/reports/lc-testing-v1/LC_TESTING_DESIGN.md`); until it is decided, AGENTS.md wins.

BENCH never substitutes for L1–L4, and L1–L4 never send real model requests: do not set `LCX_REAL_MODEL`, do not build with `-tags realmodel`, do not start Harbor/TB2.

## 3. Risk selection — minimum scope by change type

Upgrade when the risk is higher than the row suggests; never go below the row for the invariants the change touches. Path-level detail and anchor tests: `references/test-matrix.md`.

| Change | Minimum | Load |
|---|---|---|
| Docs, reports, ADR text (not embedded) | L0 + link/format check; no Go tests. `docs/lumencortex.schema.json` is embedded: treat as `config` code | — |
| Small pure function | L1: the function's tests + a new case for the bug | — |
| Logic inside one module | L1 → L2 for that package | test-matrix |
| Exported API (`runtime`, `protocol`, `pluginapi`, `evalapi`, event JSON, CLI output) | L2: contract tests + callers; note Desktop impact | test-matrix |
| Provider adapter / wire | L2: fake provider + `httptest` SSE request/response contract, error classes, usage buckets | prompt-tools-cache |
| Prompt text / tools / toolset | L2: prompt invariants, tool definitions, request serialization | prompt-tools-cache |
| Prompt cache / request layout | L2 + stable-prefix and receipt-projection regressions | prompt-tools-cache |
| Context Space (window, projection, Task State, compaction) | L2: lifecycle, isolation, prefix stability, no hidden reasoning | context-cognitive |
| Cognitive Core (posture, admission, settlement, knowledge) | L3: persistence, permission-only-narrows, state transitions, restart | context-cognitive |
| SQLite schema / store / migration | L3: upgrade from fixtures, newer-schema refusal, atomic failure, close/reopen, data preserved | reliability |
| Concurrency / cancellation | L3: `-race`, cancel at each boundary, terminal state | reliability |
| Background processes / jobs / shell | L3: exit, timeout, orphan/process-group cleanup, resource release | reliability |
| Worktree / repository services | L3: conflict, lock, apply/remove, crash at boundary | reliability |
| `config/**`, `protocol/**`, `go.mod`/`go.sum` (imported almost everywhere) | full module run (same as hand-off gate) | test-matrix |
| Test-only change | L1 for each changed test + "Test changes" table (section 5) | — |
| CI workflow / scripts | `bash -n`; `scripts/release-check.sh --mode local --allow-dirty --skip-tests`; hosted CI is `NOT_RUN(hosted)` unless a push runs it | test-matrix |
| Core candidate / release | L4 | acceptance |

## 4. Invariants that are never optional

When the change touches the area, the matching invariant is tested at the stated layer even if the task is "small":

- **Hidden reasoning** never reaches durable or recovery state (persistence, compaction, handoff, context construction, recovery).
- **Permissions only narrow** on resume, restart, delegation or retry; a revoked permission stays revoked.
- **Terminal state** is defined for every exit path (success, error, cancel, deadline, crash); a partially finalized run is never recoverable as success.
- **No replayed effects**: resume/retry does not re-run a tool call, check or settlement that already committed.
- **Transactions are atomic**: a failure inside leaves nothing partial; claims of durability are proven by close/reopen.
- **Path/trust confinement** fails closed (workspace trust, path traversal, symlinks, shell, sandbox).
- **No secrets** in logs, fixtures, evidence or committed testdata.
- **Tests are hermetic**: `t.TempDir()`/`t.Setenv` for HOME and state; no real home, other worktree or network.

## 5. Integrity — forbidden

To get a green result you MUST NOT: delete, comment out or weaken an assertion; widen a tolerance or change an expected value without a specification reason; add `t.Skip`, a build tag, `testing.Short()` or an env gate to dodge a failure; shrink inputs/iterations/concurrency so a defect no longer triggers; add sleeps or retries that hide a race; hand-edit or regenerate goldens without a reviewed diff; narrow `-run`/package lists and call it "full"; rerun until green and report only the green run; use hidden tests, answer keys, verifier internals or hard-coded answers; overwrite or delete earlier evidence; report a number you did not produce.

Changing an existing test is allowed only when the specified behavior changed. List each in a **Test changes** table (old → new assertion, reason, spec/ADR); the reviewer accepts each row. Production defects are fixed in production code, not in test expectations.

## 6. Running, timeouts and failures

- Run tests in the worktree that holds the change. Never in another agent's or a dirty worktree you do not own. On a shared host run one heavy suite at a time.
- Clear inherited flags: `env -u GOFLAGS go test …` (an inherited `GOFLAGS=-json` broke nested `go` calls in `cmd/lcx-go` tests).
- Iteration may use the Go test cache. **Evidence runs use `-count=1`**; a `(cached)` line is not a fresh result.
- Always pass `-timeout`; it applies **per package binary**. Budgets and measured package durations: `references/reliability.md` §5. Default classes: focused `-timeout=5m`; one ordinary package `10m`; any run including `runtime` `30m`; `-race` including `runtime` `45m`.
- Long runs: write `-json` to a file, keep stderr separately, record the exit code: `go test -count=1 -timeout=30m -json ./runtime > run.jsonl 2> run.stderr; echo "exit=$?"`.
- **Timeout ≠ deadlock ≠ slow.** On a timeout keep the panic goroutine dump. Decide from evidence: per-test `Elapsed` progressing in `-json` = slow; goroutines parked for minutes on the same channel/lock/`Wait` = hang. Raise a budget only with per-test duration evidence and say so; never to hide a hang.
- **Failure triage:** read the first failure, not the last. Classify `REGRESSION` (fails on your tree, passes on base), `PRE_EXISTING` (fails on base too; cite it), `ENVIRONMENT` (missing tool, cgo, disk), or `FLAKY_SUSPECTED` (fails then passes with no change: record both runs, open a Finding, not a PASS). One targeted rerun with a written reason is allowed; mechanical reruns are not.

## 7. Result vocabulary

Per command or test — only `PASS` satisfies a gate:

| Status | Meaning |
|---|---|
| `PASS` | ran on the stated tree, exit 0, no `fail` events |
| `FAIL` | ran and failed (assertion, panic, build failure) |
| `TIMEOUT` | killed by `-timeout`/job limit; goroutine dump kept; counts as a failure until diagnosed |
| `BLOCKED` | cannot run here (exact error: missing tool, platform, authorization) |
| `NOT_RUN` | deliberately not run, with reason (`platform`, `authorization`, `out-of-scope`, `hosted`) |
| `SKIP` | ran and skipped itself; not coverage. List skip reasons |
| `UNKNOWN` | no parseable result (exit 0 with a `fail` event, or the reverse) |

Per change, claim only what evidence supports: `IMPLEMENTED` → `SELF_VERIFIED` (required layers PASS, evidence stored) → `READY_FOR_REVIEW` (committed or patch hash recorded, report written) → `INDEPENDENTLY_VERIFIED` (a non-author re-ran the checks on the frozen SHA). The implementer never grants their own independent PASS; the finder of a Finding owns its fix (workspace `AGENTS.md`).

## 8. Report

Every Core change report includes, briefly:

```text
Tree: <worktree> @ <HEAD> (+ dirty diff sha256 if any)   Go: <version> darwin/arm64 cgo=1
Layer: L<n> — why (risk flags / rows of §3)
Invariants: <list> → <test names>
Commands: <exact command> → <status> (<duration>, exit <n>, counts pass/fail/skip)
Red/green: <test> red on <base>, green on <tree>     (bug/Finding fixes)
Not run: <item> NOT_RUN(<reason>) / BLOCKED(<error>)
Pre-existing failures: <test> PRE_EXISTING (base evidence)
Test changes: <table or "none">
```

Raw logs go to the task's evidence directory (workspace `bench/evidence/<task>-<date>/`, outside Git: say so) or the committed report. Never store credentials, secret-bearing output or hidden model reasoning.

## 9. Platform

macOS Apple Silicon (`darwin/arm64`, cgo on) is the native gate. Linux-only files (`//go:build linux`: namespaces, cgroup v2, bubblewrap, nftables) are `NOT_RUN(platform)` on macOS, never PASS; when they change, add compile-only checks (`references/reliability.md` §6). Linux Sandbox CI is deferred and does not block macOS work. Hosted GitHub CI (`go-runtime.yml`) is a separate signal: investigate its real failures from logs, do not count it as the macOS gate, do not hide it.

## 10. References — load only when the plan needs them

| File | Load when |
|---|---|
| `references/test-matrix.md` | writing a Diff-to-Test plan, any L2+ change, exported API, `config`/`protocol`, CI/scripts |
| `references/prompt-tools-cache.md` | prompt text, tool names/schemas/order, toolset, provider wire, cache markers, usage accounting, request layout |
| `references/context-cognitive.md` | Context Space (window, projection, Task State, receipts) or Cognitive Core (posture, admission, settlement, knowledge) |
| `references/reliability.md` | SQLite/migrations, lifecycle, cancellation, concurrency/race, crash/fault injection, processes, worktrees, timeouts |
| `references/acceptance.md` | L4: candidate acceptance, merge to `main`, release, independent review, Finding closure |
| `scripts/affected.sh` | L0 of any change with Go files: changed packages, importers, risk flags |
| `scripts/runtime-group.sh`, `scripts/run-regex.sh` | L2 selection inside `runtime` (by test file) or any package |
