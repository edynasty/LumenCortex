# Reliability: SQLite, lifecycle, concurrency, processes, worktrees, timeouts

Load this for L3: SQLite store/schema/migrations, run lifecycle, cancellation, concurrency, crash recovery, background processes, worktrees, timeouts, and platform-specific code. Prefer deterministic seams and real local processes over sleeps and mocks.

## 1. SQLite and migrations

Real SQLite only (`mattn/go-sqlite3`, cgo). Every test uses its own `t.TempDir()` state directory; never the user's `~/.lumencortex` or another worktree's database.

| Invariant | Anchor (in `internal/migrate` unless noted) |
|---|---|
| Upgrade from every shipped version fixture reaches the latest schema | `TestUpgradeFixtures`, `TestUpgradeEveryVersionToV6`, `TestUpgradeFixturesReachLatestWithNullModelRef` (fixtures: `testdata/*.sql`, `TestEmitVersionFixtures` regenerates only with `LCX_EMIT_FIXTURES` and a reviewed diff) |
| Schema from the future is refused without writing | `TestSchemaTooNewWritesNothing`, `TestLegacyNodeDatabaseIsRefusedUntouched`, `TestRefusalLeavesHotWALUntouched` |
| Migration step and version bump are atomic; failure rolls back | `TestMigrationAndVersionAreAtomic`, `TestCancelRollsBackMigrationInProgress`, `TestFailedModelRefMigrationRollsBackAndRecovers`, `Test*MigrationFailureRollsBack` |
| Reopen performs no migration; concurrent opens agree | `TestReopenPerformsNoMigration`, `TestConcurrentOpen`, `TestConcurrentProcessOpen` (helper process) |
| Downgrade refuses semantic or privacy loss | `TestDowngradeMechanism`, `TestCognitiveGraphDowngradeRefusesKnowledgeAndPrivacyLoss`, `TestDowngradeBelowSixIsRefusedWhileAFenceIsLive`; `internal/session/downgrade_test.go` |
| Crash after commit / during backup recovers; no reasoning survives | `TestCrashAfterCommitIsRecovered`, `TestBackupPartialCleanup`, `TestScrubCrashLeavesNoReasoning`, `TestReasoningInLegacyWALIsTruncated`, `TestNoReasoningAtRest` |
| Locking: shared vs exclusive, bounded wait | `TestSharedHolderBlocksUpgrade`, `TestWaitHonorsContext`, `TestLockWaitIsThirtySeconds` (`LockWait` = 30 s; a test that holds a shared store and then needs the exclusive lock waits the full 30 s — close the store first) |
| Store invariants (session) | `internal/session` package in full; for durability claims, close and reopen the store and assert from the reopened handle |

For a new migration: add the SQL pair, a round-trip test, a failure-rolls-back test, a downgrade decision (refuse or lossless), a fixture for the previous version when the shape changed, and run `internal/migrate`, `internal/session`, `cmd/lcx-go` and `runtime` group *Persistence* with `-race` on `internal/migrate` and `internal/session`.

Fault injection: use the package's existing seams (cancelled `context`, failing statement via a trigger or a constraint in the test DB, helper-process kill). Do not add production hooks only for a test without stating it in the report.

## 2. Lifecycle, cancellation and concurrency

- **Cancel at each boundary** the change touches: before send (`internal/agent/envelope_test.go` `TestCancelledBeforeSendRecordsNothing`), after admission (`internal/agent/lifecycle_control_test.go` `TestCancellationAfterAdmissionSendsNoRequest`), during backoff (`internal/agent/recovery_test.go` `TestCancelDuringBackoffEndsTheRun`), during a tool (`internal/agent/execution_test.go` `TestCancelledToolLeavesTheExecutionAdmitted`), during shell (`runtime/lifecycle_test.go` `TestCancelDuringShellSleepEndsInterruptedCancelled`), mid-stream (`provider/anthropic/errors_test.go` `TestCancellationMidStream`).
- **Terminal state for every exit path** and run-lock release: `runtime/runlock_test.go` `TestRunLockIsReleasedOnEveryExitPath`, `TestOpenReconcilesARunningSessionWhoseLockIsFree`, `TestCancelDuringToolThenStartWithInputLeavesAPairedTranscript`.
- **No replayed effects after a crash**: helper-process tests that kill the Engine at a named point — `runtime/request_crash_unix_test.go` `TestKilledAroundARequestIsUnknownAndNotResent`, `runtime/execution_crash_unix_test.go` `TestKilledAroundAToolIsUnknownAndNotReplayed`, `runtime/crash_combined_unix_test.go` `TestKilledAroundTheRequestAfterATool`, `runtime/checkpoint_resume_test.go` `TestInterruptedRunResumesFromPersistedSafeBoundaryWithoutToolReplay`. Pattern: the test binary re-executes itself (`os.Args[0]`, `-test.run=TestHelper…`), the child stops at a crash point, the parent `SIGKILL`s it and reopens the state.
- **Duplicate/repeated requests**: a second Start on a held run is `ErrRunHeld` (`TestSecondStartOnTheSameSupervisorIsErrRunHeld`, `TestEngineCannotRecoverOrClobberARunHeldElsewhere`); retries/failover assert attempt counts and that unknown outcomes are not resent.
- **Permissions on resume/retry/revocation only narrow**; see `context-cognitive.md` §2 and `runtime` group *Lifecycle* `Revoke` tests.
- **Race detector**: `-race` on every changed package with shared state, plus a `-run` subset of `runtime` for the changed group. Synchronize with channels, `sync.WaitGroup`, barriers or explicit hooks; a `time.Sleep` that makes a test pass is a defect. A test that fails once under load and passes alone is `FLAKY_SUSPECTED`: record both runs and open a Finding.
- Concurrency tests run the contended section many times in one test (the existing `*_race_test.go` files are the pattern), not the whole test many times with `-count=N` as the only evidence.

## 3. Background processes, jobs and shell

| Invariant | Anchor |
|---|---|
| Timeout and cancel terminate the whole process group | `internal/shell/process_test.go` `TestTimeoutTerminatesTheProcessGroup`, `TestCancellationKillsTheProcessGroup`; `internal/jobs/jobs_test.go` `TestCancelQueuedAndRunningKillsProcessGroup`, `TestCancelDuringClaimWindowKillsTheCommand`. Known gaps (lc-testing V1 validation S5): **LT-G1** no committed test has a group member that ignores SIGTERM after the shell exits on a *cancel*; dropping `ctx.Err() != nil` from the reap condition in `internal/shell/runner.go` survives `internal/shell`, `internal/toolset`, `internal/agent` and `runtime` `bench003_timeout_test.go`, `lifecycle*_test.go` and group *Process*. **LT-G4** `TestCallerCancellationGraceAndNoStartAfterCancellation` misses removal of the "cancelled before start" check (0/50): SIGTERM wins the race against the marker command. When you touch cancel cleanup, close the gap you touch |
| Owner death leaves no orphan and restart reconciles | `internal/jobs/restart_unix_test.go` `TestRestartAfterOwnerDeath`; `internal/shell/process_crash_unix_test.go`; `internal/runlock/runlock_kill_unix_test.go` `TestCrossProcessKill` |
| Cancel waits for every stopped job; resources released | `TestCancelRunWaitsForEveryStoppedJob`; check file descriptors / temp dirs are gone after the test |
| Interrupt escalation in eval workers | `evalruntime/lifecycle_remediation_test.go` `TestInterruptEscalationStopsAnIgnoringCell` (needs Python/Node; missing interpreter → `SKIP`/`BLOCKED`, not PASS) |
| MCP server crash/hang | `runtime/mcp_lifecycle_test.go` (test binary as MCP server, `LCX_TEST_MCP_MODE=crash|hang`) |

Tests that start processes must kill them in `t.Cleanup` and assert they are gone. Never leave a child running after the test binary exits; after a `TIMEOUT`, check for leftover processes started by the test (`pgrep -f <test binary name>`) and report them.

## 4. Worktrees and repository services

Each test creates its own repository under `t.TempDir()` (`git init`); never point a test at the developer's checkout or `wt/`.

Anchors: `runtime/worktree_safety_test.go` (`TestWorktreeOfARunningSessionCannotBeRemoved`, `TestBorrowerCannotRemoveTheOwnersWorktree`, `TestWorktreeCleanupMatrix`, `TestDeletionRetryAfterPartialCleanup`), `runtime/worktree_conflicts_test.go`, `runtime/worktree_handoff_test.go` (dirty target and overlapping files block), `runtime/deletion_crash_unix_test.go` (kill during deletion, finished at next open), `internal/repository/*_test.go` (path traversal, symlinks, cancellation, bounded output).

## 5. Timeouts and durations

Six different clocks; do not confuse them:

| Clock | Where set | Meaning when it fires |
|---|---|---|
| Single test | measured `Elapsed` in `-json` | slow test, not a failure by itself |
| Package binary | `go test -timeout` (default 10 m) — applies to the **whole package binary**, not per test | panic + goroutine dump; `TIMEOUT` |
| Agent run | `config` step/deadline settings, `internal/agent` deadlines | product behavior under test |
| External process | shell/job timeouts (`internal/shell`, `internal/jobs`) | product behavior under test |
| Provider idle | `config.DefaultIdleTimeout` (120 s) | product behavior; tests use `httptest` with explicit short values |
| CI job | `.github/workflows/*.yml` (`go-runtime.yml` sets no `timeout-minutes` and no `-timeout` today) | hosted signal, separate from the macOS gate. Last `main` run (`b21049c`, run 45318690704) is red on Linux: `internal/editing` `TestR201TwoOSProcessesCreateRace/…/before_commit` got `unsupported_filesystem` (2 hard links), not a timeout; the same tree passed on its PR run, so it is `FLAKY_SUSPECTED` on Linux (LT-G2), not a macOS failure |

Measured package durations (`go test -count=1 -json ./...` on `main` `b21049c`, darwin/arm64, load 11–17, 2026-11-28, 661 s wall; earlier Fable measurements on `c6324ab`/`5925f5b` for range):

| Package | Tests | Seconds (this run) | Historical range | Suggested `-timeout` |
|---|---|---|---|---|
| `runtime` | 667 | 634 | 699–1,556 | `30m` (`45m` with `-race`) |
| `evalruntime` | ~135 | 105 | 102–182 | `10m` |
| `internal/session` | ~140 | 41 (30.7 of it is `TestDowngradeEveryBoundary`: it keeps a store open (`downgrade_test.go:443-447`) and expects `Downgrade(…, 0)` to refuse, which it does only after the 30 s `LockWait`; known, fix scheduled in master plan E1) | 38–42 | `5m` |
| `internal/toolset`, `internal/agent`, `internal/jobs`, `internal/migrate`, `internal/evaluator` | — | 18–35 each | 17–26 | `5m` |
| every other package | — | < 11 each | — | `2m` (default `10m` is fine) |

Slowest single tests in this run: `runtime` `TestCrossProcessBusyIsAStorageFailure` 39.0 s, `internal/session` `TestDowngradeEveryBoundary` 30.7 s, `runtime` `TestCognitionCheckFailureAndRealRevalidationDeadline` 19.7 s; all others < 13 s.

Rules:

- Pass an explicit `-timeout` sized from the table plus margin for load; record machine load (`uptime`) with every long run. Under heavy load, durations double (runtime 699 s idle vs 1,556 s loaded in the Fable measurement).
- **Slow vs hung.** In `-json`, test `Elapsed` values keep arriving = slow. In the timeout dump, find the goroutine of the running test: blocked for "N minutes" on the same channel/mutex/`Wait`/lock file = hung; runnable or in syscalls (SQLite, fsync, exec) = slow. One sample does not prove absence of deadlock; rerun only the single test, alone, with `-v -timeout` sized to its measured duration × 3, and keep both outputs.
- Historical example: an `internal/agent` "15 s timeout" (2026-11) was a caller-set `-timeout=15s` on the whole package binary; the running test had used ~2 s and was runnable in SQLite prepare — slow budget, no deadlock evidence (`archives/git-cleanup-20261127/worktree-commit-closure/finalization/TEST_TIMEOUT_INVESTIGATION.md`).
- Never raise a timeout to make a hang pass. Raise it only with per-test duration evidence, and say so in the report.
- On a shared host run one heavy suite (`runtime`, full module, race) at a time; parallel worktree suites have produced false timeouts (load 60+).

## 6. Platform

- Native gate: `darwin/arm64`, `CGO_ENABLED=1`.
- `//go:build linux` files exist in `internal/shell`, `internal/cgroupv2`, `internal/editing`, `internal/jobs`, `internal/mcp`, `evalruntime`, `cmd/lcx-broker` (e.g. `internal/shell/containment_linux_test.go`). They do not run on macOS: `NOT_RUN(platform)`. When they change, add compile-only checks for the changed packages and label them `COMPILE_ONLY`, never runtime PASS:

```bash
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 env -u GOFLAGS go vet ./internal/shell ./internal/cgroupv2   # the changed packages
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 env -u GOFLAGS go test -c -o /dev/null ./internal/shell
```

  With `CGO_ENABLED=0` the SQLite driver compiles as a stub, so a cross-compile proves nothing about storage behavior.
- Env-gated tests (`LC_TEST_LLDB`, `LC_TEST_DELVE`, `LC_TEST_SANDBOX_BWRAP`, `LCX_TEST_PYTHON`, `LC_TEST_NODE`, …) `SKIP` without their tool. Report the skip; set the variable only when the change touches that integration and the tool is installed.
