# Cognitive Core v1.0 — Build / Vet / Test / Race / Platforms

## Identity and evidence rules / 身份与证据规则

- Input / 输入：`8c30f75e063db69a1dbc867741dc8f042c7d0dbc`，独立 `integ/cognitive-core-v1-20261116`。
- All 893 Go/module/SQL files remain byte-identical to that input. Source-map SHA-256 / 源码清单摘要：`ff95fd3bcb8585810f121c73dd1be1450d29ad2fb35b4e53c5ef16e56964926a`。
- Raw evidence / 原始凭证：workspace `bench/evidence/cognitive-core-v1-integration-20261116/logs/`。Command TSV rows contain start, finish, seconds, exit status and command. 未运行的项不计通过，历史结果不计本轮结果。
- Test counts are terminal `go test -json` events with a `Test` field, including parent tests and subtests. Package failures and packages with no test files are separate. 同一次运行不把 package PASS 再计成测试；合并覆盖按 `(Package, Test)` 去重，不能简单相加。
- The first full-module invocations are retained even if a package timeout prevented completion. A later targeted completion is explicitly separate, not a rewritten successful full-module invocation. 第一轮失败日志原样保留，补跑不能冒充一次全量绿。

## Verification state / 验证状态

**COMPLETE / 已完成：** Build/Vet, Windows compilation, native Linux cognitive paths and the offline public demo passed. Runtime-only completion closes both initial macOS package timeouts. Distinct same-source macOS normal and Race coverage is **3005 PASS / 0 unresolved FAIL / 46 SKIP each**, across all 43 tested packages; Linux cognitive coverage is **425 PASS / 0 unresolved FAIL / 2 SKIP**. Original failed invocations remain preserved, not converted to one clean full run. No new product defect or data race was established; platform limits below still apply.

## Original invocations / 首轮命令

| Platform / 范围 | Command / 命令 | Test PASS | Test FAIL | Test SKIP | Exit / 结论 |
|---|---|---:|---:|---:|---|
| macOS arm64, full module | `CGO_ENABLED=1 go build ./...` | — | — | — | 0; 20 s |
| macOS arm64, full module | `CGO_ENABLED=1 go vet ./...` | — | — | — | 0; 3 s |
| macOS arm64, full module | `go test -json -count=1 ./...` | 2411 | 0 | 37 | 1; runtime package hit default 10-minute timeout; 633 s command |
| macOS arm64, full module | `go test -json -race -count=1 ./...` | 2359 | 0 | 28 | 1; runtime package hit default 10-minute timeout; 653 s command |
| Linux arm64, full module | `go build ./...` | — | — | — | 0; 64 s |
| Linux arm64, full module | `go vet ./...` | — | — | — | 0; 13 s |
| Linux arm64, selected cognitive paths | `go test -json -race -count=1 -p 4 -timeout=20m` on five packages with the recorded name selector | 419 | 6 | 2 | 1; two curl-dependent families failed; 581 s command |
| Windows amd64, cross-build | `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...` | — | — | — | 0; 5 s |
| Windows amd64, cross-vet | same environment, `go vet ./...` | — | — | — | 0; 3 s |
| Windows amd64, compile tests only | same environment, `go test -c -o <owned-temp-dir>/ ./...` | — | — | — | 0; 14 s; no test execution |

The macOS full-module invocations each completed 42 non-runtime packages successfully and reported six no-test-file packages; runtime did not finish. No data race was reported in the original race log, but an incomplete run alone cannot certify the remaining tests. 首轮 macOS 没有失败测试终态，仍有真实 package 超时失败，二者不能混淆。

Linux packages: `./internal/knowledge ./internal/cognition ./internal/session ./internal/migrate ./runtime`. Selector: `Cognit|Experience|Assess|Governor|SourceRun|PreAdmission|Unrelated.*(Experience|Consumer)|Canonical|Fence|Privacy|Deletion`. This is not a full Linux test suite. Linux 六个 FAIL 事件包括一个父测试及其四个子测试，加另一个测试；不是六个独立缺陷。

## Diagnosed recovery / 已定位的补验原因

### macOS package deadline

`macos-test.log` records `panic: test timed out after 10m0s` while `TestDeliveryQueuedHostResolutionWinsOverAutoDone` had been running for only 2 s. The Race log reaches the same package deadline while `TestCrossProcessBusyIsAStorageFailure` has run 40 s. This is the package's total deadline, not a new 10-minute cognitive operation hang. 保留两份超时堆栈，不改断言、不删测试、不缩减已有运行路径。

The incomplete `runtime` package was completed once with `-timeout=30m`, ordinary and Race, in `macos-runtime-test-recovery.log` and `macos-runtime-race-recovery.log`. Both exited 0 with **908 PASS / 0 FAIL / 21 SKIP**; package durations were 841.430 s / 1140.299 s, command durations 845 s / 1143 s. The 42 already-passing packages plus completed runtime cover **3005 PASS / 0 unresolved FAIL / 46 SKIP** per mode, with identical test-name sets and no data race report. No previously completed runtime test is missing from the recovery. This is complete same-source coverage, not a single full-module invocation that exited 0. 仅改变包级期限，未改测试断言或 Runtime 执行约束；完整计数及跳过理由见 `FINAL_VERIFICATION.json`、`FINAL_SKIP_REASONS.json`。

### Linux HTTP gate prerequisite

`TestCognitionCloseWaitsForManualVerification` and all four modes of `TestCognitionWorkUnitFailureEpisodeInterleaving` execute `curl --noproxy '*' -fsS <loopback-test-server>` to hold a real check episode open. `command -v curl` returned no executable. One maintenance pass therefore ended before its check reached the server; interleaving cases timed out waiting for a request no process could send.

The matching Ubuntu package `curl_8.5.0-2ubuntu10.16_arm64.deb` was downloaded and unpacked **only** beneath the task's unique Linux verification directory; SHA-256 `b3f66040e895d5a2bb9eb3fca5d98766997348080e445c209a29b1819a9a7d60`. It uses the existing matching libcurl; no shared package was installed or updated. `linux-curl-prerequisite.log` retains the apt cleanup-hook permission warning. 未改共享容器包、Docker 配置、测试、sandbox 或 Fence。

The first targeted invocation mistakenly used the user's default, empty `/home/lc/go/pkg/mod`; with `GOPROXY=off` it failed before any test ran. `docker inspect` identified the existing read-only cache at `/gomod`. `linux-curl-gate-recovery.log` preserves this setup failure; the corrected invocation uses `/gomod` and only the same two families. 这次环境参数错误不计测试 FAIL，也不记为产品缺陷。

The corrected-cache recovery exited 0 in 65 s: **6 PASS / 0 FAIL / 0 SKIP**, covering both top-level tests and all four interleaving modes. Across the original and recovered same-source paths, the unique Linux result is **425 PASS / 0 unresolved FAIL / 2 SKIP**. The original 6 failure events and the setup failure remain preserved; this is not one wholly green invocation. Linux 因缺 curl 造成的验证阻断已解除，没有生产代码或断言修复。


## Real public API scenario / 真实公开调用场景

`CGO_ENABLED=1 go run ./cmd/lcx-experience-demo` exited 0 in 8 s (`macos-offline-demo.json`). It used an in-process deterministic `demo/offline` provider, real Engine/Agent Loop/tool dispatch, temporary Git/config/SQLite and actual host-approved check processes. No HTTP model client or paid request was used.

Observed / 实测：

- Origin run wrote the release marker and produced a qualified outcome check; extraction created a candidate, then validation produced a conditional validated Experience.
- Explicit recall and host adoption returned the gated exact procedure. The origin output was removed, the later consumer's ordinary run created a fresh `status=ready` output, and feedback recorded one real success.
- Closing/reopening Engine preserved the same node version, one Use and the feedback. Recall remained available.
- Removing the condition marker caused a later adoption to refuse; version and Use count stayed unchanged.
- The owned scratch root was removed (`cleanup.removed=true`). `benefitMeasured=false`: this proves conditional reuse mechanics, not causal benefit or general model task transfer.

## Required behavior mapping / 必须行为的实测映射

The following paths passed in the original native Linux Race gate, even though its two curl-dependent families failed. `ACCEPTANCE_PATHS_INITIAL.json` lists exact PASS events; no row below promotes that whole gate to PASS.

| Boundary / 边界 | Representative actually passed tests / 实际通过的代表用例 |
|---|---|
| Real SQLite, migration, rollback | `TestCognitiveGraphMigrationRoundTrip`, `TestCognitiveCompletionMigrationRoundTripAndFailure`, `TestCognitiveCompletionDowngradeRefusesNewSemantics`, `TestCanonicalSequencePreservesHubAndRecoversBackup` |
| Privacy Delete, foreign decision content | `TestDeleteSessionPurgesForeignCognitiveDecisionsCitingDeletedSource`, `TestDeleteExperienceOwnerPurgesForeignCognitiveDecisions`, `TestExperiencePrivacyDeletionErasesHistory` |
| Source revocation, disclosure fences | `TestRevokeSourceFencesReadersAndCancelsTheirRuns`, `TestCognitionLocatorPrivacyRefusesRevokedStartNotice`, `TestExperienceCounterexampleWarningRetainsWithdrawal` |
| Consumer isolation | `TestExperienceFeedbackDetailsArePrivateToConsumer`, `TestExperienceConsumerWithdrawalDoesNotFenceCoConsumers`, `TestUnrelatedConsumerRevocationDoesNotFenceExperienceDependent` |
| Version/CAS and shared use | `TestCognitiveGraphCASAttributionAndStaleSourceRevision`, `TestExperienceScopeIsolationAndConcurrentAdoption`, `TestExperienceSettlesEveryConcurrentConsumerOfAFullRecord`, `TestUnrelatedExperienceUseKeepsDependentCurrent` |
| Actual application, no false success | `TestCompletionFeedbackRequiresActualExperienceApplication`, `TestExperienceApplicationSettlementRequiresOrderedExactExecutions`, `TestExperienceFeedbackWithoutExecutionIsNotSuccess`, `TestExperienceFeedbackDoesNotReusePreviousRunExecution` |
| Negative evidence / unknown / conflict | `TestCompletionObservedFailedActionSupportsNegativeExperience`, `TestCompletionAssessmentCannotPromoteUnknownOrHideConflicts`, `TestCompletionAssessmentIncludesConflictsTheModelDidNotSelect` |
| Restart, interrupted cleanup | `TestCognitiveGraphUpgradeReopenAndRestart`, `TestExperienceRecoveredSettlementKeepsTheOutcomeOfTheApplication`, `TestCognitionPrivacySIGKILLRecovery`, `TestKilledDeletionIsFinishedAtOpen` |
| Native Governor and retention | `TestGovernorHostLifecyclePathIsReachableOnSQLite`, `TestGovernorSnapshotProjectsCanonicalAndExcludesConsolidatedAliasesFromBacklog`, `TestGovernorRefusesAliasChainsAndInactiveCanonical`, `TestGovernorKeepsNotYetValidKnowledgeOutOfArchive` |
| Run identity before admission | `TestPreAdmissionRunFailureDoesNotSplicePreviousRunTraceOrFinalizeStaleProposal`, `TestPreAdmissionRunFailureSettlesConsumerAdoptionsWithoutFalseAttributionOrRecoveryLoop` |

## Platform limits / 平台边界

- **macOS:** native darwin/arm64, Go 1.26.5, CGO=1. Real-model, live IDE/DAP adapter and optional capability gates were unset. Existing helper/platform skips stay explicit; no new Skip was added.
- **Linux:** native linux/arm64 Go 1.27.1 processes in Ubuntu 24.04 userland inside the existing privileged Docker Desktop/LinuxKit container, non-root UID 1000, running systemd user manager and delegated user cgroup. This is not an Ubuntu VM or physical host. Source was copied to a unique writable directory, not edited in shared `/src`; module cache is read-only `/gomod`. Scoped cognitive Race is not full Linux UI/debugger/resource certification.
- **Windows:** cross-build and test-binary compilation only. CGO=0 selects the SQLite stub; no claim about Windows SQLite, shell/sandbox, runtime execution or native UI.
- **Unmeasured:** real-model candidate usefulness, sustained learning benefit, causal improvement, large-domain performance, product release and production migration. Existing independent code-review acceptance is separate from these self-run integration checks.

Owned platform cleanup completed: the unique Linux source/tool directory and Windows cross-test binary directory were removed. The shared Linux container and all historical worktrees remain. `logs/platform-cleanup.log` records the exact targets and success. Both macOS completion commands have exited successfully; no task-started verification job remains running. No historical evidence was removed.

[Final report](FINAL_COGNITIVE_CORE_V1_REPORT.md) · [完整中文结论](FINAL_COGNITIVE_CORE_V1_REPORT.zh-CN.md) · [Migration](COGNITIVE_CORE_V1_MIGRATION.md) · [Git/cleanup](COGNITIVE_CORE_V1_GIT.md).
