# R1a integration report (T0-S1, T1, B0+R1, T2, T3-S1..S3, T4) - input to G-R1a

- **Date:** 2026-10-26
- **Branch:** `cog/r1a` at `b60ae09` (this report is the next commit); base `main` = `42ee234` (unchanged).
- **Not included:** `cog/t3-s4` (`9674321`, privacy deletion) stays a separate branch until O-3; it was gated and
  mutation-tested at its own SHA (§4).
- **Status:** all local technical checks below pass. G-R1a itself is **not passed**: it also needs T0-S1 merged
  into `main`, O-4 approved, and an Opus gate review in a fresh context (plan §8.2, §G-R1a). Nothing pushed, tagged
  or merged into `main`.

## 1. Slices on `cog/r1a`

| Slice | Commits | Report |
|---|---|---|
| T0-S1 | `0fab51a`..`32e3521`, `c08f5c4` | `t0-s1-state-20261025.md` |
| T1-S1..S7 | `47ff0c4`..`4c26a4b` | `t1-s1-schema`, `t1-s2`..`t1-s7`, `t1-package-b`, `t1-package-c` |
| B0 + R1 | `d667c00`..`c7bcd3c` | commit messages; ADR-0018 |
| T2-S1, T2-S2 | `f3167e2`..`c4faa53` | `t2-s1-20261025.md`, `t2-s2-20261025.md` |
| T3-S1..S3 | `b9ca6ff`..`9974f58` | `t3-s1-20261025.md`, `t3-s2-20261026.md`, `t3-s3-20261026.md` |
| T4-S1..S3 | `a79a43b`..`b60ae09` | `t4-s1-20261026.md`, `t4-s2-20261026.md`, `t4-s3-20261026.md` |

## 2. Gate run (exact SHA, clean worktree, private HOME)

Detached worktree at `b60ae09`, `git status --porcelain` empty, `HOME`/`XDG_*`/`TMPDIR` private, `GOPROXY=off`.

| # | Command | Exit |
|---|---|---|
| 1 | `go build ./...` | 0 |
| 2 | `go vet ./...` | 0 |
| 3 | `go test -count=1 ./...` (54 `ok` lines over both test runs; runtime 146 s) | 0 |
| 4 | `go test -count=1 -race ./...` (runtime 182 s) | 0 |
| 5 | `GOOS=windows go build ./...` | 0 |
| 6 | `GOOS=windows go vet ./...` | 0 |
| 7 | `go vet -tags realmodel ./internal/realmodel` | 0 |

Cross-process tests (`runtime/*_unix_test.go`: cross-process fence/revocation linearization, run lock, request and
execution crash) run inside 3 and 4. The private HOME held only Go telemetry and LumenCortex lock directories
afterwards, no state database. Log: workspace `wt/lcgate/full-r1a-final.log` (outside Git).

## 3. ROADMAP §3 items -> named tests

| Item | Tests |
|---|---|
| 1 uncited agent note unverified; user rewrite not laundered | `internal/threadctx` `TestApplyDecidesApplicabilityAndWithholdsText`; `runtime` `TestNoteApplicabilityFollowsSourcesAndFences`, `TestRealEngineUserRewriteDoesNotLaunderAgentText`; `internal/taskstate` `TestUserRewriteOfAgentTextNeedsReview` |
| 2 revoked sources filtered from every carrier | `internal/session` `TestSourceStatusIsWithdrawnByAFenceOnTheSessionOrAnySourceOnTheChain`, `TestProjectionWithholdsAReadOfAReachedSession`; `runtime` `TestStartProjectsAThreadThatReadARevokedSource`, `TestReadExitsAfterRevokingASubagent` |
| 3 envelope = request sent; unknown never answered; no echo bytes | `internal/agent` `TestEnvelopeDescribesTheRequestThatWasSent`, `TestEveryAttemptHasItsOwnEnvelope`, `TestAnswerNotStoredLeavesTheRequestUnknown`; `runtime` `TestHiddenReasoningNeverReachesTheDatabaseThroughEnvelopes`, `TestKilledAroundARequestIsUnknownAndNotResent` |
| 4 no new admission after the linearization point, across Engines; judge/Governor covered | `internal/session` `TestAdmissionOnAFencedSessionIsRefusedAndRecorded`; `internal/agent` `TestFencedAtAdmissionSendsNothing`; `runtime` `TestCrossProcessRevocationIsLinearizedWithAdmission`, `TestCrossProcessFenceIsLinearizedWithAdmission`, `TestRevokedSessionDoesNotCallTheAutoJudge`; `cmd/lcx-go` `TestGovernorCuratorFailsClosedWithoutAModelCall` |
| 5 constraint only via approved binding + fresh check or deviation; set shrink/revert/old acceptance do not pass | `runtime` `TestConstraintIsSatisfiedOnlyThroughAnApprovedBinding`, `TestRewritingTheBoundCheckCannotSatisfyTheConstraint`, `TestRunThatRewritesItsCheckCannotPassIt`, `TestShrinkingTheCheckSetLeavesItUnapproved`, `TestWriteThenRevertKeepsTheRevocation`, `TestOldHumanAcceptanceDoesNotSurviveReapproval`, `TestWorkspaceTrustIsNotApproval`, `TestRestoringAnOldDigestDoesNotReviveItsEpoch` |
| 6 crash after admission is unknown, not replayed; changed episode passes nothing; no cross-run reuse | `runtime` `TestKilledAroundAToolIsUnknownAndNotReplayed`, `TestCheckThatRedefinesItselfOutsideTheWorkspacePassesNothing`, `TestRuntimePassIsDecidedAgainInTheNextRun`, `TestConstraintIsDecidedFreshByEveryStart`; `internal/agent` `TestUnavailablePreStateStillRunsAndIsIncomplete`; `internal/evaluator` `TestEpisodeSpecChangedDuringTheRun`, `TestEpisodeScopeChangedAndIgnoredOutput`; `internal/session` `TestRewriteOfAnUnrecordedEntryIsIncompleteForever` |

## 4. Mutation replay (plan §8.3, G-R1a row)

Each mutation applied alone in a detached worktree, the named packages tested, the change discarded
(`git status` clean afterwards). Run by the integrating agent, not by a separate reviewer: the plan's
"non-author" replay is still owed to the G-R1a reviewer.

| Mutation | SHA | Failing tests |
|---|---|---|
| request admission skips the fence read | `e37d1c8` | `TestAdmissionOnAFencedSessionIsRefusedAndRecorded` |
| execution admission skips the fence read | `e37d1c8` | `TestAdmissionOnAFencedSessionIsRefusedAndRecorded`, `TestRevokeSessionFencesWhatItReachesAndReports`, `TestCrossProcessFenceIsLinearizedWithAdmission`, `TestCrossProcessRevocationIsLinearizedWithAdmission` |
| post snapshot skipped (post := pre) | `e37d1c8` | `TestEpisodeScopeChangedAndIgnoredOutput` |
| configScope re-read deleted | `e37d1c8` | **none** -> gap closed by `b60ae09` (`TestCheckThatRedefinesItselfOutsideTheWorkspacePassesNothing`, which fails under the mutation) |
| cross-run reuse restored (`return inRun` -> `true`) | `e37d1c8` | `TestSatisfies`, `TestCheckVerdict`, `TestEpisodeReuseIsInRunOnly`, `TestRuntimePassIsDecidedAgainInTheNextRun`, `TestConstraintIsDecidedFreshByEveryStart` |
| purge skips `agent_steps` | `9674321` (`cog/t3-s4`) | `TestDeletingASessionRemovesEveryCopyOfItsContent` |
| binding check skipped (`matchBinding`) | `e37d1c8` | `TestConstraintIsSatisfiedOnlyThroughAnApprovedBinding` (2 subtests), `TestConstraintRevalidationIsCapped` |

## 5. Performance (same session, same machine, same harness)

`TestT1PerfBaseline` (`LCX_T1_PERF=1 LCX_T1_PERF_RUNS=50`, two runs each), the T1-S7 harness copied into a `42ee234`
worktree for the baseline:

| | `42ee234` | `b60ae09` | Budget (approval §7) |
|---|---|---|---|
| median step latency | 18.05 / 17.96 ms | 19.25 / 19.25 ms | rise ≤ 25 %: **+7 %, met** |
| step p95 | 42.5 / 44.2 ms | 67.2 / 65.6 ms | no budget; reported |
| after shell (median) | - | 62.0 ms | - |
| store tx p99 (`TestT1StoreTransactionLatency`, 500 cycles, concurrent reader) | - | AdmitRequest 1.17, AppendAnswered 1.09, AdmitExecution 2.15, CommitToolResult 2.93 ms | reported |

T1-S7 measured +105 % (32.1 vs 15.5 ms); B0+R1's one-git-process snapshot brought it to +7 %. The baseline itself moved
between days (15.5 -> 18.0 ms), so only same-session numbers are compared. The fixture has no checks and no
constraints: the cost of an episode per Decide and of constraint revalidation (≤2 checks, ≤15 s per Start) is not in
these numbers.

## 6. H-1 test-change ledger closure

| H-1 rows | Outcome | Where |
|---|---|---|
| C2-01, C2-04, C2-07, C2-08 (`NEEDS_SONNET`) | assertions kept; fixtures carry closed episodes; the runtime reuse case is named in-run, a new case shows another run's pass reruns | T4-S2 report §5 |
| C6-01, C6-04, C6-05, C6-08, C6-10, C6-11, C6-13, C6-14, C6-15 (`NEEDS_SONNET`) | assertions unchanged; runtime fixtures approve their check set before the unchanged scenario | T4-S1 report §6; pass at `b60ae09` |
| T4-S3 and the two later test commits | no existing assertion changed | T4-S3 report §5 |

## 7. G-R1a preconditions not met / OPEN

- **T0-S1 merged into `main`:** not done (`main` = `42ee234`); needs Owner authorization to merge locally.
- **O-4** (check approval as the default for every user): not approved. Until it is, a non-interactive run with
  required checks in a project without an approved set ends `failed/spec_unapproved`.
- **O-3** (privacy deletion semantics): not approved; `cog/t3-s4` stays separate.
- **Non-author gate review:** §2 and §4 were run by the integrating agent; plan §8.2 asks a reviewer to re-run them
  at the exact SHA.
- **Desktop:** no Desktop change; spec_unapproved, constraint and approval UI are T7-S1. Desktop is not validated
  against this Core revision.
- **Not proven:** cognitive benefit (`COGNITIVE_BENEFIT_PROVEN=NO`); R0 boundary unchanged (same-UID full shell).
